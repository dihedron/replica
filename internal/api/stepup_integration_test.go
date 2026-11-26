//go:build integration

package api

import (
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	vehov1 "github.com/dihedron/veho/api/veho/v1"
	"github.com/dihedron/veho/internal/config"
	"github.com/dihedron/veho/internal/testidp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// setupStepUp is the identity fixture with a second OIDC issuer, corp,
// that offers the CLI's login and asks the sensitive methods for acr 2 in
// the last 10 minutes.
func setupStepUp(t *testing.T) (*idFixture, *testidp.IdP) {
	t.Helper()
	idp := testidp.New(t, audience, "veho-cli")
	f := setupIssuers(t, Options{}, config.OIDC{Name: "corp", Issuer: idp.URL, Audience: audience, GroupsClaim: "groups", CAFile: idp.CAFile,
		MaxTokenLifetime: time.Hour, CLIClientID: "veho-cli", Scopes: []string{"openid", "offline_access"},
		StepUp: &config.StepUp{ACRValues: []string{"2", "3"}, MaxAge: 10 * time.Minute}})
	return f, idp
}

// AU-T8: a sensitive method needs the issuer's acr and a recent auth_time;
// containment and the other credentials need none.
func TestStepUp(t *testing.T) {
	f, idp := setupStepUp(t)
	f.entitle(t, "oidc", idp.URL, "alice", "", &f.a, RoleAdmin)
	f.entitle(t, "oidc", f.iss, "carol", "", &f.a, RoleAdmin)
	now, n := time.Now().Unix(), 0
	put := func(tok string) reply {
		t.Helper()
		n++
		return f.rest(t, "PUT", "/v1/secrets/s-"+strconv.Itoa(n), tok, map[string]any{"value": base64.StdEncoding.EncodeToString([]byte(`{"k": "v"}`))})
	}
	for name, claims := range map[string]map[string]any{
		"no acr":         {"sub": "alice", "auth_time": now},
		"a weak acr":     {"sub": "alice", "acr": "1", "auth_time": now},
		"no auth_time":   {"sub": "alice", "acr": "2"},
		"an old login":   {"sub": "alice", "acr": "2", "auth_time": now - 11*60},
		"a future login": {"sub": "alice", "acr": "2", "auth_time": now + 3600},
	} {
		r := put(idp.Sign(claims))
		if r.code != http.StatusUnauthorized || r.body["code"] != "step_up_required" {
			t.Errorf("%s: %d %v", name, r.code, r.body)
			continue
		}
		if h := r.header.Get("WWW-Authenticate"); !strings.Contains(h, `error="insufficient_user_authentication"`) ||
			!strings.Contains(h, `acr_values="2 3"`) || !strings.Contains(h, `max_age="600"`) {
			t.Errorf("%s: WWW-Authenticate %q", name, h)
		}
	}
	if r := put(idp.Sign(map[string]any{"sub": "alice", "acr": "3", "auth_time": now - 60})); r.code != http.StatusOK {
		t.Errorf("a strong, recent login: %d %v", r.code, r.body)
	}

	// containment is never delayed, and reads need nothing
	weak := idp.Sign(map[string]any{"sub": "alice", "acr": "1", "auth_time": now})
	for _, c := range []struct{ method, path string }{
		{"POST", "/v1/admin/keys/aaaaaaaaaaaa:revoke"},
		{"POST", "/v1/admin/serviceaccounts/nobody:disable"},
		{"DELETE", "/v1/admin/serviceaccounts/nobody"},
	} {
		if r := f.rest(t, c.method, c.path, weak, map[string]any{}); r.code != http.StatusNotFound {
			t.Errorf("%s %s with a weak login: %d %v", c.method, c.path, r.code, r.body)
		}
	}
	if r := f.rest(t, "GET", "/v1/secrets", weak, nil); r.code != http.StatusOK {
		t.Errorf("list secrets with a weak login: %d", r.code)
	}

	// another issuer without step_up, and API keys, are not asked
	if r := put(f.token(t, "carol", nil)); r.code != http.StatusOK {
		t.Errorf("an issuer without step_up: %d %v", r.code, r.body)
	}
	if r := put(f.keys["a-admin"]); r.code != http.StatusOK {
		t.Errorf("an API key: %d %v", r.code, r.body)
	}
}

// Every method that changes a tenant's configuration is sensitive, but
// those that end an access (veho-authentication.md §5).
func TestSensitiveMethods(t *testing.T) {
	notSensitive := map[string]bool{"RevokeKey": true, "DisableServiceAccount": true, "DeleteServiceAccount": true, "DeleteEntitlement": true}
	changes := func(m string) bool {
		for _, p := range []string{"Create", "Update", "Delete", "Put", "Set", "Rotate", "Enable"} {
			if strings.HasPrefix(m, p) {
				return true
			}
		}
		return false
	}
	for _, sd := range []grpc.ServiceDesc{vehov1.EndpointService_ServiceDesc, vehov1.SecretService_ServiceDesc, vehov1.WebhookService_ServiceDesc,
		vehov1.TenantService_ServiceDesc, vehov1.AdminService_ServiceDesc} {
		for _, m := range sd.Methods {
			full := "/" + sd.ServiceName + "/" + m.MethodName
			if want := changes(m.MethodName) && !notSensitive[m.MethodName]; sensitive[full] != want {
				t.Errorf("%s: sensitive %v, want %v", full, sensitive[full], want)
			}
		}
	}
	if !sensitive["/veho.v1.PolicyService/SetGrants"] {
		t.Error("SetGrants is not sensitive")
	}
}

// WhoAmI: an OIDC identity needs no tenant and sees all its roles; an API
// key sees its own; a SPIFFE identity must name a tenant.
func TestWhoAmI(t *testing.T) {
	f, idp := setupStepUp(t)
	f.entitle(t, "oidc", idp.URL, "alice", "", &f.a, RoleAdmin)
	f.entitle(t, "oidc", idp.URL, "", "ops", &f.b, RoleViewer)
	f.entitle(t, "oidc", idp.URL, "alice", "", nil, RolePlatformAdmin)
	a, b := f.tenantName(t, f.a), f.tenantName(t, f.b)
	f.tenant = ""
	r := f.rest(t, "GET", "/v1/whoami", idp.Sign(map[string]any{"sub": "alice", "groups": []string{"ops"}}), nil)
	if r.code != http.StatusOK || r.body["name"] != "oidc:"+idp.URL+"#alice" || r.body["kind"] != "oidc" || r.body["subject"] != "alice" {
		t.Fatalf("whoami: %d %v", r.code, r.body)
	}
	got := map[string]string{}
	for _, tr := range r.body["tenants"].([]any) {
		got[tr.(map[string]any)["tenant"].(string)] = tr.(map[string]any)["role"].(string)
	}
	if len(got) != 3 || got[a] != RoleAdmin || got[b] != RoleViewer || got["platform"] != RolePlatformAdmin {
		t.Errorf("tenants: %v", got)
	}
	if r := f.rest(t, "GET", "/v1/whoami", idp.Sign(map[string]any{"sub": "nobody"}), nil); r.code != http.StatusOK || r.body["tenants"] != nil {
		t.Errorf("an identity entitled nowhere: %d %v", r.code, r.body)
	}
	k := f.rest(t, "GET", "/v1/whoami", f.keys["a-operator"], nil)
	if k.code != http.StatusOK || k.body["kind"] != "api_key" || len(k.body["tenants"].([]any)) != 1 ||
		k.body["tenants"].([]any)[0].(map[string]any)["role"] != RoleOperator {
		t.Errorf("an API key: %d %v", k.code, k.body)
	}
	svid := f.svidKey.sign(t, map[string]any{"sub": "spiffe://" + td + "/w", "aud": audience, "exp": time.Now().Add(time.Hour).Unix()})
	if r := f.rest(t, "GET", "/v1/whoami", svid, nil); r.code != http.StatusBadRequest || r.body["code"] != "tenant_required" {
		t.Errorf("SPIFFE without a tenant: %d %v", r.code, r.body)
	}
	if r := f.rest(t, "GET", "/v1/whoami", "", nil); r.code != http.StatusUnauthorized {
		t.Errorf("no credential: %d", r.code)
	}
}

// GetConfig needs no credential and offers only the issuers with a CLI
// client.
func TestGetConfig(t *testing.T) {
	f, idp := setupStepUp(t)
	r := f.rest(t, "GET", "/v1/auth/config", "", nil)
	if r.code != http.StatusOK {
		t.Fatalf("config: %d %v", r.code, r.body)
	}
	is := r.body["issuers"].([]any)
	if len(is) != 1 {
		t.Fatalf("issuers: %v", is)
	}
	i := is[0].(map[string]any)
	if i["name"] != "corp" || i["issuer"] != idp.URL || i["cli_client_id"] != "veho-cli" || len(i["scopes"].([]any)) != 2 {
		t.Errorf("issuer: %v", i)
	}
	conn, err := grpc.NewClient(f.addr, grpc.WithTransportCredentials(credentials.NewTLS(f.tlsConfig())))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if c, err := vehov1.NewAuthServiceClient(conn).GetConfig(t.Context(), &vehov1.GetConfigRequest{}); err != nil || len(c.GetIssuers()) != 1 {
		t.Errorf("over gRPC: %v, %v", c, err)
	}
}
