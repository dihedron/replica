package http

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	nethttp "net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dihedron/veho/internal/netguard"
	"github.com/dihedron/veho/pkg/plugin"
	"github.com/dihedron/veho/pkg/plugin/conformance"
)

// object is what the test server serves at a path.
type object struct {
	data []byte
	etag string
	mod  time.Time
}

// server is an HTTPS server of objects under /pub/, served by
// http.ServeContent (ranges, conditional requests), with a bearer token.
type server struct {
	*httptest.Server
	mu      sync.Mutex
	objects map[string]object
	n       int
	// etag decides the validators: "strong" (default), "weak", "none" (only
	// Last-Modified)
	etag string
	// wrap, when set, replaces the handler for one test
	wrap func(w nethttp.ResponseWriter, r *nethttp.Request, serve func())
	// auth is the last Authorization header received
	auth string
}

func newServer(t *testing.T) *server {
	s := &server{objects: map[string]object{}, etag: "strong"}
	s.Server = httptest.NewTLSServer(nethttp.HandlerFunc(s.handle))
	t.Cleanup(s.Close)
	return s
}

func (s *server) handle(w nethttp.ResponseWriter, r *nethttp.Request) {
	s.mu.Lock()
	s.auth = r.Header.Get("Authorization")
	o, ok := s.objects[r.URL.Path]
	etag, wrap := s.etag, s.wrap
	s.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer good" {
		nethttp.Error(w, "no", nethttp.StatusUnauthorized)
		return
	}
	serve := func() {
		if !ok {
			nethttp.NotFound(w, r)
			return
		}
		switch etag {
		case "strong":
			w.Header().Set("ETag", o.etag)
		case "weak":
			w.Header().Set("ETag", "W/"+o.etag)
		}
		mod := o.mod
		if etag == "weak" {
			mod = time.Time{} // no Last-Modified either
		}
		nethttp.ServeContent(w, r, "", mod, bytes.NewReader(o.data))
	}
	if wrap != nil {
		wrap(w, r, serve)
		return
	}
	serve()
}

// put stores data at /pub/<key>, with a new ETag and a later time.
func (s *server) put(_ *testing.T, ref plugin.Ref, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n++
	s.objects["/pub/"+ref.Key] = object{data: data, etag: fmt.Sprintf(`"v%d"`, s.n), mod: time.Unix(1_700_000_000+int64(s.n), 0).UTC()}
}

// driver is the driver with the loopback allowed and the server's
// certificate trusted.
func (s *server) driver(t *testing.T) Driver {
	t.Helper()
	g, err := netguard.New([]string{"127.0.0.0/8", "::1/128"})
	if err != nil {
		t.Fatal(err)
	}
	return Driver{guard: g, tlsConfig: s.Client().Transport.(*nethttp.Transport).TLSClientConfig}
}

func (s *server) config(extra string) plugin.Config {
	return plugin.Config(`{"base_url": "` + s.URL + `/pub/"` + extra + `}`)
}

var good = plugin.Credentials{"bearer_token": "good"}

func (s *server) open(t *testing.T) plugin.Client {
	t.Helper()
	c, err := s.driver(t).Open(context.Background(), s.config(""), good)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// TestConformance: SRC-T7, the suite as a source, with confinement.
func TestConformance(t *testing.T) {
	for _, etag := range []string{"strong", "none"} {
		t.Run(etag, func(t *testing.T) {
			s := newServer(t)
			s.etag = etag
			conformance.Run(t, conformance.Target{Driver: s.driver(t), Config: s.config(""), Credentials: good,
				BadCredentials: plugin.Credentials{"bearer_token": "bad"}, Put: s.put, Confined: true, PartSize: 64 << 10})
		})
	}
}

// TestVersion: SRC-T3, the version is the strong ETag, else Last-Modified
// with the size; a weak ETag alone is no version.
func TestVersion(t *testing.T) {
	s := newServer(t)
	ref := plugin.Ref{Key: "a.img"}
	s.put(t, ref, []byte("hello"))
	c := s.open(t)
	info, err := c.Stat(context.Background(), ref)
	if err != nil || info.Size != 5 || info.Version != `etag:"v1"` {
		t.Errorf("strong ETag: %+v %v", info, err)
	}
	s.etag = "none"
	if info, err = c.Stat(context.Background(), ref); err != nil || info.Version != "lm:Tue, 14 Nov 2023 22:13:21 GMT:5" {
		t.Errorf("Last-Modified: %+v %v", info, err)
	}
	s.etag = "weak"
	_, err = c.Stat(context.Background(), ref)
	wantClass(t, "a weak ETag alone", err, plugin.Permanent)
}

func wantClass(t *testing.T, what string, err error, class plugin.Class) {
	t.Helper()
	if err == nil {
		t.Errorf("%s: no error, want %v", what, class)
	} else if plugin.ClassOf(err) != class {
		t.Errorf("%s: %v (%v), want %v", what, err, plugin.ClassOf(err), class)
	}
}

// TestRefused: SRC-T3, what the driver refuses from a server.
func TestRefused(t *testing.T) {
	s := newServer(t)
	ref := plugin.Ref{Key: "a.img"}
	s.put(t, ref, []byte("0123456789"))
	c := s.open(t)
	ctx := context.Background()
	read := func(off, n int64, version string) error {
		r, err := c.ReadRange(ctx, ref, off, n, version)
		if err != nil {
			return err
		}
		defer func() { _ = r.Close() }()
		_, err = io.ReadAll(r)
		return err
	}
	info, err := c.Stat(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name  string
		wrap  func(w nethttp.ResponseWriter, r *nethttp.Request, serve func())
		stat  bool // the Stat fails, else the read
		class plugin.Class
	}{
		{"no Accept-Ranges", func(w nethttp.ResponseWriter, r *nethttp.Request, _ func()) {
			w.Header().Set("ETag", `"v1"`)
			w.Header().Set("Content-Length", "10")
		}, true, plugin.Permanent},
		{"no Content-Length", func(w nethttp.ResponseWriter, r *nethttp.Request, _ func()) {
			w.Header().Set("ETag", `"v1"`)
			w.Header().Set("Accept-Ranges", "bytes")
			w.Header().Set("Transfer-Encoding", "chunked")
			w.WriteHeader(nethttp.StatusOK)
		}, true, plugin.Permanent},
		{"a redirect", func(w nethttp.ResponseWriter, r *nethttp.Request, _ func()) {
			nethttp.Redirect(w, r, "https://elsewhere.example/x", nethttp.StatusFound)
		}, true, plugin.Permanent},
		{"a server error", func(w nethttp.ResponseWriter, r *nethttp.Request, _ func()) {
			w.WriteHeader(nethttp.StatusServiceUnavailable)
		}, true, plugin.Transient},
		{"200 instead of 206", func(w nethttp.ResponseWriter, r *nethttp.Request, serve func()) {
			r.Header.Del("Range")
			serve()
		}, false, plugin.Permanent},
		{"another Content-Range", func(w nethttp.ResponseWriter, r *nethttp.Request, serve func()) {
			r.Header.Set("Range", "bytes=0-3")
			serve()
		}, false, plugin.Permanent},
		{"fewer bytes than the range", func(w nethttp.ResponseWriter, r *nethttp.Request, _ func()) {
			w.Header().Set("ETag", `"v1"`)
			w.Header().Set("Content-Range", "bytes 2-5/10")
			w.Header().Set("Content-Length", "4")
			w.WriteHeader(nethttp.StatusPartialContent)
			_, _ = w.Write([]byte("23"))
		}, false, plugin.Transient},
		{"a changed validator, the condition ignored", func(w nethttp.ResponseWriter, r *nethttp.Request, serve func()) {
			r.Header.Del("If-Match")
			w.Header().Set("ETag", `"v9"`)
			o := s.objects["/pub/a.img"]
			nethttp.ServeContent(w, r, "", o.mod, bytes.NewReader(o.data))
		}, false, plugin.SourceChanged},
	} {
		s.mu.Lock()
		s.wrap = c.wrap
		s.mu.Unlock()
		if c.stat {
			_, err = c2stat(ctx, s, ref)
		} else {
			err = read(2, 4, info.Version)
		}
		wantClass(t, c.name, err, c.class)
	}
	s.mu.Lock()
	s.wrap = nil
	s.mu.Unlock()
	if err := read(2, 4, info.Version); err != nil {
		t.Fatalf("the read back to normal: %v", err)
	}
	// SourceChanged through 412, after a new version
	s.put(t, ref, []byte("9876543210"))
	wantClass(t, "a new version", read(2, 4, info.Version), plugin.SourceChanged)
}

func c2stat(ctx context.Context, s *server, ref plugin.Ref) (plugin.ObjectInfo, error) {
	c, err := s.driver(&testing.T{}).Open(ctx, s.config(""), good)
	if err != nil {
		return plugin.ObjectInfo{}, err
	}
	defer func() { _ = c.Close() }()
	return c.Stat(ctx, ref)
}

// TestOpen: SRC-T3 and SRC-W6, the configurations and credentials refused.
func TestOpen(t *testing.T) {
	s := newServer(t)
	d := s.driver(t)
	ctx := context.Background()
	for _, cfg := range []string{
		`{}`, `{"base_url": "ftp://x/"}`, `{"base_url": "https://user:pw@x/"}`, `{"base_url": "https://x/?q=1"}`,
		`{"base_url": "https://x/#f"}`, `{"base_url": "https:///pub/"}`, `{"base_url": "https://x/", "bogus": 1}`,
		`{"base_url": "https://x/", "headers": {"Authorization": "Bearer x"}}`,
		`{"base_url": "https://x/", "headers": {"range": "bytes=0-1"}}`,
		`{"base_url": "https://x/", "headers": {"If-Match": "*"}}`,
		`{"base_url": "https://x/", "headers": {"Host": "y"}}`,
		`{"base_url": "https://x/", "headers": {"X-Bad": "a\nb"}}`,
	} {
		if c, err := d.Open(ctx, plugin.Config(cfg), nil); err == nil {
			_ = c.Close()
			t.Errorf("%s: accepted", cfg)
		} else if plugin.ClassOf(err) != plugin.Permanent {
			t.Errorf("%s: %v, want Permanent", cfg, err)
		}
	}
	for _, creds := range []plugin.Credentials{{"username": "u"}, {"bearer_token": "t", "username": "u", "password": "p"}, {"nope": "x"}} {
		if _, err := d.Open(ctx, s.config(""), creds); err == nil {
			t.Errorf("credentials %v: accepted", creds)
		}
	}
	// no credentials over plain HTTP (SRC-W2)
	if _, err := d.Open(ctx, plugin.Config(`{"base_url": "http://x/"}`), good); err == nil {
		t.Error("credentials over plain HTTP: accepted")
	}
	if c, err := d.Open(ctx, plugin.Config(`{"base_url": "http://x/"}`), nil); err != nil {
		t.Errorf("plain HTTP without credentials: %v", err)
	} else {
		_ = c.Close()
	}
	// basic authentication, and the configured headers, reach the server
	c, err := d.Open(ctx, s.config(`, "headers": {"Accept": "*/*"}`), plugin.Credentials{"username": "u", "password": "p"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_, _ = c.Stat(ctx, plugin.Ref{Key: "x"})
	if s.auth != "Basic dTpw" {
		t.Errorf("basic authentication: %q", s.auth)
	}
}

// TestGuard: SRC-3, without allow_cidrs the loopback is refused before
// anything is sent.
func TestGuard(t *testing.T) {
	s := newServer(t)
	d, err := Factory(json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	c, err := d.Open(context.Background(), s.config(""), good)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_, err = c.Stat(context.Background(), plugin.Ref{Key: "x"})
	if !errors.Is(err, netguard.ErrForbidden) || plugin.ClassOf(err) != plugin.Permanent {
		t.Errorf("the loopback: %v, want ErrForbidden, Permanent", err)
	}
	if s.auth != "" {
		t.Error("the server was reached")
	}
}

// TestFactory: the settings, allow_cidrs only.
func TestFactory(t *testing.T) {
	if d, err := Factory(json.RawMessage(`{"allow_cidrs": ["10.0.0.0/8"]}`)); err != nil || d.Name() != "http" {
		t.Errorf("allow_cidrs: %v %v", d, err)
	}
	for _, bad := range []string{`{"allow_cidrs": ["nonsense"]}`, `{"plaintext": true}`} {
		if _, err := Factory(json.RawMessage(bad)); err == nil {
			t.Errorf("%s: accepted", bad)
		}
	}
}

func TestParseRef(t *testing.T) {
	d := Driver{}
	if ref, err := d.ParseRef("https://images.example/pub/a/b.img"); err != nil || ref.Key != "pub/a/b.img" {
		t.Errorf("%+v %v", ref, err)
	}
	for _, bad := range []string{"ftp://x/a", "https://x/", "https://x/a/../b", "nonsense"} {
		if _, err := d.ParseRef(bad); err == nil {
			t.Errorf("%q: accepted", bad)
		}
	}
}

// FuzzLocate: SRC-T9, a key either is refused or names a URL on the base
// URL's host, under its path, with no dot segment.
func FuzzLocate(f *testing.F) {
	for _, k := range []string{"a.img", "a/b/c", "../x", "%2e%2e/x", "a//b", "//evil.example/x", "a\\..\\x", "a?b#c", ".", "a/./b"} {
		f.Add(k)
	}
	base, _ := url.Parse("https://images.example/pub/")
	f.Fuzz(func(t *testing.T, key string) {
		u, err := locate(base, key)
		if err != nil {
			return
		}
		got, err := url.Parse(u)
		if err != nil {
			t.Fatalf("%q: %q does not parse: %v", key, u, err)
		}
		if got.Host != base.Host || got.Scheme != base.Scheme || got.RawQuery != "" || got.Fragment != "" || !strings.HasPrefix(got.Path, base.Path) || got.Path == base.Path {
			t.Fatalf("%q: %q leaves the base URL", key, u)
		}
		for _, seg := range strings.Split(strings.TrimPrefix(got.Path, base.Path), "/") {
			if seg == "" || seg == "." || seg == ".." {
				t.Fatalf("%q: %q has the segment %q", key, u, seg)
			}
		}
	})
}

var _ = tls.VersionTLS12
