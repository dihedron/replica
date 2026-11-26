//go:build integration

package api

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"
	"uuid"
)

// AU-T9: keys rotate under one service account, whose grants they keep; a
// fourth active key and an expiry over the maximum are refused; disabling
// the account refuses all its keys at once. Deleting it removes its grants
// with it, and a platform administrator manages platform accounts.
func TestServiceAccounts(t *testing.T) {
	f := setup(t, Options{})
	admin := f.keys["a-admin"]
	name := "ci-" + uuid.New().String()[:8]
	if r := f.rest(t, "POST", "/v1/admin/serviceaccounts", admin, map[string]any{"name": name, "role": "operator"}); r.code != http.StatusOK ||
		r.body["name"] != name || r.body["role"] != "operator" || r.body["state"] != "active" {
		t.Fatalf("create: %d %v", r.code, r.body)
	}
	if r := f.rest(t, "POST", "/v1/admin/serviceaccounts", admin, map[string]any{"name": name, "role": "viewer"}); r.code != http.StatusConflict {
		t.Errorf("the same name again: %d %v", r.code, r.body)
	}
	newKey := func(body map[string]any) reply {
		t.Helper()
		body["service_account"] = name
		return f.rest(t, "POST", "/v1/admin/keys", admin, body)
	}
	f.rest(t, "POST", "/v1/policies", admin, map[string]any{"name": "geo", "policy": geo("g/x")})
	if r := f.rest(t, "PUT", "/v1/policies/geo/grants", admin, map[string]any{"grants": []map[string]string{{"grantee": "key:" + name, "permission": "read"}}}); r.code != http.StatusOK {
		t.Fatalf("grant: %d %v", r.code, r.body)
	}
	reads := func(key string) int {
		t.Helper()
		return f.rest(t, "GET", "/v1/policies/geo", key, nil).code
	}

	// rotation: two keys of one account, the grant is the account's
	k1, k2 := newKey(map[string]any{}), newKey(map[string]any{"expires_in": strconv.Itoa(3600)})
	if k1.code != http.StatusOK || k2.code != http.StatusOK {
		t.Fatalf("keys: %d %v, %d %v", k1.code, k1.body, k2.code, k2.body)
	}
	key1, key2 := k1.body["key"].(string), k2.body["key"].(string)
	if reads(key1) != http.StatusOK || reads(key2) != http.StatusOK {
		t.Errorf("both keys read through the account's grant: %d %d", reads(key1), reads(key2))
	}
	if r := f.rest(t, "POST", "/v1/admin/keys/"+k1.body["id"].(string)+":revoke", admin, map[string]any{}); r.code != http.StatusOK {
		t.Fatalf("revoke: %d", r.code)
	}
	if reads(key1) != http.StatusUnauthorized || reads(key2) != http.StatusOK {
		t.Errorf("after revoking k1: %d %d", reads(key1), reads(key2))
	}

	// the expiry: the default when none is given, never over the maximum
	l := f.rest(t, "GET", "/v1/admin/keys?service_account="+name, admin, nil)
	for _, k := range l.body["keys"].([]any) {
		k := k.(map[string]any)
		if k["service_account"] != "key:"+name {
			t.Errorf("a key of another account in the filtered list: %v", k)
		}
		if k["id"] == k1.body["id"] {
			exp, _ := time.Parse(time.RFC3339Nano, k["expires_at"].(string))
			if d := time.Until(exp); d < 89*24*time.Hour || d > 90*24*time.Hour {
				t.Errorf("the default lifetime: %v", d)
			}
		}
	}
	if r := newKey(map[string]any{"expires_in": strconv.Itoa(91 * 24 * 3600)}); r.code != http.StatusBadRequest {
		t.Errorf("an expiry over the maximum: %d %v", r.code, r.body)
	}
	if r := newKey(map[string]any{"expires_in": "-1"}); r.code != http.StatusBadRequest {
		t.Errorf("a negative expiry: %d", r.code)
	}

	// at most 3 active keys: k2, k3, k4; the fifth is refused
	k3, k4 := newKey(map[string]any{}), newKey(map[string]any{})
	if k3.code != http.StatusOK || k4.code != http.StatusOK {
		t.Fatalf("keys 3 and 4: %d %d", k3.code, k4.code)
	}
	if r := newKey(map[string]any{}); r.code != http.StatusConflict || r.body["code"] != "too_many_keys" {
		t.Errorf("a fourth active key: %d %v", r.code, r.body)
	}
	if g := f.rest(t, "GET", "/v1/admin/serviceaccounts/"+name, admin, nil); g.code != http.StatusOK || g.body["active_keys"] != float64(3) {
		t.Errorf("show: %d %v", g.code, g.body)
	}

	// disabling refuses every key at once; enabling gives them back
	if r := f.rest(t, "POST", "/v1/admin/serviceaccounts/"+name+":disable", admin, map[string]any{}); r.code != http.StatusOK || r.body["state"] != "disabled" {
		t.Fatalf("disable: %d %v", r.code, r.body)
	}
	for _, k := range []reply{k2, k3, k4} {
		if c := reads(k.body["key"].(string)); c != http.StatusUnauthorized {
			t.Errorf("a key of a disabled account: %d", c)
		}
	}
	if r := f.rest(t, "POST", "/v1/admin/serviceaccounts/"+name+":enable", admin, map[string]any{}); r.code != http.StatusOK || reads(key2) != http.StatusOK {
		t.Errorf("enable: %d, then %d", r.code, reads(key2))
	}

	// another tenant sees nothing of it
	bAdmin := f.key(t, &f.b, RoleAdmin, "b-admin")
	if r := f.rest(t, "GET", "/v1/admin/serviceaccounts/"+name, bAdmin, nil); r.code != http.StatusNotFound {
		t.Errorf("another tenant's account: %d", r.code)
	}
	if r := f.rest(t, "POST", "/v1/admin/keys", bAdmin, map[string]any{"service_account": name}); r.code != http.StatusNotFound {
		t.Errorf("a key of another tenant's account: %d", r.code)
	}

	// deletion takes the keys and the grants; the name starts afresh
	if r := f.rest(t, "DELETE", "/v1/admin/serviceaccounts/"+name, admin, nil); r.code != http.StatusOK {
		t.Fatalf("delete: %d %v", r.code, r.body)
	}
	if c := reads(key2); c != http.StatusUnauthorized {
		t.Errorf("a key of a deleted account: %d", c)
	}
	f.rest(t, "POST", "/v1/admin/serviceaccounts", admin, map[string]any{"name": name, "role": "operator"})
	again := newKey(map[string]any{})
	if again.code != http.StatusOK {
		t.Fatalf("a key of the new account: %d %v", again.code, again.body)
	}
	if c := reads(again.body["key"].(string)); c != http.StatusNotFound {
		t.Errorf("the new account inherits the old one's grant: %d", c)
	}
	if r := f.rest(t, "DELETE", "/v1/admin/serviceaccounts/nobody", admin, nil); r.code != http.StatusNotFound {
		t.Errorf("delete an unknown account: %d", r.code)
	}
	if r := f.rest(t, "GET", "/v1/admin/serviceaccounts", f.keys["a-operator"], nil); r.code != http.StatusForbidden {
		t.Errorf("an operator lists accounts: %d", r.code)
	}
}

// AU-T9, platform scope: a platform administrator manages the platform's
// service accounts, which no tenant admin sees.
func TestPlatformServiceAccounts(t *testing.T) {
	f := setup(t, Options{})
	platform, name := f.keys["platform"], "ops-"+uuid.New().String()[:8]
	if r := f.rest(t, "POST", "/v1/admin/serviceaccounts", platform, map[string]any{"name": name}); r.code != http.StatusOK || r.body["role"] != "platform-admin" {
		t.Fatalf("create a platform account: %d %v", r.code, r.body)
	}
	if r := f.rest(t, "POST", "/v1/admin/serviceaccounts", platform, map[string]any{"name": name + "-x", "role": "admin"}); r.code != http.StatusBadRequest {
		t.Errorf("a tenant role in the platform: %d", r.code)
	}
	k := f.rest(t, "POST", "/v1/admin/keys", platform, map[string]any{"service_account": name})
	if k.code != http.StatusOK {
		t.Fatalf("a platform key: %d %v", k.code, k.body)
	}
	if r := f.rest(t, "GET", "/v1/tenants", k.body["key"].(string), nil); r.code != http.StatusOK {
		t.Errorf("the new platform key lists tenants: %d", r.code)
	}
	if r := f.rest(t, "GET", "/v1/admin/serviceaccounts/"+name, f.keys["a-admin"], nil); r.code != http.StatusNotFound {
		t.Errorf("a tenant admin sees a platform account: %d", r.code)
	}
	l := f.rest(t, "GET", "/v1/admin/serviceaccounts", platform, nil)
	for _, a := range l.body["service_accounts"].([]any) {
		if a.(map[string]any)["role"] != "platform-admin" {
			t.Errorf("a tenant's account in the platform's list: %v", a)
		}
	}
	if r := f.rest(t, "DELETE", "/v1/admin/serviceaccounts/"+name, platform, nil); r.code != http.StatusOK {
		t.Errorf("delete a platform account: %d", r.code)
	}
}

// The limit of 3 active keys holds when keys are created at once.
func TestKeyLimitConcurrent(t *testing.T) {
	f := setup(t, Options{})
	admin, name := f.keys["a-admin"], "par-"+uuid.New().String()[:8]
	f.rest(t, "POST", "/v1/admin/serviceaccounts", admin, map[string]any{"name": name, "role": "viewer"})
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() { f.rest(t, "POST", "/v1/admin/keys", admin, map[string]any{"service_account": name}) })
	}
	wg.Wait()
	var n int
	if err := f.migrator.QueryRow(context.Background(), `select count(*) from veho.api_keys k join veho.service_accounts s on s.id = k.service_account_id
		where s.name = $1 and k.revoked_at is null and k.expires_at > now()`, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("%d active keys, want 3", n)
	}
}
