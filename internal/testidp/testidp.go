// Package testidp is an OIDC identity provider for tests only: discovery,
// keys, the authorization code flow with PKCE, the device flow (RFC 8628),
// refresh tokens that rotate, and revocation (RFC 7009). Every login is
// approved at once, as Subject; a login that asks for acr_values gets the
// first of them as its acr. It never runs outside tests.
package testidp

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// IdP is a running test identity provider.
type IdP struct {
	URL    string
	CAFile string
	// Audience is the aud of its access tokens.
	Audience string
	// ClientID is the public client it accepts.
	ClientID string

	key *ecdsa.PrivateKey
	t   testing.TB

	mu sync.Mutex
	// Subject is who every login authenticates; ACR its acr unless the login
	// asked for acr_values.
	Subject string
	ACR     string
	// AccessTTL is the lifetime of its access tokens.
	AccessTTL time.Duration
	codes     map[string]grant
	devices   map[string]*grant
	refresh   map[string]grant // the refresh tokens that are valid
	used      map[string]bool  // refresh tokens already rotated
	// Refreshes counts the refreshes it granted, Reused the refresh tokens
	// presented again after their rotation, Revoked the tokens revoked.
	Refreshes, Reused int
	Revoked           []string
	// Logins counts the code and device logins, with the acr of the last.
	Logins  int
	LastACR string
}

type grant struct {
	challenge, redirect, acr string
	polls                    int
}

// New starts an IdP over TLS; its CA is in CAFile.
func New(t testing.TB, audience, clientID string) *IdP {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	p := &IdP{Audience: audience, ClientID: clientID, key: k, t: t, Subject: "alice", ACR: "1", AccessTTL: time.Hour,
		codes: map[string]grant{}, devices: map[string]*grant{}, refresh: map[string]grant{}, used: map[string]bool{}}
	srv := httptest.NewTLSServer(http.HandlerFunc(p.serve))
	t.Cleanup(srv.Close)
	p.URL = srv.URL
	p.CAFile = filepath.Join(t.TempDir(), "idp.pem")
	if err := os.WriteFile(p.CAFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// Sign signs claims as an access token of the IdP; iss, aud, iat and exp
// are filled in unless given.
func (p *IdP) Sign(claims map[string]any) string {
	p.t.Helper()
	p.mu.Lock()
	ttl := p.AccessTTL
	p.mu.Unlock()
	return p.sign(ttl, claims)
}

// sign reads only what never changes, so that it runs under p.mu too.
func (p *IdP) sign(ttl time.Duration, claims map[string]any) string {
	now := time.Now()
	all := map[string]any{"iss": p.URL, "aud": p.Audience, "iat": now.Unix(), "exp": now.Add(ttl).Unix()}
	for k, v := range claims {
		all[k] = v
	}
	sg, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: jose.JSONWebKey{Key: p.key, KeyID: "idp-1"}}, (&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		p.t.Fatal(err)
	}
	tok, err := jwt.Signed(sg).Claims(all).Serialize()
	if err != nil {
		p.t.Fatal(err)
	}
	return tok
}

func random() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func (p *IdP) serve(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/.well-known/openid-configuration":
		writeJSON(w, http.StatusOK, map[string]any{"issuer": p.URL, "jwks_uri": p.URL + "/jwks",
			"authorization_endpoint": p.URL + "/authorize", "token_endpoint": p.URL + "/token",
			"device_authorization_endpoint": p.URL + "/device", "revocation_endpoint": p.URL + "/revoke",
			"id_token_signing_alg_values_supported": []string{"ES256"}, "code_challenge_methods_supported": []string{"S256"}})
	case "/jwks":
		writeJSON(w, http.StatusOK, jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &p.key.PublicKey, KeyID: "idp-1", Algorithm: "ES256", Use: "sig"}}})
	case "/authorize":
		p.authorize(w, r)
	case "/device":
		p.device(w, r)
	case "/token":
		p.token(w, r)
	case "/revoke":
		_ = r.ParseForm()
		p.mu.Lock()
		p.Revoked = append(p.Revoked, r.PostForm.Get("token"))
		delete(p.refresh, r.PostForm.Get("token"))
		p.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	default:
		http.NotFound(w, r)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func oauthError(w http.ResponseWriter, code string) {
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": code})
}

// acrOf is the acr a login gets: the first of acr_values, else the IdP's.
func (p *IdP) acrOf(q url.Values) string {
	if v := strings.Fields(q.Get("acr_values")); len(v) > 0 {
		return v[0]
	}
	return p.ACR
}

// authorize approves at once and redirects to the loopback redirect URI.
func (p *IdP) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	redirect, err := url.Parse(q.Get("redirect_uri"))
	if q.Get("client_id") != p.ClientID || err != nil || redirect.Hostname() != "127.0.0.1" || q.Get("response_type") != "code" ||
		q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || q.Get("state") == "" {
		http.Error(w, "bad authorization request", http.StatusBadRequest)
		return
	}
	code := random()
	p.mu.Lock()
	p.codes[code] = grant{challenge: q.Get("code_challenge"), redirect: q.Get("redirect_uri"), acr: p.acrOf(q)}
	p.mu.Unlock()
	v := redirect.Query()
	v.Set("code", code)
	v.Set("state", q.Get("state"))
	redirect.RawQuery = v.Encode()
	http.Redirect(w, r, redirect.String(), http.StatusFound) //nolint:gosec // to the loopback only, checked above
}

// device starts a device login, approved after its first poll.
func (p *IdP) device(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	if r.PostForm.Get("client_id") != p.ClientID {
		oauthError(w, "invalid_client")
		return
	}
	dc := random()
	p.mu.Lock()
	p.devices[dc] = &grant{acr: p.acrOf(r.PostForm)}
	p.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"device_code": dc, "user_code": "ABCD-EFGH", "verification_uri": p.URL + "/activate",
		"expires_in": 300, "interval": 1})
}

func (p *IdP) token(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	f := r.PostForm
	if f.Get("client_id") != p.ClientID {
		oauthError(w, "invalid_client")
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	var g grant
	switch f.Get("grant_type") {
	case "authorization_code":
		c, ok := p.codes[f.Get("code")]
		delete(p.codes, f.Get("code"))
		sum := sha256.Sum256([]byte(f.Get("code_verifier")))
		if !ok || c.redirect != f.Get("redirect_uri") || base64.RawURLEncoding.EncodeToString(sum[:]) != c.challenge {
			oauthError(w, "invalid_grant")
			return
		}
		g = c
		p.Logins++
	case "urn:ietf:params:oauth:grant-type:device_code":
		d, ok := p.devices[f.Get("device_code")]
		if !ok {
			oauthError(w, "invalid_grant")
			return
		}
		if d.polls++; d.polls < 2 {
			oauthError(w, "authorization_pending")
			return
		}
		delete(p.devices, f.Get("device_code"))
		g = *d
		p.Logins++
	case "refresh_token":
		rt := f.Get("refresh_token")
		old, ok := p.refresh[rt]
		if !ok {
			if p.used[rt] {
				p.Reused++
			}
			oauthError(w, "invalid_grant")
			return
		}
		delete(p.refresh, rt)
		p.used[rt] = true
		g = old
		p.Refreshes++
	default:
		oauthError(w, "unsupported_grant_type")
		return
	}
	p.LastACR = g.acr
	rt := random()
	p.refresh[rt] = grant{acr: g.acr}
	access := p.sign(p.AccessTTL, map[string]any{"sub": p.Subject, "acr": g.acr, "auth_time": time.Now().Unix()})
	writeJSON(w, http.StatusOK, map[string]any{"access_token": access, "token_type": "Bearer", "refresh_token": rt,
		"expires_in": int(p.AccessTTL.Seconds())})
}

// Counts returns the refreshes granted and the rotated refresh tokens that
// were presented again.
func (p *IdP) Counts() (refreshes, reused, logins int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.Refreshes, p.Reused, p.Logins
}

// Set changes the IdP's subject, acr and access token lifetime.
func (p *IdP) Set(subject, acr string, ttl time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Subject, p.ACR, p.AccessTTL = subject, acr, ttl
}
