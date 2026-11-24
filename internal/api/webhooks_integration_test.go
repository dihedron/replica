//go:build integration

package api

import (
	"net/http"
	"strings"
	"testing"
)

// WH: the webhook API over REST: the admin role, the secret shown once, the
// URL rules, tenancy, and the reserved secret names.
func TestWebhooks(t *testing.T) {
	f := setup(t, Options{})
	admin := f.keys["a-admin"]
	hook := map[string]any{"name": "ci", "url": "https://hooks.example.com/veho", "events": []string{"job.succeeded", "job.failed"}}
	for _, who := range []string{"a-viewer", "a-operator"} {
		if r := f.rest(t, "POST", "/v1/webhooks", f.keys[who], hook); r.code != http.StatusForbidden {
			t.Errorf("%s creates: %d", who, r.code)
		}
		if r := f.rest(t, "GET", "/v1/webhooks", f.keys[who], nil); r.code != http.StatusForbidden {
			t.Errorf("%s lists: %d", who, r.code)
		}
	}
	r := f.rest(t, "POST", "/v1/webhooks", admin, hook)
	if r.code != http.StatusOK {
		t.Fatalf("create: %d %v", r.code, r.body)
	}
	secret, _ := r.body["secret"].(string)
	if !strings.HasPrefix(secret, "whsec_") {
		t.Fatalf("no secret in %v", r.body)
	}
	// the secret is shown once: no read-back carries it
	for _, path := range []string{"/v1/webhooks/ci", "/v1/webhooks"} {
		if g := f.rest(t, "GET", path, admin, nil); g.code != http.StatusOK || strings.Contains(toString(g.body), secret) {
			t.Errorf("GET %s: %d, the secret is read back", path, g.code)
		}
	}
	if r := f.rest(t, "POST", "/v1/webhooks", admin, hook); r.code != http.StatusConflict || r.body["code"] != "already_exists" {
		t.Errorf("duplicate: %d %v", r.code, r.body)
	}
	for _, bad := range []map[string]any{
		{"name": "x", "url": "http://hooks.example.com/", "events": []string{"job.failed"}},
		{"name": "x", "url": "https://u:p@hooks.example.com/", "events": []string{"job.failed"}},
		{"name": "x", "url": "https://hooks.example.com/", "events": []string{"job.nope"}},
		{"name": "x", "url": "https://hooks.example.com/"},
	} {
		if r := f.rest(t, "POST", "/v1/webhooks", admin, bad); r.code != http.StatusBadRequest || r.body["code"] != "invalid_request" {
			t.Errorf("%v: %d %v", bad, r.code, r.body)
		}
	}
	// update, rotate
	if r := f.rest(t, "PATCH", "/v1/webhooks/ci", admin, map[string]any{"enabled": false}); r.code != http.StatusOK || r.body["enabled"] == true {
		t.Errorf("disable: %d %v", r.code, r.body)
	}
	if r := f.rest(t, "PATCH", "/v1/webhooks/ci", admin, map[string]any{"enabled": true, "events": []string{"job.cancelled"}}); r.code != http.StatusOK || r.body["enabled"] != true {
		t.Errorf("enable: %d %v", r.code, r.body)
	}
	if r := f.rest(t, "POST", "/v1/webhooks/ci:rotate", admin, map[string]any{}); r.code != http.StatusOK || r.body["secret"] == secret || !strings.HasPrefix(toString(r.body["secret"]), "whsec_") {
		t.Errorf("rotate: %d %v", r.code, r.body)
	}
	// tenant B sees nothing of it
	badmin := f.key(t, &f.b, RoleAdmin, "b-admin")
	if g := f.rest(t, "GET", "/v1/webhooks/ci", badmin, nil); g.code != http.StatusNotFound {
		t.Errorf("another tenant gets it: %d", g.code)
	}
	if l := f.rest(t, "GET", "/v1/webhooks", badmin, nil); l.code != http.StatusOK || l.body["webhooks"] != nil {
		t.Errorf("another tenant lists: %d %v", l.code, l.body)
	}
	if d := f.rest(t, "DELETE", "/v1/webhooks/ci", badmin, nil); d.code != http.StatusNotFound {
		t.Errorf("another tenant deletes: %d", d.code)
	}
	// the webhooks' secrets are veho's own: not readable, writable, listable or usable as a credential
	if l := f.rest(t, "GET", "/v1/secrets", admin, nil); l.code != http.StatusOK || strings.Contains(toString(l.body), "webhook.") {
		t.Errorf("the secrets list shows veho's own: %v", l.body)
	}
	if p := f.rest(t, "PUT", "/v1/secrets/webhook.x", admin, map[string]string{"value": "eA=="}); p.code != http.StatusBadRequest {
		t.Errorf("put a reserved name: %d", p.code)
	}
	ep := map[string]any{"name": "steal", "driver": "file", "location": "lab-a", "config": map[string]string{"root": t.TempDir()}, "credential_ref": "db://webhook.anything"}
	if p := f.rest(t, "POST", "/v1/endpoints", admin, ep); p.code != http.StatusBadRequest {
		t.Errorf("an endpoint with a reserved credential: %d %v", p.code, p.body)
	}
	if d := f.rest(t, "DELETE", "/v1/webhooks/ci", admin, nil); d.code != http.StatusOK {
		t.Errorf("delete: %d", d.code)
	}
	if g := f.rest(t, "GET", "/v1/webhooks/ci", admin, nil); g.code != http.StatusNotFound {
		t.Errorf("get after delete: %d", g.code)
	}
}
