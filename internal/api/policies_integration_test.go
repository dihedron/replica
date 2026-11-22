//go:build integration

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"uuid"
)

func geo(path string) map[string]any {
	return map[string]any{"destinations": []any{
		map[string]string{"endpoint": "d1", "path": path}, map[string]string{"endpoint": "d2", "path": path}}}
}

func byRef(name string, version int) map[string]any {
	return map[string]any{"source": map[string]string{"endpoint": "source", "path": "obj"},
		"policy_ref": map[string]any{"name": name, "version": version}}
}

// API-T14: versions, validation, deletion.
func TestPolicies(t *testing.T) {
	f := setup(t, Options{})
	admin := f.keys["a-admin"]
	for i, path := range []string{"v1/x", "v2/x"} {
		if r := f.rest(t, "POST", "/v1/policies", admin, map[string]any{"name": "geo", "policy": geo(path)}); r.code != http.StatusOK || r.body["version"] != float64(i+1) {
			t.Fatalf("create v%d: %d %v", i+1, r.code, r.body)
		}
	}
	if r := f.rest(t, "GET", "/v1/policies/geo", admin, nil); r.body["version"] != 2.0 {
		t.Errorf("latest: %v", r.body)
	}
	if r := f.rest(t, "GET", "/v1/policies/geo?version=1", admin, nil); r.body["version"] != 1.0 {
		t.Errorf("v1: %v", r.body)
	}
	one := map[string]any{"destinations": []any{map[string]string{"endpoint": "d1", "path": "x"}}}
	if r := f.rest(t, "POST", "/v1/policies:validate", admin, map[string]any{"policy": one}); r.code != http.StatusOK || r.body["valid"] == true || r.body["code"] != "invalid_policy" {
		t.Errorf("validate a bad policy: %d %v", r.code, r.body)
	}
	if r := f.rest(t, "POST", "/v1/policies:validate", admin, map[string]any{"policy": geo("x")}); r.body["valid"] != true {
		t.Errorf("validate a good policy: %v", r.body)
	}
	if r := f.rest(t, "POST", "/v1/policies", admin, map[string]any{"name": "bad", "policy": one}); r.code != http.StatusBadRequest {
		t.Errorf("create a bad policy: %d", r.code)
	}
	if r := f.rest(t, "GET", "/v1/policies", admin, nil); len(r.body["policies"].([]any)) != 1 {
		t.Errorf("list: %v", r.body)
	}

	// a job in progress under v1 holds the name
	id := uuid.NewV7()
	if _, err := f.migrator.Exec(context.Background(), `insert into veho.jobs (id, tenant_id, source_endpoint_id, source_path, policy, state, deadline_at, policy_ref)
		select $1, $2, e.id, 'obj', '{}', 'RUNNING', now() + interval '1 hour',
			(select p.id from veho.policies p where p.tenant_id = $2 and p.name = 'geo' and p.version = 1)
		from veho.endpoints e where e.tenant_id = $2 and e.name = 'source'`, id, f.a); err != nil {
		t.Fatal(err)
	}
	if r := f.rest(t, "DELETE", "/v1/policies/geo", admin, nil); r.code != http.StatusConflict || r.body["code"] != "in_use" {
		t.Errorf("delete while used: %d %v", r.code, r.body)
	}
	if _, err := f.migrator.Exec(context.Background(), "update veho.jobs set state = 'FAILED' where id = $1", id); err != nil {
		t.Fatal(err)
	}
	if r := f.rest(t, "DELETE", "/v1/policies/geo", admin, nil); r.code != http.StatusOK {
		t.Errorf("delete: %d %v", r.code, r.body)
	}
	if r := f.rest(t, "GET", "/v1/policies/geo", admin, nil); r.code != http.StatusNotFound {
		t.Errorf("get a deleted policy: %d", r.code)
	}
	if r := f.rest(t, "POST", "/v1/policies", admin, map[string]any{"name": "geo", "policy": geo("v3/x")}); r.body["version"] != 3.0 {
		t.Errorf("the name again: %v", r.body)
	}
}

// API-T15, API-T17: grants decide who sees, submits and manages; a job by
// reference keeps the version it used.
func TestGrants(t *testing.T) {
	f := setupIdentities(t, Options{})
	admin := f.keys["a-admin"]
	op, opName := f.keyAs(t, &f.a, RoleOperator, "op-"+uuid.New().String()[:8])
	f.rest(t, "POST", "/v1/policies", admin, map[string]any{"name": "geo", "policy": geo("g/x")})
	grant := func(gs ...map[string]string) {
		t.Helper()
		if r := f.rest(t, "PUT", "/v1/policies/geo/grants", admin, map[string]any{"grants": gs}); r.code != http.StatusOK {
			t.Fatalf("set grants: %d %v", r.code, r.body)
		}
	}
	if r := f.rest(t, "GET", "/v1/policies/geo", op, nil); r.code != http.StatusNotFound {
		t.Errorf("no grant, get: %d", r.code)
	}
	if r := f.rest(t, "POST", "/v1/jobs", op, byRef("geo", 0)); r.code != http.StatusNotFound {
		t.Errorf("no grant, submit: %d", r.code)
	}
	grant(map[string]string{"grantee": opName, "permission": "read"})
	if r := f.rest(t, "GET", "/v1/policies/geo", op, nil); r.code != http.StatusOK {
		t.Errorf("read, get: %d", r.code)
	}
	if r := f.rest(t, "POST", "/v1/jobs", op, byRef("geo", 0)); r.code != http.StatusForbidden {
		t.Errorf("read, submit: %d %v", r.code, r.body)
	}
	grant(map[string]string{"grantee": opName, "permission": "submit"})
	r := f.rest(t, "POST", "/v1/jobs", op, byRef("geo", 0))
	if r.code != http.StatusAccepted {
		t.Fatalf("submit, submit: %d %v", r.code, r.body)
	}
	job := r.body["id"].(string)
	if ref, _ := r.body["policy_ref"].(map[string]any); ref["name"] != "geo" || ref["version"] != 1.0 {
		t.Errorf("the job's reference: %v", r.body["policy_ref"])
	}
	if r := f.rest(t, "POST", "/v1/policies", op, map[string]any{"name": "geo", "policy": geo("g2/x")}); r.code != http.StatusForbidden {
		t.Errorf("submit, new version: %d", r.code)
	}
	grant(map[string]string{"grantee": opName, "permission": "manage"})
	if r := f.rest(t, "POST", "/v1/policies", op, map[string]any{"name": "geo", "policy": geo("g2/x")}); r.code != http.StatusOK || r.body["version"] != 2.0 {
		t.Errorf("manage, new version: %d %v", r.code, r.body)
	}
	if r := f.rest(t, "POST", "/v1/policies", op, map[string]any{"name": "new", "policy": geo("n/x")}); r.code != http.StatusForbidden {
		t.Errorf("an operator creates a name: %d", r.code)
	}
	// API-T17: the job still says v1, and its snapshot is v1's
	var snapshot []byte
	if err := f.migrator.QueryRow(context.Background(), "select policy from veho.jobs where id = $1", job).Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	var p struct{ Destinations []struct{ Path string } }
	_ = json.Unmarshal(snapshot, &p)
	if g := f.rest(t, "GET", "/v1/jobs/"+job, op, nil); g.body["policy_ref"].(map[string]any)["version"] != 1.0 || p.Destinations[0].Path != "g/x" {
		t.Errorf("the job after v2: %v, %s", g.body["policy_ref"], snapshot)
	}

	// a group grant, for an OIDC member
	f.entitle(t, "oidc", f.iss, "", "replicators", &f.a, RoleOperator)
	grant(map[string]string{"grantee": "group:replicators", "permission": "submit"})
	member := f.token(t, "erin", map[string]any{"groups": []string{"replicators"}})
	if r := f.rest(t, "POST", "/v1/jobs", member, byRef("geo", 0)); r.code != http.StatusAccepted {
		t.Errorf("a group member submits: %d %v", r.code, r.body)
	}
	if r := f.rest(t, "PUT", "/v1/policies/geo/grants", admin, map[string]any{"grants": []map[string]string{{"grantee": "nobody", "permission": "read"}}}); r.code != http.StatusBadRequest {
		t.Errorf("a malformed grantee: %d", r.code)
	}
}

// API-T16: the tenant's inline_policies.
func TestInlinePolicies(t *testing.T) {
	f := setup(t, Options{})
	op, opName := f.keyAs(t, &f.a, RoleOperator, "op-"+uuid.New().String()[:8])
	setting := func(mode string) {
		t.Helper()
		if _, err := f.migrator.Exec(context.Background(), `update veho.tenants set settings = jsonb_build_object('inline_policies', $2::text) where id = $1`, f.a, mode); err != nil {
			t.Fatal(err)
		}
	}
	if r := f.rest(t, "POST", "/v1/jobs", op, job("i/all")); r.code != http.StatusAccepted {
		t.Errorf("all: %d %v", r.code, r.body)
	}
	setting("granted")
	if r := f.rest(t, "POST", "/v1/jobs", op, job("i/granted")); r.code != http.StatusForbidden || r.body["code"] != "inline_policy_refused" {
		t.Errorf("granted, no grant: %d %v", r.code, r.body)
	}
	if _, err := f.migrator.Exec(context.Background(), `insert into veho.policy_grants (tenant_id, grantee, permission) values ($1, $2, 'submit_inline')`, f.a, opName); err != nil {
		t.Fatal(err)
	}
	if r := f.rest(t, "POST", "/v1/jobs", op, job("i/granted2")); r.code != http.StatusAccepted {
		t.Errorf("granted, with the grant: %d %v", r.code, r.body)
	}
	setting("none")
	if r := f.rest(t, "POST", "/v1/jobs", f.keys["a-admin"], job("i/none")); r.code != http.StatusForbidden {
		t.Errorf("none, an admin: %d", r.code)
	}
}
