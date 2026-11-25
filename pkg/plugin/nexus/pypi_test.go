package nexus

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"testing"

	"github.com/dihedron/veho/pkg/plugin"
)

// pypiRepo is the fake's PyPI side: the forms it received.
type pypiRepo struct {
	forms  []map[string]string
	order  [][]string // the form's field names, in order
	evil   bool       // the index links elsewhere (NX-6)
	badSum bool       // the index's sha256 is wrong
}

// servePyPI answers the PyPI repository "pypi": twine's upload and the
// simple index; the files themselves are the fake's assets.
func (f *fakeNexus) servePyPI(w http.ResponseWriter, r *http.Request, authed bool) bool {
	switch {
	case r.URL.Path == "/service/rest/v1/search/assets" && r.URL.Query().Get("format") == "pypi":
		var items []map[string]string
		for path := range f.assets {
			if rest, ok := strings.CutPrefix(path, "pypi/packages/"); ok && strings.Split(rest, "/")[1] == r.URL.Query().Get("version") {
				items = append(items, map[string]string{"id": url.PathEscape(path), "path": "/" + strings.TrimPrefix(path, "pypi/")})
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
		return true
	case strings.HasPrefix(r.URL.Path, "/service/rest/v1/assets/") && r.Method == http.MethodDelete:
		path, _ := url.PathUnescape(strings.TrimPrefix(r.URL.EscapedPath(), "/service/rest/v1/assets/"))
		delete(f.assets, path)
		w.WriteHeader(http.StatusNoContent)
		return true
	case strings.HasPrefix(r.URL.Path, "/repository/pypi/") && r.Method == http.MethodDelete:
		w.WriteHeader(http.StatusNotFound) // as Nexus: no DELETE of a PyPI content path
		return true
	case r.URL.Path == "/repository/pypi/" && r.Method == http.MethodPost:
		if !authed {
			w.WriteHeader(http.StatusUnauthorized)
			return true
		}
		mr, err := r.MultipartReader()
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return true
		}
		form, order, file := map[string]string{}, []string{}, []byte(nil)
		f.mu.Unlock()
		for {
			p, err := mr.NextPart()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				f.mu.Lock()
				return true // an unfinished request leaves nothing
			}
			b, _ := io.ReadAll(p)
			order = append(order, p.FormName())
			if p.FormName() == "content" {
				form["content-filename"], file = p.FileName(), b
				continue
			}
			form[p.FormName()] = string(b)
		}
		f.mu.Lock()
		sum := sha256.Sum256(file)
		if form[":action"] != "file_upload" || form["sha256_digest"] != hex.EncodeToString(sum[:]) || form["name"] == "" || form["version"] == "" {
			http.Error(w, "bad form", http.StatusBadRequest)
			return true
		}
		path := fmt.Sprintf("pypi/packages/%s/%s/%s", pep503(form["name"]), form["version"], form["content-filename"])
		if _, there := f.assets[path]; there && f.noDeploy["pypi"] {
			w.WriteHeader(http.StatusBadRequest)
			return true
		}
		f.assets[path] = file
		f.pypi.forms, f.pypi.order = append(f.pypi.forms, form), append(f.pypi.order, order)
		w.WriteHeader(http.StatusOK)
		return true
	case strings.HasPrefix(r.URL.Path, "/repository/pypi/simple/") && r.Method == http.MethodGet:
		project := strings.Trim(strings.TrimPrefix(r.URL.Path, "/repository/pypi/simple/"), "/")
		var links []string
		for path, b := range f.assets {
			rest, ok := strings.CutPrefix(path, "pypi/packages/"+project+"/")
			if !ok {
				continue
			}
			sum := sha256.Sum256(b)
			if f.pypi.badSum {
				sum[0] ^= 1
			}
			href := "../../packages/" + project + "/" + rest
			if f.pypi.evil {
				href = "https://evil.example/" + rest
			}
			file := rest[strings.LastIndex(rest, "/")+1:]
			links = append(links, fmt.Sprintf(`<a href="%s#sha256=%s" data-requires-python="&gt;=3.8">%s</a><br/>`, href, hex.EncodeToString(sum[:]), file))
		}
		if len(links) == 0 {
			w.WriteHeader(http.StatusNotFound)
			return true
		}
		sort.Strings(links)
		_, _ = fmt.Fprintf(w, "<html><head><title>Links for %s</title></head><body><h1>Links for %s</h1>%s</body></html>", project, project, strings.Join(links, "\n"))
		return true
	}
	return false
}

// NX-T8: wheel and sdist names.
func TestPyPIKey(t *testing.T) {
	for key, want := range map[string]pypiKey{
		"veho_test-1.0-py3-none-any.whl":                      {file: "veho_test-1.0-py3-none-any.whl", name: "veho_test", project: "veho-test", version: "1.0", filetype: "bdist_wheel", pyversion: "py3"},
		"numpy-2.1.0-1-cp312-cp312-manylinux_2_17_x86_64.whl": {file: "numpy-2.1.0-1-cp312-cp312-manylinux_2_17_x86_64.whl", name: "numpy", project: "numpy", version: "2.1.0", filetype: "bdist_wheel", pyversion: "cp312"},
		"Veho.Test-2!1.0rc1+local.7.tar.gz":                   {file: "Veho.Test-2!1.0rc1+local.7.tar.gz", name: "Veho.Test", project: "veho-test", version: "2!1.0rc1+local.7", filetype: "sdist", pyversion: "source"},
	} {
		if got, err := parsePyPI(key); err != nil || got != want {
			t.Errorf("%s: %+v, %v", key, got, err)
		}
	}
	for _, key := range []string{"", "x.whl", "a-1.0.whl", "a-1.0-py3-none.whl", "a-1.0-x-py3-none-any.whl", "a-1.0.zip", "a.tar.gz",
		"-1.0.tar.gz", "a_-1.0.tar.gz", "a/b-1.0.tar.gz", "../a-1.0.tar.gz", "a-1.0-py3-none-any.whl\n", "a b-1.0.tar.gz", "a-1 0.tar.gz", "a-1.0.tar.gz?x"} {
		if _, err := parsePyPI(key); plugin.ClassOf(err) != plugin.Permanent {
			t.Errorf("%q accepted: %v", key, err)
		}
	}
}

func FuzzPyPIKey(f *testing.F) {
	for _, s := range []string{"veho_test-1.0-py3-none-any.whl", "a-1.0.tar.gz", "a-1.0-py3-none-../x.whl"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, key string) {
		k, err := parsePyPI(key)
		if err != nil {
			return
		}
		for _, v := range []string{k.file, k.name, k.version, k.pyversion, k.project} {
			if strings.ContainsAny(v, "/\\%?#\"\r\n ") || v == "." || v == ".." {
				t.Fatalf("%q: %+v", key, k)
			}
		}
	})
}

func pypiFake(t *testing.T) *fakeNexus {
	t.Helper()
	f := newFake(t)
	f.repos["pypi"] = repository{Name: "pypi", Format: "pypi", Type: "hosted"}
	return f
}

// NX-T8 on the fake: a wheel round trip, the form's fields and order, the
// file found in the index, deleted.
func TestPyPI(t *testing.T) {
	f := pypiFake(t)
	ctx := context.Background()
	c := open(t, f, "pypi", `, "format": "pypi"`, good)
	data := bytes.Repeat([]byte("wheel bytes "), 300000)
	key := "veho_test-1.0-py3-none-any.whl"
	u, err := c.Begin(ctx, plugin.Ref{Key: key}, plugin.ObjectInfo{Size: int64(len(data))}, plugin.PutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	half := int64(len(data) / 2)
	p0, err := u.WritePart(ctx, 0, bytes.NewReader(data[:half]), half)
	if err != nil {
		t.Fatal(err)
	}
	p1, err := u.WritePart(ctx, 1, bytes.NewReader(data[half:]), int64(len(data))-half)
	if err != nil {
		t.Fatal(err)
	}
	info, err := u.Commit(ctx, []plugin.PartResult{p0, p1})
	if err != nil || info.Size != int64(len(data)) || info.Version != etag(data) {
		t.Fatalf("commit: %+v, %v", info, err)
	}
	form := f.pypi.forms[0]
	if form["name"] != "veho_test" || form["version"] != "1.0" || form["filetype"] != "bdist_wheel" || form["pyversion"] != "py3" ||
		form["protocol_version"] != "1" || form["content-filename"] != key {
		t.Fatalf("form: %+v", form)
	}
	if order := f.pypi.order[0]; order[len(order)-1] != "sha256_digest" || order[len(order)-2] != "content" {
		t.Fatalf("the digest after the file: %v", order)
	}
	got, err := c.Stat(ctx, plugin.Ref{Key: key})
	if err != nil || got.Size != info.Size || got.Version != info.Version {
		t.Fatalf("stat: %+v, %v", got, err)
	}
	r, err := c.ReadRange(ctx, plugin.Ref{Key: key}, 5, 7, info.Version)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r)
	_ = r.Close()
	if string(b) != string(data[5:12]) {
		t.Fatalf("range %q", b)
	}
	if _, err := c.Stat(ctx, plugin.Ref{Key: "veho_test-2.0-py3-none-any.whl"}); !errors.Is(err, plugin.ErrNotFound) {
		t.Fatalf("not in the index: %v", err)
	}
	if err := c.Delete(ctx, plugin.Ref{Key: key}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Stat(ctx, plugin.Ref{Key: key}); !errors.Is(err, plugin.ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
	if err := c.Delete(ctx, plugin.Ref{Key: key}); err != nil {
		t.Fatalf("delete again: %v", err)
	}
	if _, err := put(t, c, "veho_test-1.0.tar.gz", []byte("sdist")); err != nil || f.pypi.forms[1]["filetype"] != "sdist" || f.pypi.forms[1]["pyversion"] != "source" {
		t.Fatalf("sdist: %v, %+v", err, f.pypi.forms)
	}
}

// NX-T8: an index that links elsewhere, a wrong sha256.
func TestPyPIRefused(t *testing.T) {
	f := pypiFake(t)
	ctx := context.Background()
	c := open(t, f, "pypi", `, "format": "pypi"`, good)
	f.pypi.evil = true
	evil := "evil-1.0.tar.gz"
	if _, err := put(t, c, evil, []byte("x")); err == nil {
		t.Error("uploaded with an index linking elsewhere")
	}
	if _, err := c.Stat(ctx, plugin.Ref{Key: evil}); plugin.ClassOf(err) != plugin.Permanent || !strings.Contains(err.Error(), "outside") {
		t.Errorf("a file elsewhere: %v", err)
	}
	f.pypi.evil, f.pypi.badSum = false, true
	if _, err := put(t, c, "bad-1.0.tar.gz", []byte("y")); plugin.ClassOf(err) != plugin.Integrity {
		t.Errorf("upload with a wrong sha256: %v", err)
	}
	if _, err := c.Stat(ctx, plugin.Ref{Key: "bad-1.0.tar.gz"}); plugin.ClassOf(err) != plugin.Integrity {
		t.Errorf("source with a wrong sha256: %v", err)
	}
}
