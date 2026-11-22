//go:build integration

package api

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
	"uuid"

	vehov1 "github.com/dihedron/veho/api/veho/v1"
	"github.com/dihedron/veho/internal/config"
	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const (
	audience = "veho-api"
	td       = "veho.test"
)

// signer signs JWTs with an EC key named kid.
type signer struct {
	key *ecdsa.PrivateKey
	kid string
}

func newSigner(kid string) signer {
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	return signer{key: k, kid: kid}
}

func (s signer) jwks() []byte {
	b, _ := json.Marshal(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &s.key.PublicKey, KeyID: s.kid, Algorithm: "ES256", Use: "jwt-svid"}}})
	return b
}

func (s signer) sign(t *testing.T, claims map[string]any) string {
	t.Helper()
	sg, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: jose.JSONWebKey{Key: s.key, KeyID: s.kid}}, (&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		t.Fatal(err)
	}
	tok, err := jwt.Signed(sg).Claims(claims).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// issuer is a test OIDC issuer: discovery and JWKS over TLS.
func issuer(t *testing.T, s signer) (*httptest.Server, string) {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]any{"issuer": srv.URL, "jwks_uri": srv.URL + "/jwks",
				"id_token_signing_alg_values_supported": []string{"ES256"}})
		case "/jwks":
			_, _ = w.Write(s.jwks())
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	ca := filepath.Join(t.TempDir(), "issuer.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	return srv, ca
}

// trustDomain is a CA of a SPIFFE trust domain.
type trustDomain struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func newTrustDomain(t *testing.T, name string) trustDomain {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	u, _ := url.Parse("spiffe://" + name)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name}, URIs: []*url.URL{u}, IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	return trustDomain{cert: c, key: k}
}

// svid issues an X.509-SVID for id.
func (d trustDomain) svid(t *testing.T, id string) *tls.Certificate {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	u, _ := url.Parse(id)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), URIs: []*url.URL{u}, KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, d.cert, &k.PublicKey, d.key)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: k}
}

type idFixture struct {
	*fixture
	oidc    signer
	iss     string
	svidKey signer
	domain  trustDomain
}

func (f *idFixture) entitle(t *testing.T, kind, issuer, subject, group string, tenant *uuid.UUID, role string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := f.migrator.QueryRow(context.Background(), `insert into veho.entitlements (tenant_id, kind, issuer, subject, "group", role)
		values ($1, $2, $3, nullif($4, ''), nullif($5, ''), $6) returning id`, tenant, kind, issuer, subject, group, role).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func setupIdentities(t *testing.T, o Options) *idFixture {
	t.Helper()
	f := &idFixture{oidc: newSigner("oidc-1"), svidKey: newSigner("svid-1"), domain: newTrustDomain(t, td)}
	srv, ca := issuer(t, f.oidc)
	f.iss = srv.URL
	dir := t.TempDir()
	x509b, jwtb := filepath.Join(dir, "x509.pem"), filepath.Join(dir, "jwt.json")
	if err := os.WriteFile(x509b, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.domain.cert.Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jwtb, f.svidKey.jwks(), 0o600); err != nil {
		t.Fatal(err)
	}
	f.fixture = setupWith(t, o, func(*fixture) *Identities {
		ids, err := NewIdentities(context.Background(), config.API{
			OIDC:   []config.OIDC{{Issuer: srv.URL, Audience: audience, GroupsClaim: "groups", CAFile: ca}},
			SPIFFE: config.SPIFFE{TrustDomain: td, X509BundleFile: x509b, JWTBundleFile: jwtb, Audience: audience},
		})
		if err != nil {
			t.Fatal(err)
		}
		return ids
	})
	return f
}

func (f *idFixture) token(t *testing.T, sub string, extra map[string]any) string {
	claims := map[string]any{"iss": f.iss, "sub": sub, "aud": audience, "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix()}
	for k, v := range extra {
		claims[k] = v
	}
	return f.oidc.sign(t, claims)
}

// API-T9: OIDC.
func TestOIDC(t *testing.T) {
	f := setupIdentities(t, Options{})
	f.entitle(t, "oidc", f.iss, "alice", "", &f.a, RoleOperator)
	f.entitle(t, "oidc", f.iss, "", "readers", &f.a, RoleViewer)
	f.entitle(t, "oidc", f.iss, "carol", "", &f.a, RoleViewer)
	f.entitle(t, "oidc", f.iss, "carol", "", &f.b, RoleViewer)
	if r := f.rest(t, "POST", "/v1/jobs", f.token(t, "alice", nil), job("out/oidc")); r.code != http.StatusAccepted {
		t.Errorf("alice creates: %d %v", r.code, r.body)
	}
	bob := f.token(t, "bob", map[string]any{"groups": []string{"readers"}})
	if r := f.rest(t, "GET", "/v1/jobs", bob, nil); r.code != http.StatusOK || len(r.body["jobs"].([]any)) != 1 {
		t.Errorf("bob, a reader, lists: %d %v", r.code, r.body)
	}
	if r := f.rest(t, "POST", "/v1/jobs", bob, job("out/bob")); r.code != http.StatusForbidden {
		t.Errorf("bob, a reader, creates: %d", r.code)
	}
	other := newSigner("oidc-1") // the same key ID, another key
	for name, tok := range map[string]string{
		"wrong audience":  f.token(t, "alice", map[string]any{"aud": "other"}),
		"another issuer":  f.token(t, "alice", map[string]any{"iss": "https://elsewhere.example"}),
		"expired":         f.token(t, "alice", map[string]any{"exp": time.Now().Add(-time.Minute).Unix()}),
		"bad signature":   other.sign(t, map[string]any{"iss": f.iss, "sub": "alice", "aud": audience, "exp": time.Now().Add(time.Hour).Unix()}),
		"unbound subject": f.token(t, "mallory", nil),
		"two tenants":     f.token(t, "carol", nil),
	} {
		if r := f.rest(t, "GET", "/v1/jobs", tok, nil); r.code != http.StatusUnauthorized {
			t.Errorf("%s: %d %v", name, r.code, r.body)
		}
	}
}

// API-T10: X.509-SVIDs over gRPC and REST; the internal header is not
// believed from outside.
func TestX509SVID(t *testing.T) {
	f := setupIdentities(t, Options{})
	ingest := "spiffe://veho.test/ns/ingest-" + uuid.New().String()[:8] // the lab's database keeps older runs' rows
	f.entitle(t, "spiffe", td, ingest, "", &f.a, RoleOperator)
	f.client = f.domain.svid(t, ingest)
	if r := f.rest(t, "POST", "/v1/jobs", "", job("out/svid")); r.code != http.StatusAccepted {
		t.Errorf("REST with an SVID: %d %v", r.code, r.body)
	}
	c := f.grpc(t)
	if l, err := c.ListJobs(context.Background(), &vehov1.ListJobsRequest{}); err != nil || len(l.GetJobs()) != 1 {
		t.Errorf("gRPC with an SVID: %v, %v", l, err)
	}
	f.client = newTrustDomain(t, "other.test").svid(t, "spiffe://other.test/ns/ingest")
	if r := f.rest(t, "GET", "/v1/jobs", "", nil); r.code != http.StatusUnauthorized {
		t.Errorf("another trust domain: %d", r.code)
	}
	f.client = newTrustDomain(t, td).svid(t, ingest) // the right name, another CA
	if r := f.rest(t, "GET", "/v1/jobs", "", nil); r.code != http.StatusUnauthorized {
		t.Errorf("another CA: %d", r.code)
	}
	f.client = nil
	good := base64.StdEncoding.EncodeToString(f.domain.svid(t, ingest).Certificate[0])
	if r := f.rest(t, "GET", "/v1/jobs", "", nil, "X-Veho-Peer-Chain", good, "Grpc-Metadata-X-Veho-Peer-Chain", good); r.code != http.StatusUnauthorized {
		t.Errorf("REST with the internal header: %d", r.code)
	}
	ctx := metadata.AppendToOutgoingContext(context.Background(), peerChainMD, good)
	if _, err := f.grpc(t).ListJobs(ctx, &vehov1.ListJobsRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Errorf("gRPC with the internal metadata: %v", err)
	}
}

// API-T11: JWT-SVIDs.
func TestJWTSVID(t *testing.T) {
	f := setupIdentities(t, Options{})
	batch := "spiffe://veho.test/ns/batch-" + uuid.New().String()[:8]
	f.entitle(t, "spiffe", td, batch, "", &f.a, RoleViewer)
	sign := func(claims map[string]any) string {
		c := map[string]any{"sub": batch, "aud": audience, "exp": time.Now().Add(time.Hour).Unix()}
		for k, v := range claims {
			c[k] = v
		}
		return f.svidKey.sign(t, c)
	}
	if r := f.rest(t, "GET", "/v1/jobs", sign(nil), nil); r.code != http.StatusOK {
		t.Errorf("JWT-SVID: %d %v", r.code, r.body)
	}
	for name, tok := range map[string]string{
		"another audience":     sign(map[string]any{"aud": "other"}),
		"another trust domain": sign(map[string]any{"sub": "spiffe://other.test/ns/" + batch[len("spiffe://veho.test/ns/"):]}),
		"expired":              sign(map[string]any{"exp": time.Now().Add(-time.Minute).Unix()}),
	} {
		if r := f.rest(t, "GET", "/v1/jobs", tok, nil); r.code != http.StatusUnauthorized {
			t.Errorf("%s: %d", name, r.code)
		}
	}
}

// API-T12: a watch ends when its token expires, or its entitlement goes.
func TestIdentityRevocation(t *testing.T) {
	f := setupIdentities(t, Options{Reauthorise: 300 * time.Millisecond})
	f.entitle(t, "oidc", f.iss, "alice", "", &f.a, RoleOperator)
	entitlement := f.entitle(t, "oidc", f.iss, "dave", "", &f.a, RoleViewer)
	id := f.running(t, f.a)
	watch := func(tok string) error {
		w, err := f.grpc(t).WatchJob(as(tok), &vehov1.JobRef{Id: id})
		if err != nil {
			return err
		}
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
			if _, err := w.Recv(); err != nil {
				return err
			}
		}
		return nil
	}
	short := f.token(t, "alice", map[string]any{"exp": time.Now().Add(2 * time.Second).Unix()})
	if err := watch(short); status.Code(err) != codes.Unauthenticated {
		t.Errorf("an expiring token: %v", err)
	}
	dave := f.token(t, "dave", nil)
	go func() {
		time.Sleep(time.Second)
		_, _ = f.migrator.Exec(context.Background(), "delete from veho.entitlements where id = $1", entitlement)
	}()
	if err := watch(dave); status.Code(err) != codes.Unauthenticated {
		t.Errorf("an entitlement deleted: %v", err)
	}
}
