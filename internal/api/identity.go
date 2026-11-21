package api

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
	"uuid"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/dihedron/veho/internal/config"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spiffe/go-spiffe/v2/bundle/jwtbundle"
	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/svid/jwtsvid"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
)

// identity is a verified OIDC or SPIFFE identity, before its entitlements.
type identity struct {
	kind    string // oidc, spiffe
	issuer  string // the OIDC issuer, or the SPIFFE trust domain
	subject string
	groups  []string
}

type oidcIssuer struct {
	conf     config.OIDC
	verifier *oidc.IDTokenVerifier
	client   *http.Client
}

// Identities verifies the OIDC and SPIFFE credentials of §4.4.
type Identities struct {
	oidc     map[string]*oidcIssuer // by issuer
	td       spiffeid.TrustDomain
	x509     *reloading[*x509bundle.Bundle]
	jwt      *reloading[*jwtbundle.Bundle]
	audience string
}

// NewIdentities fetches the OIDC issuers' discovery documents and reads the
// SPIFFE bundles; it fails when one cannot be had (fail fast).
func NewIdentities(ctx context.Context, c config.API) (*Identities, error) {
	ids := &Identities{oidc: map[string]*oidcIssuer{}, audience: c.SPIFFE.Audience}
	for _, o := range c.OIDC {
		client := http.DefaultClient
		if o.CAFile != "" {
			pem, err := os.ReadFile(o.CAFile)
			if err != nil {
				return nil, fmt.Errorf("api: oidc %s: %w", o.Issuer, err)
			}
			roots := x509.NewCertPool()
			if !roots.AppendCertsFromPEM(pem) {
				return nil, fmt.Errorf("api: oidc %s: no certificate in %s", o.Issuer, o.CAFile)
			}
			client = &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}}
		}
		p, err := oidc.NewProvider(oidc.ClientContext(ctx, client), o.Issuer)
		if err != nil {
			return nil, fmt.Errorf("api: oidc %s: %w", o.Issuer, err)
		}
		ids.oidc[o.Issuer] = &oidcIssuer{conf: o, verifier: p.Verifier(&oidc.Config{ClientID: o.Audience}), client: client}
	}
	if c.SPIFFE.TrustDomain == "" {
		return ids, nil
	}
	td, err := spiffeid.TrustDomainFromString(c.SPIFFE.TrustDomain)
	if err != nil {
		return nil, fmt.Errorf("api: spiffe: %w", err)
	}
	ids.td = td
	if f := c.SPIFFE.X509BundleFile; f != "" {
		if ids.x509, err = newReloading(f, func(p string) (*x509bundle.Bundle, error) { return x509bundle.Load(td, p) }); err != nil {
			return nil, fmt.Errorf("api: spiffe: %w", err)
		}
	}
	if f := c.SPIFFE.JWTBundleFile; f != "" {
		if ids.jwt, err = newReloading(f, func(p string) (*jwtbundle.Bundle, error) { return jwtbundle.Load(td, p) }); err != nil {
			return nil, fmt.Errorf("api: spiffe: %w", err)
		}
	}
	return ids, nil
}

// bearer verifies a JWT: a JWT-SVID when its subject is a spiffe:// ID, an
// OIDC token of a configured issuer otherwise.
func (ids *Identities) bearer(ctx context.Context, token string) (identity, error) {
	claims, err := peek(token)
	if err != nil {
		return identity{}, errBadKey
	}
	if strings.HasPrefix(claims.Sub, "spiffe://") {
		if ids.jwt == nil {
			return identity{}, errBadKey
		}
		svid, err := jwtsvid.ParseAndValidate(token, ids.jwt.get(), []string{ids.audience})
		if err != nil {
			return identity{}, errBadKey
		}
		return identity{kind: "spiffe", issuer: svid.ID.TrustDomain().Name(), subject: svid.ID.String()}, nil
	}
	iss, ok := ids.oidc[claims.Iss]
	if !ok {
		return identity{}, errBadKey
	}
	tok, err := iss.verifier.Verify(oidc.ClientContext(ctx, iss.client), token)
	if err != nil {
		return identity{}, errBadKey
	}
	var all map[string]any
	if err := tok.Claims(&all); err != nil {
		return identity{}, errBadKey
	}
	id := identity{kind: "oidc", issuer: tok.Issuer, subject: tok.Subject}
	if gs, ok := all[iss.conf.GroupsClaim].([]any); ok {
		for _, g := range gs {
			if s, ok := g.(string); ok {
				id.groups = append(id.groups, s)
			}
		}
	}
	return id, nil
}

// peek reads a JWT's claims without verifying them, to choose its verifier.
func peek(token string) (struct{ Iss, Sub string }, error) {
	var c struct{ Iss, Sub string }
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return c, errors.New("not a JWT")
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return c, err
	}
	err = json.Unmarshal(b, &c)
	return c, err
}

// svid verifies an X.509-SVID chain, leaf first.
func (ids *Identities) svid(chain []*x509.Certificate) (identity, error) {
	if ids == nil || ids.x509 == nil || len(chain) == 0 {
		return identity{}, errBadKey
	}
	id, _, err := x509svid.Verify(chain, ids.x509.get())
	if err != nil || id.TrustDomain() != ids.td {
		return identity{}, errBadKey
	}
	return identity{kind: "spiffe", issuer: id.TrustDomain().Name(), subject: id.String()}, nil
}

// entitle maps an identity to its principal through its entitlements (§4.4).
func entitle(ctx context.Context, pool *pgxpool.Pool, id identity) (Principal, error) {
	rows, err := pool.Query(ctx, "select id, tenant_id, role from veho.entitlements_for($1, $2, $3, $4)", id.kind, id.issuer, id.subject, id.groups)
	if err != nil {
		return Principal{}, fmt.Errorf("api: entitlements: %w", err)
	}
	type entitlement struct {
		ID     uuid.UUID
		Tenant *uuid.UUID
		Role   string
	}
	bs, err := pgx.CollectRows(rows, pgx.RowToStructByPos[entitlement])
	if err != nil {
		return Principal{}, fmt.Errorf("api: entitlements: %w", err)
	}
	if len(bs) == 0 {
		return Principal{}, errBadKey
	}
	best := bs[0]
	for _, b := range bs[1:] {
		if (b.Tenant == nil) != (best.Tenant == nil) || (b.Tenant != nil && *b.Tenant != *best.Tenant) {
			return Principal{}, errAmbiguous
		}
		if rank[b.Role] > rank[best.Role] {
			best = b
		}
	}
	return Principal{ID: best.ID, Name: id.kind + ":" + id.subject, Role: best.Role, Tenant: best.Tenant}, nil
}

// errAmbiguous refuses an identity entitled in more than one tenant (§4.4).
var errAmbiguous = errors.New("ambiguous identity: entitled in more than one tenant")

// reloading is a file read again, at most once a minute, when it changed.
type reloading[T any] struct {
	path    string
	load    func(string) (T, error)
	mu      sync.Mutex
	value   T
	mod     time.Time
	checked time.Time
}

func newReloading[T any](path string, load func(string) (T, error)) (*reloading[T], error) {
	r := &reloading[T]{path: path, load: load}
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if r.value, err = load(path); err != nil {
		return nil, err
	}
	r.mod, r.checked = fi.ModTime(), time.Now()
	return r, nil
}

func (r *reloading[T]) get() T {
	r.mu.Lock()
	defer r.mu.Unlock()
	if time.Since(r.checked) < time.Minute {
		return r.value
	}
	r.checked = time.Now()
	if fi, err := os.Stat(r.path); err == nil && !fi.ModTime().Equal(r.mod) {
		if v, err := r.load(r.path); err == nil {
			r.value, r.mod = v, fi.ModTime()
		}
	}
	return r.value
}
