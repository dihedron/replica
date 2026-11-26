//go:build integration

package api

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
	"uuid"

	vehov1 "github.com/dihedron/veho/api/veho/v1"
	"github.com/dihedron/veho/internal/store"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// tenantName is the name of a tenant.
func (f *fixture) tenantName(t *testing.T, id uuid.UUID) string {
	t.Helper()
	var name string
	if err := f.migrator.QueryRow(context.Background(), "select name from veho.tenants where id = $1", id).Scan(&name); err != nil {
		t.Fatal(err)
	}
	return name
}

// in calls as token in the tenant, over REST and gRPC, and returns the HTTP
// status, the code of the problem and the gRPC code of a job list.
func (f *idFixture) in(t *testing.T, token, tenant string) (int, string, codes.Code) {
	t.Helper()
	r := f.rest(t, "GET", "/v1/jobs", token, nil, "X-Veho-Tenant", tenant)
	ctx := as(token)
	if tenant != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "x-veho-tenant", tenant)
	}
	f.tenant = ""
	_, err := f.grpc(t).ListJobs(ctx, &vehov1.ListJobsRequest{})
	code, _ := r.body["code"].(string)
	return r.code, code, status.Code(err)
}

// AU-T1: the admin of a tenant cannot lock out another tenant's users, or a
// platform administrator, by entitling them (G-1).
func TestNoCrossTenantLockout(t *testing.T) {
	f := setupIdentities(t, Options{})
	a, b := f.tenantName(t, f.a), f.tenantName(t, f.b)
	alice, root := "alice-"+uuid.New().String()[:8], "root-"+uuid.New().String()[:8]
	f.entitle(t, "oidc", f.iss, alice, "", &f.a, RoleOperator)
	f.entitle(t, "oidc", f.iss, root, "", nil, RolePlatformAdmin)
	bAdmin := f.key(t, &f.b, RoleAdmin, "b-admin")
	for _, sub := range []string{alice, root} {
		if r := f.rest(t, "POST", "/v1/admin/entitlements", bAdmin, map[string]string{"kind": "oidc", "issuer": f.iss, "subject": sub, "role": "viewer"}); r.code != http.StatusOK {
			t.Fatalf("b entitles %s: %d %v", sub, r.code, r.body)
		}
	}
	if code, problem, g := f.in(t, f.token(t, alice, nil), a); code != http.StatusOK || g != codes.OK {
		t.Errorf("alice in her tenant, also entitled in another: %d %s %v", code, problem, g)
	}
	if r := f.rest(t, "GET", "/v1/tenants", f.token(t, root, nil), nil, "X-Veho-Tenant", "platform"); r.code != http.StatusOK {
		t.Errorf("the platform administrator, also entitled in a tenant: %d %v", r.code, r.body)
	}
	if code, problem, g := f.in(t, f.token(t, alice, nil), b); code != http.StatusOK || g != codes.OK {
		t.Errorf("alice in the other tenant: %d %s %v", code, problem, g)
	}
}

// AU-T1: the tenant is named, and what it does not say.
func TestTenantNaming(t *testing.T) {
	f := setupIdentities(t, Options{})
	alice := "alice-" + uuid.New().String()[:8]
	f.entitle(t, "oidc", f.iss, alice, "", &f.a, RoleAdmin)
	tok := f.token(t, alice, nil)
	if code, problem, g := f.in(t, tok, ""); code != http.StatusBadRequest || problem != "tenant_required" || g != codes.InvalidArgument {
		t.Errorf("no tenant: %d %s %v", code, problem, g)
	}
	suspended := "susp-" + uuid.New().String()[:8]
	var id uuid.UUID
	if err := f.migrator.QueryRow(context.Background(), "select veho.create_tenant($1)", suspended).Scan(&id); err != nil {
		t.Fatal(err)
	}
	f.entitle(t, "oidc", f.iss, alice, "", &id, RoleAdmin)
	if _, err := f.migrator.Exec(context.Background(), "update veho.tenants set state = 'suspended' where id = $1", id); err != nil {
		t.Fatal(err)
	}
	for what, tenant := range map[string]string{"unknown": "no-such-tenant", "suspended": suspended, "not entitled": f.tenantName(t, f.b), "platform": "platform"} {
		if code, problem, g := f.in(t, tok, tenant); code != http.StatusForbidden || problem != "not_entitled" || g != codes.PermissionDenied {
			t.Errorf("%s: %d %s %v", what, code, problem, g)
		}
	}
	// a bad credential is still 401, whatever the tenant
	if code, _, g := f.in(t, f.token(t, alice, map[string]any{"aud": "other"}), f.tenantName(t, f.a)); code != http.StatusUnauthorized || g != codes.Unauthenticated {
		t.Errorf("a bad token: %d %v", code, g)
	}
	// an API key has its tenant, and cannot name another
	for tenant, want := range map[string]int{"": http.StatusOK, f.tenantName(t, f.a): http.StatusOK, f.tenantName(t, f.b): http.StatusForbidden, "platform": http.StatusForbidden} {
		if code, problem, _ := f.in(t, f.keys["a-viewer"], tenant); code != want {
			t.Errorf("a key naming %q: %d %s", tenant, code, problem)
		}
	}
	if r := f.rest(t, "GET", "/v1/tenants", f.keys["platform"], nil, "X-Veho-Tenant", "platform"); r.code != http.StatusOK {
		t.Errorf("a platform key naming platform: %d", r.code)
	}
	if r := f.rest(t, "POST", "/v1/tenants", f.keys["platform"], map[string]string{"name": "platform"}); r.code != http.StatusBadRequest {
		t.Errorf("a tenant named platform: %d %v", r.code, r.body)
	}
}

// AU-T2: equal subjects and groups of two issuers are two identities (G-2).
func TestIssuersAreDistinct(t *testing.T) {
	other := newSigner("oidc-2")
	srv, ca := issuer(t, other)
	f := setupIssuers(t, Options{}, oidcConf(srv.URL, ca))
	sub := "alice-" + uuid.New().String()[:8]
	for _, iss := range []string{f.iss, srv.URL} {
		f.entitle(t, "oidc", iss, sub, "", &f.a, RoleOperator)
	}
	claims := map[string]any{"iss": srv.URL, "sub": sub, "aud": audience, "groups": []string{"ops"},
		"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix()}
	first, second := f.token(t, sub, map[string]any{"groups": []string{"ops"}}), other.sign(t, claims)
	admin := f.keys["a-admin"]
	if r := f.rest(t, "POST", "/v1/policies", admin, map[string]any{"name": "geo", "policy": job("x/y")["policy"]}); r.code != http.StatusOK {
		t.Fatalf("policy: %d %v", r.code, r.body)
	}
	for _, grantee := range []string{"oidc:" + f.iss + "#" + sub, "group:" + f.iss + "#ops"} {
		if r := f.rest(t, "PUT", "/v1/policies/geo/grants", admin, map[string]any{"grants": []map[string]string{{"grantee": grantee, "permission": "read"}}}); r.code != http.StatusOK {
			t.Fatalf("grant to %s: %d %v", grantee, r.code, r.body)
		}
		if r := f.rest(t, "GET", "/v1/policies/geo", first, nil); r.code != http.StatusOK {
			t.Errorf("%s, the issuer's: %d", grantee, r.code)
		}
		if r := f.rest(t, "GET", "/v1/policies/geo", second, nil); r.code != http.StatusNotFound {
			t.Errorf("%s, another issuer's: %d", grantee, r.code)
		}
	}
	for _, grantee := range []string{"oidc:" + sub, "group:ops", "group:#ops", "oidc:" + f.iss + "#", "spiffe:x", "key:"} {
		if r := f.rest(t, "PUT", "/v1/policies/geo/grants", admin, map[string]any{"grants": []map[string]string{{"grantee": grantee, "permission": "read"}}}); r.code != http.StatusBadRequest {
			t.Errorf("the grantee %q: %d", grantee, r.code)
		}
	}
}

// AU-T3: the members of a group have a rate limit each (G-6).
func TestGroupMembersLimits(t *testing.T) {
	f := setupIdentities(t, Options{RateLimit: rateLimit(1000, 0.01, 200, 1000)}) // two calls each
	group := "g-" + uuid.New().String()[:8]
	f.entitle(t, "oidc", f.iss, "", group, &f.a, RoleViewer)
	member := func(sub string) string { return f.token(t, sub, map[string]any{"groups": []string{group}}) }
	for range 4 {
		f.rest(t, "GET", "/v1/jobs", member("greedy"), nil)
	}
	if r := f.rest(t, "GET", "/v1/jobs", member("greedy"), nil); r.code != http.StatusTooManyRequests {
		t.Errorf("the greedy member: %d", r.code)
	}
	if r := f.rest(t, "GET", "/v1/jobs", member("other"), nil); r.code != http.StatusOK {
		t.Errorf("another member of the group: %d", r.code)
	}
}

// AU-T5: an idempotency key is its caller's, and a job says who created it
// (G-3, G-4).
func TestIdempotencyPerCaller(t *testing.T) {
	f := setup(t, Options{})
	one, oneName := f.keyAs(t, &f.a, RoleOperator, "one-"+uuid.New().String()[:8])
	two, _ := f.keyAs(t, &f.a, RoleOperator, "two-"+uuid.New().String()[:8])
	submit := func(key string) string {
		t.Helper()
		r := f.rest(t, "POST", "/v1/jobs", key, job("out/idem"), "Idempotency-Key", "k1")
		if r.code != http.StatusAccepted {
			t.Fatalf("submit: %d %v", r.code, r.body)
		}
		return r.body["id"].(string)
	}
	first := submit(one)
	if again := submit(one); again != first {
		t.Errorf("the same caller: %s, then %s", first, again)
	}
	if other := submit(two); other == first {
		t.Error("another caller with the same key got the first caller's job")
	}
	var by string
	if err := store.InTenant(context.Background(), f.api, f.a, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), "select created_by from veho.jobs where id = $1", first).Scan(&by)
	}); err != nil || by != oneName {
		t.Errorf("created by %q, %v, want %q", by, err, oneName)
	}
	if _, err := f.migrator.Exec(context.Background(), "update veho.jobs set created_by = 'key:x' where id = $1", first); err == nil {
		t.Error("the creator of a job changed")
	}
}

// FuzzTenantName: whatever the caller names, an identity without an
// entitlement is refused, and no name is an error of the database.
func FuzzTenantName(f *testing.F) {
	for _, s := range []string{"", "platform", "Platform", "\xff", "a\x00b", "' or true --", "ü", strings.Repeat("x", 1<<16)} {
		f.Add(s)
	}
	dsn := os.Getenv("VEHO_TEST_API_DATABASE_URL")
	if dsn == "" {
		f.Skip("VEHO_TEST_API_DATABASE_URL is not set")
	}
	pool, err := store.Open(context.Background(), dsn, store.Options{Name: "test/fuzz"})
	if err != nil {
		f.Fatal(err)
	}
	f.Cleanup(pool.Close)
	f.Fuzz(func(t *testing.T, tenant string) {
		_, err := entitle(context.Background(), pool, identity{kind: "oidc", issuer: "https://fuzz.example", subject: "nobody"}, tenant)
		if !errors.Is(err, errNotEntitled) && !errors.Is(err, errTenantRequired) {
			t.Errorf("%q: %v", tenant, err)
		}
	})
}
