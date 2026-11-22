//go:build integration

package api

import (
	"encoding/base64"
	"net/http"
	"os"
	"strings"
	"testing"
	"uuid"

	"github.com/dihedron/veho/internal/config"
)

// API-T18: endpoints.
func TestEndpoints(t *testing.T) {
	f := setup(t, Options{})
	admin := f.keys["a-admin"]
	root := t.TempDir()
	ep := map[string]any{"name": "e1", "driver": "file", "location": "lab-a", "config": map[string]string{"root": root}}
	if r := f.rest(t, "POST", "/v1/endpoints", admin, ep); r.code != http.StatusOK || r.body["driver"] != "file" {
		t.Fatalf("create: %d %v", r.code, r.body)
	}
	if r := f.rest(t, "POST", "/v1/endpoints", admin, ep); r.code != http.StatusConflict || r.body["code"] != "already_exists" {
		t.Errorf("create again: %d %v", r.code, r.body)
	}
	if r := f.rest(t, "GET", "/v1/endpoints/e1", f.keys["a-viewer"], nil); r.code != http.StatusOK || r.body["location"] != "lab-a" {
		t.Errorf("get: %d %v", r.code, r.body)
	}
	if r := f.rest(t, "GET", "/v1/endpoints", f.keys["a-viewer"], nil); len(r.body["endpoints"].([]any)) != 4 {
		t.Errorf("list: %v", r.body)
	}
	root2 := t.TempDir()
	if r := f.rest(t, "PUT", "/v1/endpoints/e1", admin, map[string]any{"location": "lab-a", "config": map[string]string{"root": root2}}); r.code != http.StatusOK ||
		r.body["config"].(map[string]any)["root"] != root2 {
		t.Errorf("update: %d %v", r.code, r.body)
	}
	if r := f.rest(t, "PUT", "/v1/endpoints/e1", admin, map[string]any{"driver": "s3", "location": "lab-a", "config": map[string]string{}}); r.code != http.StatusBadRequest {
		t.Errorf("change the driver: %d", r.code)
	}
	for name, bad := range map[string]map[string]any{
		"driver":   {"name": "e2", "driver": "nope", "location": "lab-a", "config": map[string]string{}},
		"location": {"name": "e2", "driver": "file", "location": "nowhere", "config": map[string]string{}},
		"name":     {"name": "E 2", "driver": "file", "location": "lab-a", "config": map[string]string{}},
	} {
		if r := f.rest(t, "POST", "/v1/endpoints", admin, bad); r.code != http.StatusBadRequest {
			t.Errorf("a bad %s: %d %v", name, r.code, r.body)
		}
	}
	missing := map[string]any{"name": "e3", "driver": "file", "location": "lab-a", "config": map[string]string{"root": root}, "credential_ref": "db://none"}
	if r := f.rest(t, "POST", "/v1/endpoints", admin, missing); r.code != http.StatusNotFound {
		t.Errorf("a missing secret: %d %v", r.code, r.body)
	}
	if r := f.rest(t, "POST", "/v1/endpoints", f.keys["a-operator"], ep); r.code != http.StatusForbidden {
		t.Errorf("an operator creates: %d", r.code)
	}
	if r := f.rest(t, "POST", "/v1/endpoints/e1:test", admin, map[string]any{}); r.code != http.StatusOK || r.body["ok"] != true {
		t.Errorf("test: %d %v", r.code, r.body)
	}
	if err := os.Chmod(root2, 0o500); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(root2, 0o700) }()
	if r := f.rest(t, "POST", "/v1/endpoints/e1:test", admin, map[string]any{}); r.code != http.StatusOK || r.body["ok"] == true || r.body["step"] != "write" {
		t.Errorf("test an unwritable root: %d %v", r.code, r.body)
	}
	if r := f.rest(t, "DELETE", "/v1/endpoints/e1", admin, nil); r.code != http.StatusOK {
		t.Errorf("delete: %d %v", r.code, r.body)
	}
	f.rest(t, "POST", "/v1/jobs", f.keys["a-operator"], job("out/ep"))
	if r := f.rest(t, "DELETE", "/v1/endpoints/source", admin, nil); r.code != http.StatusConflict || r.body["code"] != "in_use" {
		t.Errorf("delete an endpoint a job uses: %d %v", r.code, r.body)
	}
}

// API-T19: secrets, write-only.
func TestSecrets(t *testing.T) {
	f := setup(t, Options{})
	admin := f.keys["a-admin"]
	canary := "canary-" + uuid.New().String()
	encoded := base64.StdEncoding.EncodeToString([]byte(canary))
	var bodies strings.Builder
	keep := func(r reply) reply {
		for k, v := range r.body {
			bodies.WriteString(k)
			bodies.WriteString("=")
			bodies.WriteString(strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(toString(v), "\n", ""), " ", "")))
		}
		return r
	}
	if r := keep(f.rest(t, "PUT", "/v1/secrets/s1", admin, map[string]string{"value": encoded})); r.code != http.StatusOK || r.body["version"] != 1.0 {
		t.Fatalf("put: %d %v", r.code, r.body)
	}
	if r := keep(f.rest(t, "GET", "/v1/secrets/s1", admin, nil)); r.code != http.StatusOK || r.body["value"] != nil {
		t.Errorf("get: %d %v", r.code, r.body)
	}
	if r := keep(f.rest(t, "GET", "/v1/secrets", admin, nil)); len(r.body["secrets"].([]any)) != 1 {
		t.Errorf("list: %v", r.body)
	}
	if strings.Contains(bodies.String(), canary) || strings.Contains(bodies.String(), encoded) {
		t.Error("an answer carries the value")
	}
	if r := f.rest(t, "GET", "/v1/secrets", f.keys["a-operator"], nil); r.code != http.StatusForbidden {
		t.Errorf("an operator lists: %d", r.code)
	}
	ep := map[string]any{"name": "s3e", "driver": "file", "location": "lab-a", "config": map[string]string{"root": t.TempDir()}, "credential_ref": "db://s1"}
	if r := f.rest(t, "POST", "/v1/endpoints", admin, ep); r.code != http.StatusOK {
		t.Fatalf("an endpoint with the secret: %d %v", r.code, r.body)
	}
	if r := f.rest(t, "DELETE", "/v1/secrets/s1", admin, nil); r.code != http.StatusConflict || r.body["code"] != "in_use" {
		t.Errorf("delete a used secret: %d %v", r.code, r.body)
	}
}

func toString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	default:
		return ""
	}
}

// API-T20: tenants, for platform administrators.
func TestTenants(t *testing.T) {
	f := setup(t, Options{})
	root := f.keys["platform"]
	name := "t-" + uuid.New().String()[:8]
	r := f.rest(t, "POST", "/v1/tenants", root, map[string]string{"name": name})
	if r.code != http.StatusOK || r.body["state"] != "active" {
		t.Fatalf("create: %d %v", r.code, r.body)
	}
	id := r.body["id"].(string)
	if r := f.rest(t, "GET", "/v1/tenants/"+id, root, nil); r.body["name"] != name {
		t.Errorf("get: %v", r.body)
	}
	found := false
	for _, x := range f.rest(t, "GET", "/v1/tenants", root, nil).body["tenants"].([]any) {
		found = found || x.(map[string]any)["id"] == id
	}
	if !found {
		t.Error("list: the new tenant is missing")
	}
	tid := uuid.MustParse(id)
	key := f.key(t, &tid, RoleAdmin, "boss")
	if r := f.rest(t, "GET", "/v1/jobs", key, nil); r.code != http.StatusOK {
		t.Errorf("the tenant's key: %d", r.code)
	}
	if r := f.rest(t, "PATCH", "/v1/tenants/"+id, root, map[string]any{"state": "suspended", "quotas": map[string]int{"max_active_jobs": 3}}); r.code != http.StatusOK ||
		r.body["state"] != "suspended" || r.body["quotas"].(map[string]any)["max_active_jobs"] != 3.0 {
		t.Errorf("suspend: %d %v", r.code, r.body)
	}
	if r := f.rest(t, "GET", "/v1/jobs", key, nil); r.code != http.StatusUnauthorized {
		t.Errorf("a suspended tenant's key: %d", r.code)
	}
	if r := f.rest(t, "PATCH", "/v1/tenants/"+id, root, map[string]any{"state": "gone"}); r.code != http.StatusBadRequest {
		t.Errorf("a bad state: %d", r.code)
	}
	if r := f.rest(t, "GET", "/v1/tenants", f.keys["a-admin"], nil); r.code != http.StatusForbidden {
		t.Errorf("a tenant admin lists tenants: %d", r.code)
	}
}

// API-T21: a tenant's keys, entitlements and settings.
func TestAdmin(t *testing.T) {
	f := setup(t, Options{})
	admin := f.keys["a-admin"]
	r := f.rest(t, "POST", "/v1/admin/keys", admin, map[string]any{"name": "ci", "role": "viewer"})
	if r.code != http.StatusOK || !strings.HasPrefix(toString(r.body["key"]), "veho_") {
		t.Fatalf("create a key: %d %v", r.code, r.body)
	}
	key, id := r.body["key"].(string), r.body["id"].(string)
	if g := f.rest(t, "GET", "/v1/jobs", key, nil); g.code != http.StatusOK {
		t.Errorf("the new key: %d", g.code)
	}
	l := f.rest(t, "GET", "/v1/admin/keys", admin, nil)
	secret := key[len("veho_")+keyIDLen+1:]
	for _, k := range l.body["keys"].([]any) {
		for _, v := range k.(map[string]any) {
			if strings.Contains(toString(v), secret) {
				t.Error("the list carries a secret")
			}
		}
	}
	if r := f.rest(t, "POST", "/v1/admin/keys", admin, map[string]any{"name": "root", "role": "platform-admin"}); r.code != http.StatusBadRequest {
		t.Errorf("a platform-admin key from a tenant admin: %d", r.code)
	}
	if r := f.rest(t, "POST", "/v1/admin/keys/"+id+":revoke", admin, map[string]any{}); r.code != http.StatusOK {
		t.Errorf("revoke: %d", r.code)
	}
	if g := f.rest(t, "GET", "/v1/jobs", key, nil); g.code != http.StatusUnauthorized {
		t.Errorf("a revoked key: %d", g.code)
	}
	e := f.rest(t, "POST", "/v1/admin/entitlements", admin, map[string]any{"kind": "oidc", "issuer": "https://idp.example", "subject": "zoe", "role": "viewer"})
	if e.code != http.StatusOK {
		t.Fatalf("create an entitlement: %d %v", e.code, e.body)
	}
	if l := f.rest(t, "GET", "/v1/admin/entitlements", admin, nil); len(l.body["entitlements"].([]any)) != 1 {
		t.Errorf("list entitlements: %v", l.body)
	}
	if d := f.rest(t, "DELETE", "/v1/admin/entitlements/"+e.body["id"].(string), admin, nil); d.code != http.StatusOK {
		t.Errorf("delete an entitlement: %d", d.code)
	}
	if r := f.rest(t, "POST", "/v1/admin/entitlements", admin, map[string]any{"kind": "oidc", "issuer": "https://idp.example", "role": "viewer"}); r.code != http.StatusBadRequest {
		t.Errorf("an entitlement without subject or group: %d", r.code)
	}
	if s := f.rest(t, "PUT", "/v1/admin/settings", admin, map[string]string{"inline_policies": "none"}); s.code != http.StatusOK || s.body["inline_policies"] != "none" {
		t.Errorf("settings: %d %v", s.code, s.body)
	}
	if s := f.rest(t, "GET", "/v1/admin/settings", admin, nil); s.body["inline_policies"] != "none" {
		t.Errorf("get settings: %v", s.body)
	}
	if s := f.rest(t, "PUT", "/v1/admin/settings", admin, map[string]string{"inline_policies": "sometimes"}); s.code != http.StatusBadRequest {
		t.Errorf("a bad setting: %d", s.code)
	}
	if r := f.rest(t, "GET", "/v1/admin/keys", f.keys["a-operator"], nil); r.code != http.StatusForbidden {
		t.Errorf("an operator lists keys: %d", r.code)
	}
}

// API-T22: rate limits.
func TestRateLimits(t *testing.T) {
	f := setup(t, Options{RateLimit: rateLimit(1, 1, 1, 3)})
	key := f.keys["a-viewer"]
	limited := 0
	for range 5 {
		if r := f.rest(t, "GET", "/v1/jobs", key, nil); r.code == http.StatusTooManyRequests {
			limited++
			if r.header.Get("Retry-After") == "" || r.body["code"] != "rate_limited" {
				t.Errorf("a limited answer: %v %v", r.header, r.body)
			}
		}
	}
	if limited == 0 {
		t.Error("never limited")
	}
	for range 3 {
		f.rest(t, "GET", "/v1/jobs", "veho_wrongwrongwr_AAAA", nil)
	}
	if r := f.rest(t, "GET", "/v1/jobs", f.keys["a-admin"], nil); r.code != http.StatusTooManyRequests {
		t.Errorf("a valid key from an address that failed 3 times: %d", r.code)
	}
}

func rateLimit(tenant, principal, burst, failures float64) config.RateLimit {
	return config.RateLimit{TenantPerSecond: tenant, PrincipalPerSecond: principal, Burst: burst, FailuresPerMinute: failures}
}
