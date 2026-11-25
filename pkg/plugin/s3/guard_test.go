package s3

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/dihedron/veho/pkg/plugin"
)

// SRC-3 for s3 (veho-sources.md §9, point 1): a private address is
// refused before any request unless allow_cidrs lists it; a redirect is not
// followed; the metadata addresses never.
func TestGuard(t *testing.T) {
	var hits, elsewhere atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { elsewhere.Add(1) }))
	t.Cleanup(other.Close)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if strings.Contains(r.URL.Path, "moved") {
			http.Redirect(w, r, other.URL+r.URL.Path, http.StatusTemporaryRedirect)
			return
		}
		w.Header().Set("Content-Length", "3")
		w.Header().Set("ETag", `"x"`)
	}))
	t.Cleanup(srv.Close)
	cfg := plugin.Config(`{"endpoint": "` + srv.URL + `", "region": "r", "bucket": "b", "path_style": true}`)
	creds := plugin.Credentials{"access_key_id": "a", "secret_access_key": "s"}
	ctx := context.Background()

	c, err := New().Open(ctx, cfg, creds)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Stat(ctx, plugin.Ref{Key: "k"}); plugin.ClassOf(err) != plugin.Permanent || hits.Load() != 0 {
		t.Fatalf("loopback without allow_cidrs: %v, %d requests", err, hits.Load())
	}

	d, err := Factory(json.RawMessage(`{"allow_cidrs": ["127.0.0.0/8"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if c, err = d.Open(ctx, cfg, creds); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Stat(ctx, plugin.Ref{Key: "k"}); err != nil || hits.Load() != 1 {
		t.Fatalf("allowed: %v, %d requests", err, hits.Load())
	}
	if _, err := c.Stat(ctx, plugin.Ref{Key: "moved"}); err == nil || elsewhere.Load() != 0 {
		t.Fatalf("a redirect: %v, %d requests elsewhere", err, elsewhere.Load())
	}

	all, _ := Factory(json.RawMessage(`{"allow_cidrs": ["0.0.0.0/0"]}`))
	meta, err := all.Open(ctx, plugin.Config(`{"endpoint": "http://169.254.169.254", "region": "r", "bucket": "latest", "path_style": true}`), creds)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := meta.Stat(ctx, plugin.Ref{Key: "meta-data/iam/security-credentials/role"}); plugin.ClassOf(err) != plugin.Permanent || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("the metadata address: %v", err)
	}
	for _, s := range []string{`{"allow_cidrs": ["nonsense"]}`, `{"other": 1}`} {
		if _, err := Factory(json.RawMessage(s)); err == nil {
			t.Errorf("settings %s accepted", s)
		}
	}
}

// loopback is the driver allowed to the test's servers.
func loopback(t *testing.T) plugin.Driver {
	t.Helper()
	d, err := Factory(json.RawMessage(`{"allow_cidrs": ["127.0.0.0/8"]}`))
	if err != nil {
		t.Fatal(err)
	}
	return d
}
