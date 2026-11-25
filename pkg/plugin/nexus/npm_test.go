package nexus

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha1" //nolint:gosec // npm's shasum
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/dihedron/veho/pkg/plugin"
)

// npmRepo is the fake's npm side: packuments by name, the publish
// documents it received, and components for the REST search.
type npmRepo struct {
	packuments map[string]map[string]any
	published  []map[string]any
	evil       bool // the packument links a tarball elsewhere (NX-6)
	badSum     bool // the packument's integrity is wrong
}

// serveNPM answers the npm repository "npm"; false if r is not for it.
func (f *fakeNexus) serveNPM(w http.ResponseWriter, r *http.Request, authed bool) bool {
	if r.URL.Path == "/service/rest/v1/search" && r.URL.Query().Get("format") == "npm" {
		q := r.URL.Query()
		if f.lag > 0 { // as Nexus's search, which lists a new component later
			f.lag--
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{}})
			return true
		}
		name := q.Get("name")
		if g := q.Get("group"); g != "" {
			name = "@" + g + "/" + name
		}
		var items []map[string]string
		if p, ok := f.npm.packuments[name]; ok {
			if _, ok := p["versions"].(map[string]any)[q.Get("version")]; ok {
				items = append(items, map[string]string{"id": url.PathEscape(name + "@" + q.Get("version"))})
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
		return true
	}
	if id, ok := strings.CutPrefix(r.URL.Path, "/service/rest/v1/components/"); ok && r.Method == http.MethodDelete {
		key, _ := url.PathUnescape(id)
		at := strings.LastIndex(key, "@")
		name, version := key[:at], key[at+1:]
		p := f.npm.packuments[name]
		v := p["versions"].(map[string]any)[version].(map[string]any)
		u, _ := url.Parse(v["dist"].(map[string]any)["tarball"].(string))
		delete(f.assets, strings.TrimPrefix(u.Path, "/repository/"))
		delete(p["versions"].(map[string]any), version)
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	escaped, ok := strings.CutPrefix(r.URL.EscapedPath(), "/repository/npm/")
	if !ok || strings.Contains(escaped, "/-/") {
		return false
	}
	name, _ := url.PathUnescape(escaped)
	switch r.Method {
	case http.MethodGet:
		p, ok := f.npm.packuments[name]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return true
		}
		_ = json.NewEncoder(w).Encode(p)
	case http.MethodPut:
		if !authed {
			w.WriteHeader(http.StatusUnauthorized)
			return true
		}
		var doc map[string]any
		if err := json.NewDecoder(r.Body).Decode(&doc); err != nil || doc["name"] != name {
			w.WriteHeader(http.StatusBadRequest)
			return true
		}
		f.npm.published = append(f.npm.published, doc)
		p := f.npm.packuments[name]
		if p == nil {
			p = map[string]any{"name": name, "versions": map[string]any{}}
			f.npm.packuments[name] = p
		}
		for version, v := range doc["versions"].(map[string]any) {
			if _, there := p["versions"].(map[string]any)[version]; there && f.noDeploy["npm"] {
				w.WriteHeader(http.StatusBadRequest)
				return true
			}
			for file, a := range doc["_attachments"].(map[string]any) {
				data, _ := base64.StdEncoding.DecodeString(a.(map[string]any)["data"].(string))
				_, base, _ := strings.Cut(file, "/")
				if base == "" {
					base = file
				}
				path := "npm/" + name + "/-/" + base
				f.assets[path] = data
				tarball := f.srv.URL + "/repository/" + path
				if f.npm.evil {
					tarball = "https://evil.example/x.tgz"
				}
				s1 := sha1.Sum(data) //nolint:gosec // npm's shasum
				s512 := sha512.Sum512(data)
				if f.npm.badSum {
					s512[0] ^= 1
				}
				v.(map[string]any)["dist"] = map[string]any{"tarball": tarball, "shasum": hex.EncodeToString(s1[:]),
					"integrity": "sha512-" + base64.StdEncoding.EncodeToString(s512[:])}
			}
			p["versions"].(map[string]any)[version] = v
		}
		w.WriteHeader(http.StatusOK)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
	return true
}

// tgz is an npm tarball of these files.
func tgz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var b bytes.Buffer
	z := gzip.NewWriter(&b)
	w := tar.NewWriter(z)
	for name, body := range files {
		if err := w.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(body))
	}
	_ = w.Close()
	_ = z.Close()
	return b.Bytes()
}

func pkg(t *testing.T, name, version string) []byte {
	t.Helper()
	return tgz(t, map[string]string{
		"package/package.json": `{"name": "` + name + `", "version": "` + version + `", "description": "a test"}`,
		"package/index.js":     "module.exports = 1\n",
	})
}

// NX-T7: keys.
func TestNPMKey(t *testing.T) {
	for key, want := range map[string]npmKey{
		"left-pad@1.3.0":        {name: "left-pad", base: "left-pad", version: "1.3.0"},
		"@acme/tool@2.0.0-rc.1": {name: "@acme/tool", scope: "acme", base: "tool", version: "2.0.0-rc.1"},
		"a.b_c~d@0.0.1+build.5": {name: "a.b_c~d", base: "a.b_c~d", version: "0.0.1+build.5"},
	} {
		if got, err := parseNPM(key); err != nil || got != want {
			t.Errorf("%s: %+v, %v", key, got, err)
		}
	}
	for _, key := range []string{"", "left-pad", "left-pad@", "@1.0.0", "Left@1.0.0", ".x@1.0.0", "_x@1.0.0", "a/b@1.0.0",
		"@acme@1.0.0", "@acme/@1.0.0", "@/x@1.0.0", "a@1.0", "a@01.0.0", "a@1.0.0/x", strings.Repeat("a", 215) + "@1.0.0", "a b@1.0.0"} {
		if _, err := parseNPM(key); plugin.ClassOf(err) != plugin.Permanent {
			t.Errorf("%q accepted: %v", key, err)
		}
	}
}

func FuzzNPMKey(f *testing.F) {
	for _, s := range []string{"left-pad@1.3.0", "@acme/tool@2.0.0", "@a/../b@1.0.0"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, key string) {
		k, err := parseNPM(key)
		if err != nil {
			return
		}
		if strings.ContainsAny(k.base, "/%?#\\ ") || k.base == "." || k.base == ".." || strings.ContainsAny(k.scope, "/%?#\\ @") {
			t.Fatalf("%q: %+v", key, k)
		}
	})
}

func npmFake(t *testing.T) *fakeNexus {
	t.Helper()
	f := newFake(t)
	f.repos["npm"] = repository{Name: "npm", Format: "npm", Type: "hosted"}
	f.repos["npm-proxy"] = repository{Name: "npm-proxy", Format: "npm", Type: "proxy"}
	return f
}

// NX-T7 on the fake: a package round trip, the document, the dist-tag,
// the component deleted.
func TestNPM(t *testing.T) {
	f := npmFake(t)
	ctx := context.Background()
	c := open(t, f, "npm", `, "format": "npm", "npm_dist_tag": "next"`, good)
	data := pkg(t, "@acme/tool", "1.2.3")
	info, err := put(t, c, "@acme/tool@1.2.3", data)
	if sum := sha1.Sum(data); err != nil || info.Size != int64(len(data)) || info.Version != `"`+hex.EncodeToString(sum[:])+`"` { //nolint:gosec // the ETag
		t.Fatalf("put: %+v, %v", info, err)
	}
	doc := f.npm.published[0]
	v := doc["versions"].(map[string]any)["1.2.3"].(map[string]any)
	if doc["dist-tags"].(map[string]any)["next"] != "1.2.3" || v["description"] != "a test" || v["name"] != "@acme/tool" ||
		doc["_attachments"].(map[string]any)["@acme/tool-1.2.3.tgz"].(map[string]any)["length"] != float64(len(data)) {
		t.Fatalf("document: %+v", doc)
	}
	got, err := c.Stat(ctx, plugin.Ref{Key: "@acme/tool@1.2.3"})
	if err != nil || got.Size != info.Size || got.Version != info.Version {
		t.Fatalf("stat: %+v, %v", got, err)
	}
	r, err := c.ReadRange(ctx, plugin.Ref{Key: "@acme/tool@1.2.3"}, 0, int64(len(data)), info.Version)
	if err != nil {
		t.Fatal(err)
	}
	back, _ := io.ReadAll(r)
	_ = r.Close()
	if !bytes.Equal(back, data) {
		t.Fatal("read back differs")
	}
	if _, err := c.Stat(ctx, plugin.Ref{Key: "@acme/tool@9.9.9"}); !errors.Is(err, plugin.ErrNotFound) {
		t.Fatalf("another version: %v", err)
	}
	f.mu.Lock()
	f.lag = 2
	f.mu.Unlock()
	if err := c.Delete(ctx, plugin.Ref{Key: "@acme/tool@1.2.3"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Stat(ctx, plugin.Ref{Key: "@acme/tool@1.2.3"}); !errors.Is(err, plugin.ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
	if err := c.Delete(ctx, plugin.Ref{Key: "@acme/tool@1.2.3"}); err != nil {
		t.Fatalf("delete again: %v", err)
	}
	if _, err := put(t, open(t, f, "npm", `, "format": "npm"`, good), "left-pad@1.0.0", pkg(t, "left-pad", "1.0.0")); err != nil ||
		f.npm.published[1]["dist-tags"].(map[string]any)["latest"] != "1.0.0" {
		t.Fatalf("default tag: %v, %+v", err, f.npm.published[1]["dist-tags"])
	}
	if _, err := open(t, f, "npm-proxy", `, "format": "npm"`, good).Begin(ctx, plugin.Ref{Key: "a@1.0.0"}, plugin.ObjectInfo{}, plugin.PutOptions{}); plugin.ClassOf(err) != plugin.Permanent {
		t.Fatalf("into a proxy: %v", err)
	}
}

// NX-T7: what the driver refuses: hostile tarballs, a size beyond
// max_npm_size, a tarball linked elsewhere, a wrong integrity.
func TestNPMRefused(t *testing.T) {
	f := npmFake(t)
	ctx := context.Background()
	d, err := Factory(json.RawMessage(`{"allow_cidrs": ["127.0.0.1/32"], "max_npm_size": 4096}`))
	if err != nil {
		t.Fatal(err)
	}
	c, err := d.Open(ctx, f.config("npm", `, "format": "npm"`), good)
	if err != nil {
		t.Fatal(err)
	}
	var bomb bytes.Buffer
	z := gzip.NewWriter(&bomb)
	_, _ = z.Write(make([]byte, 1<<20))
	_ = z.Close()
	for name, data := range map[string][]byte{
		"no package.json": tgz(t, map[string]string{"package/index.js": "x"}),
		"another name":    pkg(t, "other", "1.0.0"),
		"another version": pkg(t, "a", "2.0.0"),
		"not gzip":        []byte("plain text"),
		"a gzip bomb":     bomb.Bytes(),
		"huge package.json": tgz(t, map[string]string{"package/package.json": `{"name": "a", "version": "1.0.0", "x": "` +
			strings.Repeat("y", 1<<20) + `"}`}),
	} {
		if len(data) > 4096 {
			t.Fatalf("%s: %d bytes, beyond the test's max_npm_size", name, len(data))
		}
		if _, err := put(t, c, "a@1.0.0", data); plugin.ClassOf(err) != plugin.Permanent {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := c.Begin(ctx, plugin.Ref{Key: "a@1.0.0"}, plugin.ObjectInfo{Size: 4097}, plugin.PutOptions{}); plugin.ClassOf(err) != plugin.Permanent {
		t.Errorf("announced beyond max_npm_size: %v", err)
	}
	u, err := c.Begin(ctx, plugin.Ref{Key: "a@1.0.0"}, plugin.ObjectInfo{}, plugin.PutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := u.WritePart(ctx, 0, bytes.NewReader(make([]byte, 4097)), 4097); plugin.ClassOf(err) != plugin.Permanent {
		t.Errorf("unannounced beyond max_npm_size: %v", err)
	}

	f.npm.evil = true
	if _, err := put(t, c, "evil@1.0.0", pkg(t, "evil", "1.0.0")); err == nil {
		t.Error("published with a tarball linked elsewhere")
	}
	if _, err := c.Stat(ctx, plugin.Ref{Key: "evil@1.0.0"}); plugin.ClassOf(err) != plugin.Permanent || !strings.Contains(err.Error(), "outside") {
		t.Errorf("a tarball elsewhere: %v", err)
	}
	f.npm.evil, f.npm.badSum = false, true
	if _, err := put(t, c, "bad@1.0.0", pkg(t, "bad", "1.0.0")); plugin.ClassOf(err) != plugin.Integrity {
		t.Errorf("publish with a wrong integrity: %v", err)
	}
	if _, err := c.Stat(ctx, plugin.Ref{Key: "bad@1.0.0"}); plugin.ClassOf(err) != plugin.Integrity {
		t.Errorf("source with a wrong integrity: %v", err)
	}
}
