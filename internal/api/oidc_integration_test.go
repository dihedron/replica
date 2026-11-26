//go:build integration

package api

import (
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dihedron/veho/internal/audit"
	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// flaky is an issuer whose discovery and keys can be taken down.
type flaky struct {
	*httptest.Server
	signer
	ca                string
	discovery, keysUp atomic.Bool
}

func newFlaky(t *testing.T) *flaky {
	t.Helper()
	f := &flaky{signer: newSigner("flaky-1")}
	f.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/.well-known/openid-configuration" && f.discovery.Load():
			_ = json.NewEncoder(w).Encode(map[string]any{"issuer": f.URL, "jwks_uri": f.URL + "/jwks", "id_token_signing_alg_values_supported": []string{"ES256", "HS256", "none"}})
		case r.URL.Path == "/jwks" && f.keysUp.Load():
			_, _ = w.Write(f.jwks())
		default:
			http.Error(w, "down", http.StatusServiceUnavailable)
		}
	}))
	t.Cleanup(f.Close)
	f.ca = filepath.Join(t.TempDir(), "flaky.pem")
	if err := os.WriteFile(f.ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *flaky) token(t *testing.T, sub string) string {
	return f.sign(t, map[string]any{"iss": f.URL, "sub": sub, "aud": audience, "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix()})
}

// AU-T6: an issuer that is down stops neither the node nor the other
// credentials, and works once it answers (G-7).
func TestIssuerDown(t *testing.T) {
	down := newFlaky(t)
	f := setupIssuers(t, Options{}, oidcConf(down.URL, down.ca))
	f.entitle(t, "oidc", down.URL, "dora", "", &f.a, RoleViewer)
	f.entitle(t, "oidc", f.iss, "alice", "", &f.a, RoleViewer)
	if r := f.rest(t, "GET", "/v1/jobs", f.keys["a-viewer"], nil); r.code != http.StatusOK {
		t.Errorf("an API key: %d", r.code)
	}
	if r := f.rest(t, "GET", "/v1/jobs", f.token(t, "alice", nil), nil); r.code != http.StatusOK {
		t.Errorf("another issuer: %d", r.code)
	}
	unavailable := func(when string) {
		t.Helper()
		if r := f.rest(t, "GET", "/v1/jobs", down.token(t, "dora"), nil); r.code != http.StatusServiceUnavailable || r.body["code"] != "unavailable" {
			t.Errorf("%s: %d %v", when, r.code, r.body)
		}
	}
	unavailable("no discovery")
	down.discovery.Store(true)
	time.Sleep(3 * time.Second) // the retry's first back-offs
	unavailable("no keys")
	down.keysUp.Store(true)
	if r := f.rest(t, "GET", "/v1/jobs", down.token(t, "dora"), nil); r.code != http.StatusOK {
		t.Errorf("the issuer is back: %d %v", r.code, r.body)
	}
}

// AU-T7: the tokens refused besides API-T9's (G-8).
func TestTokenLifetime(t *testing.T) {
	up := newFlaky(t)
	up.discovery.Store(true)
	up.keysUp.Store(true)
	f := setupIssuers(t, Options{}, oidcConf(up.URL, up.ca))
	f.entitle(t, "oidc", up.URL, "dora", "", &f.a, RoleViewer)
	now := time.Now()
	claims := func(iat, exp time.Time) map[string]any {
		return map[string]any{"iss": up.URL, "sub": "dora", "aud": audience, "iat": iat.Unix(), "exp": exp.Unix()}
	}
	noIAT := claims(now, now.Add(time.Hour))
	delete(noIAT, "iat")
	for name, c := range map[string]struct {
		claims map[string]any
		want   int
	}{
		"an hour":                 {claims(now, now.Add(time.Hour)), http.StatusOK},
		"over the lifetime":       {claims(now, now.Add(2*time.Hour)), http.StatusUnauthorized},
		"no iat":                  {noIAT, http.StatusUnauthorized},
		"from the future":         {claims(now.Add(10*time.Minute), now.Add(time.Hour)), http.StatusUnauthorized},
		"expired within the skew": {claims(now.Add(-time.Hour), now.Add(-20*time.Second)), http.StatusOK},
		"expired":                 {claims(now.Add(-time.Hour), now.Add(-2*time.Minute)), http.StatusUnauthorized},
	} {
		if r := f.rest(t, "GET", "/v1/jobs", up.sign(t, c.claims), nil); r.code != c.want {
			t.Errorf("%s: %d %v, want %d", name, r.code, r.body, c.want)
		}
	}
	// the issuer announces HS256 and none: neither is accepted
	pub, _ := json.Marshal(up.key.X.Bytes())
	hs, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.HS256, Key: pub}, (&jose.SignerOptions{}).WithHeader("kid", up.kid))
	if err != nil {
		t.Fatal(err)
	}
	mac, err := jwt.Signed(hs).Claims(claims(now, now.Add(time.Hour))).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(claims(now, now.Add(time.Hour)))
	none := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`)) + "." + base64.RawURLEncoding.EncodeToString(payload) + "."
	for name, tok := range map[string]string{"HMAC": mac, "none": none} {
		if r := f.rest(t, "GET", "/v1/jobs", tok, nil); r.code != http.StatusUnauthorized {
			t.Errorf("%s: %d", name, r.code)
		}
	}
}

// AU-T16, G-9: refused credentials are audited, summed, without the
// credential or an unverified name.
func TestRefusalsAudited(t *testing.T) {
	defer func(d time.Duration) { refusalsEvery = d }(refusalsEvery)
	refusalsEvery = 100 * time.Millisecond
	rec := &audit.Recorder{}
	f := setupIdentities(t, Options{Audit: rec})
	for range 3 {
		f.rest(t, "GET", "/v1/jobs", "veho_wrongwrongwr_CANARY", nil)
	}
	f.rest(t, "GET", "/v1/jobs", f.token(t, "mallory", nil), nil)
	f.rest(t, "GET", "/v1/jobs", f.token(t, "CANARY", map[string]any{"aud": "other"}), nil)
	f.rest(t, "GET", "/v1/jobs", "", nil)
	type key struct{ kind, principal string }
	want := map[key]int{{"api_key", ""}: 3, {"jwt", "oidc:" + f.iss + "#mallory"}: 1, {"jwt", ""}: 1, {"none", ""}: 1}
	got := map[key]int{}
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		got = map[key]int{}
		for _, e := range rec.Events() {
			raw, _ := json.Marshal(e)
			if e.Action != "AUTH_REFUSED" || e.Outcome != audit.Denied || strings.Contains(string(raw), "CANARY") {
				t.Fatalf("event: %s", raw)
			}
			d := e.Details.(refusalDetails)
			if d.Address != "127.0.0.1" {
				t.Errorf("address: %s", raw)
			}
			got[key{d.Kind, e.Principal}] += d.Count
		}
		if len(got) == len(want) {
			break
		}
	}
	for k, n := range want {
		if got[k] != n {
			t.Errorf("%v: %d refusals audited, want %d (%v)", k, got[k], n, got)
		}
	}
}
