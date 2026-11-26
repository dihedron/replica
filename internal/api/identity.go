package api

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
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
	kind    string // oidc, spiffe; api_key in a Principal
	issuer  string // the OIDC issuer, or the SPIFFE trust domain
	subject string
	groups  []string
	// an OIDC token's acr and auth_time, and its issuer's step-up
	acr      string
	authTime time.Time
	stepUp   *config.StepUp
}

// name is the identity's name (veho-authentication.md §3.1).
func (id identity) name() string {
	if id.kind == "spiffe" {
		return id.subject
	}
	return id.kind + ":" + id.issuer + "#" + id.subject
}

// oidcIssuer is an issuer; its verifier is nil until its discovery document
// was fetched.
type oidcIssuer struct {
	conf     config.OIDC
	verifier atomic.Pointer[oidc.IDTokenVerifier]
	client   *http.Client
}

// tokenSkew is the clock difference accepted with an issuer.
const tokenSkew = time.Minute

// errIssuerDown: the token cannot be verified now, which says nothing about
// the token (veho-authentication.md §5).
var errIssuerDown = errors.New("the OIDC issuer cannot be reached")

// keysFailed is how go-oidc says that it could not fetch the keys; it keeps
// no error to test for.
const keysFailed = "failed to verify signature: fetching keys "

func (i *oidcIssuer) discover(ctx context.Context) error {
	// the context stays with the verifier, which fetches the keys with it
	p, err := oidc.NewProvider(oidc.ClientContext(ctx, i.client), i.conf.Issuer)
	if err != nil {
		return err
	}
	i.verifier.Store(p.Verifier(&oidc.Config{ClientID: i.conf.Audience, Now: func() time.Time { return time.Now().Add(-tokenSkew) }}))
	return nil
}

// rediscover retries discovery until it succeeds.
func (i *oidcIssuer) rediscover(ctx context.Context) {
	for wait := time.Second; ; wait = min(2*wait, time.Minute) {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		err := i.discover(ctx)
		if err == nil {
			slog.InfoContext(ctx, "OIDC issuer reached", "issuer", i.conf.Issuer)
			return
		}
		slog.WarnContext(ctx, "OIDC issuer unreachable", "issuer", i.conf.Issuer, "error", err)
	}
}

// verify verifies a token of the issuer, and that its lifetime is bounded.
func (i *oidcIssuer) verify(ctx context.Context, token string) (*oidc.IDToken, error) {
	v := i.verifier.Load()
	if v == nil {
		return nil, errIssuerDown
	}
	tok, err := v.Verify(oidc.ClientContext(ctx, i.client), token)
	switch {
	case err != nil && strings.HasPrefix(err.Error(), keysFailed):
		return nil, fmt.Errorf("%w: %w", errIssuerDown, err)
	case err != nil, tok.IssuedAt.IsZero(), tok.IssuedAt.After(time.Now().Add(tokenSkew)), tok.Expiry.Sub(tok.IssuedAt) > i.conf.MaxTokenLifetime:
		return nil, errBadKey
	}
	return tok, nil
}

// Identities verifies the OIDC and SPIFFE credentials of §4.4.
type Identities struct {
	oidc     map[string]*oidcIssuer // by issuer
	td       spiffeid.TrustDomain
	x509     *reloading[*x509bundle.Bundle]
	jwt      *reloading[*jwtbundle.Bundle]
	audience string
}

// NewIdentities reads the SPIFFE bundles and the issuers' CAs, and fails when
// one cannot be had (fail fast). An OIDC issuer that does not answer stops
// nothing: its discovery is retried until ctx ends.
func NewIdentities(ctx context.Context, c config.API) (*Identities, error) {
	ids := &Identities{oidc: map[string]*oidcIssuer{}, audience: c.SPIFFE.Audience}
	for _, o := range c.OIDC {
		client := &http.Client{Timeout: 10 * time.Second}
		if o.CAFile != "" {
			pem, err := os.ReadFile(o.CAFile)
			if err != nil {
				return nil, fmt.Errorf("api: oidc %s: %w", o.Issuer, err)
			}
			roots := x509.NewCertPool()
			if !roots.AppendCertsFromPEM(pem) {
				return nil, fmt.Errorf("api: oidc %s: no certificate in %s", o.Issuer, o.CAFile)
			}
			client.Transport = &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}
		}
		iss := &oidcIssuer{conf: o, client: client}
		ids.oidc[o.Issuer] = iss
		if err := iss.discover(ctx); err != nil {
			slog.WarnContext(ctx, "OIDC issuer unreachable", "issuer", o.Issuer, "error", err)
			go iss.rediscover(ctx)
		}
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
	tok, err := iss.verify(ctx, token)
	if err != nil {
		return identity{}, err
	}
	var all map[string]any
	if err := tok.Claims(&all); err != nil {
		return identity{}, errBadKey
	}
	id := identity{kind: "oidc", issuer: tok.Issuer, subject: tok.Subject, stepUp: iss.conf.StepUp}
	id.acr, _ = all["acr"].(string)
	if at, ok := all["auth_time"].(float64); ok {
		id.authTime = time.Unix(int64(at), 0)
	}
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

// entitle maps an identity to its principal through its entitlements in the
// tenant it names (veho-authentication.md §4); the highest role applies.
func entitle(ctx context.Context, pool *pgxpool.Pool, id identity, tenant string) (Principal, error) {
	p := Principal{Name: id.name(), id: id}
	for _, g := range id.groups {
		p.Groups = append(p.Groups, id.issuer+"#"+g)
	}
	if tenant == "" && id.kind == "oidc" && ctx.Value(noTenantKey{}) != nil {
		// WhoAmI: who it is, everywhere; a SPIFFE identity's patterns would
		// have to be matched in every tenant, so it names one
		p.Role = roleIdentified
		return p, nil
	}
	if tenant == "" {
		return p, errTenantRequired
	}
	if !utf8.ValidString(tenant) || strings.ContainsRune(tenant, 0) { // no text of PostgreSQL's
		return p, errNotEntitled
	}
	rows, err := pool.Query(ctx, "select tenant_id, role from veho.entitlements_for($1, $2, $3, $4, $5)", tenant, id.kind, id.issuer, id.subject, id.groups)
	if err != nil {
		return Principal{}, fmt.Errorf("api: entitlements: %w", err)
	}
	type entitlement struct {
		Tenant *uuid.UUID
		Role   string
	}
	bs, err := pgx.CollectRows(rows, pgx.RowToStructByPos[entitlement])
	if err != nil {
		return Principal{}, fmt.Errorf("api: entitlements: %w", err)
	}
	if len(bs) == 0 {
		return p, errNotEntitled
	}
	best := bs[0]
	for _, b := range bs[1:] {
		if rank[b.Role] > rank[best.Role] {
			best = b
		}
	}
	p.Role, p.Tenant, p.TenantName = best.Role, best.Tenant, tenant
	return p, nil
}

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
