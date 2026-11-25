package nexus

import (
	"bytes"
	"context"
	"crypto/sha1" //nolint:gosec // Nexus's ETag
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dihedron/veho/pkg/plugin"
	"github.com/dihedron/veho/pkg/plugin/conformance"
)

// fakeNexus serves repositories and their assets as Nexus 3 does: content
// under /repository/<repo>/, the repository's format and type under the
// REST API, ETags from the SHA-1, uploads with Basic credentials.
type fakeNexus struct {
	srv *httptest.Server

	mu       sync.Mutex
	repos    map[string]repository
	noDeploy map[string]bool // repositories that refuse redeploying
	assets   map[string][]byte
	ranges   bool
	corrupt  bool
}

func newFake(t *testing.T) *fakeNexus {
	t.Helper()
	f := &fakeNexus{ranges: true, assets: map[string][]byte{}, noDeploy: map[string]bool{"once": true},
		repos: map[string]repository{
			"files":    {Name: "files", Format: "raw", Type: "hosted"},
			"once":     {Name: "once", Format: "raw", Type: "hosted"},
			"releases": {Name: "releases", Format: "maven2", Type: "hosted"},
			"central":  {Name: "central", Format: "maven2", Type: "proxy"},
		}}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func etag(b []byte) string {
	s := sha1.Sum(b) //nolint:gosec // Nexus's ETag
	return `"{SHA1{` + hex.EncodeToString(s[:]) + `}}"`
}

func (f *fakeNexus) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	user, pass, authed := r.BasicAuth()
	if authed && (user != "veho" || pass != "secret") {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if name, ok := strings.CutPrefix(r.URL.Path, "/service/rest/v1/repositories/"); ok {
		repo, found := f.repos[name]
		if !found {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(repo)
		return
	}
	path, ok := strings.CutPrefix(r.URL.Path, "/repository/")
	repoName, _, _ := strings.Cut(path, "/")
	if _, known := f.repos[repoName]; !ok || !known {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	switch r.Method {
	case http.MethodPut:
		if !authed {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if _, there := f.assets[path]; there && f.noDeploy[repoName] {
			http.Error(w, "Repository does not allow updating assets", http.StatusBadRequest)
			return
		}
		f.mu.Unlock()
		b, err := io.ReadAll(r.Body)
		f.mu.Lock()
		if err != nil {
			return // an unfinished request leaves nothing
		}
		if f.corrupt {
			b = append(b, '!')
		}
		f.assets[path] = b
		w.WriteHeader(http.StatusCreated)
	case http.MethodDelete:
		if _, there := f.assets[path]; !there {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		delete(f.assets, path)
		w.WriteHeader(http.StatusNoContent)
	case http.MethodHead, http.MethodGet:
		b, there := f.assets[path]
		if !there {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("ETag", etag(b))
		if rng := r.Header.Get("Range"); rng != "" && f.ranges && r.Method == http.MethodGet {
			var a, z int64
			if _, err := fmt.Sscanf(rng, "bytes=%d-%d", &a, &z); err != nil || a >= int64(len(b)) {
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return
			}
			z = min(z, int64(len(b))-1)
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", a, z, len(b)))
			w.Header().Set("Content-Length", fmt.Sprint(z-a+1))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(b[a : z+1])
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(b)))
		if r.Method == http.MethodGet {
			_, _ = w.Write(b)
		}
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (f *fakeNexus) config(repo, extra string) plugin.Config {
	ca, _ := json.Marshal(string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.srv.Certificate().Raw})))
	return plugin.Config(`{"url": "` + f.srv.URL + `", "repository": "` + repo + `", "ca_cert": ` + string(ca) + extra + `}`)
}

var good = plugin.Credentials{"username": "veho", "password": "secret"}

func driver(t *testing.T) plugin.Driver {
	t.Helper()
	d, err := Factory(json.RawMessage(`{"allow_cidrs": ["127.0.0.1/32"]}`))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func open(t *testing.T, f *fakeNexus, repo, extra string, creds plugin.Credentials) plugin.Client {
	t.Helper()
	c, err := driver(t).Open(context.Background(), f.config(repo, extra), creds)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// NX-T4 on the fake: the suite as source and sink, without a format and
// with format raw.
func TestConformance(t *testing.T) {
	for _, extra := range []string{"", `, "format": "raw"`} {
		f := newFake(t)
		conformance.Run(t, conformance.Target{
			Driver: driver(t), Config: f.config("files", extra), Credentials: good,
			BadCredentials: plugin.Credentials{"username": "veho", "password": "wrong"},
			PartSize:       1 << 20, Confined: true,
		})
	}
}

// NX-T1: the formats of §2.1 and the endpoint's checks.
func TestOpen(t *testing.T) {
	f := newFake(t)
	d := driver(t)
	ctx := context.Background()
	for name, tc := range map[string]struct {
		repo, extra string
		creds       plugin.Credentials
		ok          bool
	}{
		"no format":            {"files", "", good, true},
		"raw":                  {"files", `, "format": "raw"`, good, true},
		"anonymous":            {"central", "", nil, true},
		"raw on maven":         {"releases", `, "format": "raw"`, good, false},
		"maven not yet":        {"releases", `, "format": "maven2"`, good, false},
		"npm not yet":          {"files", `, "format": "npm"`, good, false},
		"pypi not yet":         {"files", `, "format": "pypi"`, good, false},
		"unknown format":       {"files", `, "format": "docker"`, good, false},
		"no repository":        {"nope", `, "format": "raw"`, good, false},
		"bad repository":       {"a/b", "", good, false},
		"unknown key":          {"files", `, "x": 1`, good, false},
		"half the credentials": {"files", "", plugin.Credentials{"username": "veho"}, false},
		"unknown credential":   {"files", "", plugin.Credentials{"token": "t"}, false},
	} {
		_, err := d.Open(ctx, f.config(tc.repo, tc.extra), tc.creds)
		if (err == nil) != tc.ok || err != nil && plugin.ClassOf(err) != plugin.Permanent {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := d.Open(ctx, plugin.Config(`{"url": "http://nexus.example", "repository": "files"}`), good); plugin.ClassOf(err) != plugin.Permanent {
		t.Errorf("http without plaintext: %v", err)
	}
	plain, _ := Factory(json.RawMessage(`{"plaintext": true}`))
	if _, err := plain.Open(ctx, plugin.Config(`{"url": "http://nexus.example/nexus", "repository": "files"}`), good); err != nil {
		t.Errorf("http with plaintext: %v", err)
	}
	for _, s := range []string{`{"nope": 1}`, `{"allow_cidrs": ["x"]}`} {
		if _, err := Factory(json.RawMessage(s)); err == nil {
			t.Errorf("settings %s accepted", s)
		}
	}
	var schema map[string]any
	if err := json.Unmarshal(d.ConfigSchema(), &schema); err != nil {
		t.Fatal(err)
	}
	for raw, ok := range map[string]bool{"nexus://files/a/b.iso": true, "nexus://files/../x": false, "nexus:///x": false, "s3://b/k": false, "nexus://files/a?b": false} {
		if _, err := d.ParseRef(raw); (err == nil) != ok {
			t.Errorf("%s: %v", raw, err)
		}
	}
}

func put(t *testing.T, c plugin.Client, key string, data []byte) (plugin.ObjectInfo, error) {
	t.Helper()
	ctx := context.Background()
	u, err := c.Begin(ctx, plugin.Ref{Key: key}, plugin.ObjectInfo{Size: int64(len(data))}, plugin.PutOptions{})
	if err != nil {
		return plugin.ObjectInfo{}, err
	}
	p, err := u.WritePart(ctx, 0, bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return plugin.ObjectInfo{}, err
	}
	return u.Commit(ctx, []plugin.PartResult{p})
}

// NX-T2: the version from the ETag, a changed asset SourceChanged, a Nexus
// without ranges refused, a URL only for an anonymous endpoint.
func TestSource(t *testing.T) {
	f := newFake(t)
	c := open(t, f, "files", "", good)
	ctx := context.Background()
	info, err := put(t, c, "dir/img", []byte("0123456789"))
	if err != nil || info.Size != 10 || info.Version != etag([]byte("0123456789")) {
		t.Fatalf("put: %+v, %v", info, err)
	}
	r, err := c.ReadRange(ctx, plugin.Ref{Key: "dir/img"}, 2, 3, info.Version)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r)
	_ = r.Close()
	if string(b) != "234" {
		t.Fatalf("range %q", b)
	}
	if _, err := put(t, c, "dir/img", []byte("other data")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ReadRange(ctx, plugin.Ref{Key: "dir/img"}, 0, 1, info.Version); plugin.ClassOf(err) != plugin.SourceChanged {
		t.Fatalf("changed: %v", err)
	}
	if _, err := c.ReadRange(ctx, plugin.Ref{Key: "dir/img"}, 0, 0, info.Version); plugin.ClassOf(err) != plugin.SourceChanged {
		t.Fatalf("changed, empty range: %v", err)
	}
	if err := c.Delete(ctx, plugin.Ref{Key: "dir/img"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ReadRange(ctx, plugin.Ref{Key: "dir/img"}, 0, 1, info.Version); plugin.ClassOf(err) != plugin.SourceChanged {
		t.Fatalf("gone: %v", err)
	}
	if _, err := put(t, c, "x", []byte("abc")); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.ranges = false
	f.mu.Unlock()
	if _, err := c.Stat(ctx, plugin.Ref{Key: "x"}); plugin.ClassOf(err) != plugin.Permanent || !strings.Contains(err.Error(), "ranges") {
		t.Fatalf("no ranges: %v", err)
	}
	if _, err := c.(plugin.Presigner).PresignRead(ctx, plugin.Ref{Key: "x"}, "", time.Hour); plugin.ClassOf(err) != plugin.Permanent {
		t.Fatalf("presign with credentials: %v", err)
	}
	anon := open(t, f, "files", "", nil)
	u, err := anon.(plugin.Presigner).PresignRead(ctx, plugin.Ref{Key: "a b/x"}, "", time.Hour)
	if err != nil || u != f.srv.URL+"/repository/files/a%20b/x" {
		t.Fatalf("presign: %s, %v", u, err)
	}
	if _, err := anon.Stat(ctx, plugin.Ref{Container: "other", Key: "x"}); plugin.ClassOf(err) != plugin.Permanent {
		t.Fatalf("another repository: %v", err)
	}
}

// NX-T3: the SHA-1 compared, a refused redeploy, a destination that is not
// raw and hosted, or without credentials.
func TestDestination(t *testing.T) {
	f := newFake(t)
	ctx := context.Background()
	once := open(t, f, "once", "", good)
	if _, err := put(t, once, "k", []byte("first")); err != nil {
		t.Fatal(err)
	}
	if _, err := put(t, once, "k", []byte("second")); plugin.ClassOf(err) != plugin.Permanent || !strings.Contains(err.Error(), "redeploy") {
		t.Fatalf("redeploy: %v", err)
	}
	for name, c := range map[string]plugin.Client{
		"maven":     open(t, f, "releases", "", good),
		"proxy":     open(t, f, "central", "", good),
		"anonymous": open(t, f, "files", "", nil),
	} {
		if _, err := c.Begin(ctx, plugin.Ref{Key: "k"}, plugin.ObjectInfo{}, plugin.PutOptions{}); plugin.ClassOf(err) != plugin.Permanent {
			t.Errorf("%s: %v", name, err)
		}
	}
	f.mu.Lock()
	f.corrupt = true
	f.mu.Unlock()
	if _, err := put(t, open(t, f, "files", "", good), "bad", []byte("payload")); plugin.ClassOf(err) != plugin.Integrity {
		t.Fatalf("corrupt: %v", err)
	}
	c := open(t, f, "files", "", good)
	if _, err := c.Resume(ctx, plugin.Ref{Key: "k"}, []byte(`{}`)); plugin.ClassOf(err) != plugin.ResumeInvalid {
		t.Fatalf("resume: %v", err)
	}
	if err := c.Delete(ctx, plugin.Ref{Key: "never"}); err != nil {
		t.Fatalf("delete of nothing: %v", err)
	}
	if err := c.Delete(ctx, plugin.Ref{Key: "../x"}); plugin.ClassOf(err) != plugin.Permanent {
		t.Fatalf("delete of a bad key: %v", err)
	}
	if _, err := open(t, f, "files", "", plugin.Credentials{"username": "veho", "password": "wrong"}).Stat(ctx, plugin.Ref{Key: "k"}); plugin.ClassOf(err) != plugin.Permanent {
		t.Fatalf("wrong password: %v", err)
	}
	if _, err := c.Stat(ctx, plugin.Ref{Key: "missing"}); !errors.Is(err, plugin.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

// The guard refuses Nexus on an address the settings do not open.
func TestGuard(t *testing.T) {
	f := newFake(t)
	d, _ := Factory(nil)
	c, err := d.Open(context.Background(), f.config("files", ""), good)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Stat(context.Background(), plugin.Ref{Key: "x"}); plugin.ClassOf(err) != plugin.Permanent || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("got %v", err)
	}
}

// OS-T8's equivalent: a parsed key never leaves the repository.
func FuzzParseRef(f *testing.F) {
	for _, s := range []string{"nexus://r/a/b", "nexus://r/..", "nexus://r/%2e%2e/x", "nexus://r/a%2Fb", "nexus://r/a?b"} {
		f.Add(s)
	}
	d, _ := Factory(nil)
	f.Fuzz(func(t *testing.T, raw string) {
		ref, err := d.ParseRef(raw)
		if err != nil {
			return
		}
		for _, seg := range strings.Split(ref.Key, "/") {
			if seg == "" || seg == "." || seg == ".." {
				t.Fatalf("%q: segment %q", raw, seg)
			}
		}
	})
}
