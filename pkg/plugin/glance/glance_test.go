package glance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/dihedron/veho/pkg/plugin"
	"github.com/dihedron/veho/pkg/plugin/conformance"
)

var good = plugin.Credentials{"application_credential_id": "id", "application_credential_secret": "good"}

func (f *fakeGlance) config(extra string) plugin.Config {
	ca, _ := json.Marshal(f.ca())
	return plugin.Config(`{"auth_url": "` + f.srv.URL + `/v3", "region": "R", "ca_cert": ` + string(ca) +
		`, "disk_format": "qcow2", "container_format": "bare"` + extra + `}`)
}

func driver(t *testing.T) Driver {
	t.Helper()
	d, err := Factory(json.RawMessage(`{"allow_cidrs": ["127.0.0.1/32"]}`))
	if err != nil {
		t.Fatal(err)
	}
	g := d.(Driver)
	g.poll = time.Millisecond
	return g
}

func open(t *testing.T, f *fakeGlance, extra string) plugin.Client {
	t.Helper()
	c, err := driver(t).Open(context.Background(), f.config(extra), good)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// write uploads data as name in parts of at most 4 bytes.
func write(t *testing.T, c plugin.Client, name string, data []byte) (plugin.Upload, plugin.ObjectInfo, error) {
	t.Helper()
	ctx := context.Background()
	u, err := c.Begin(ctx, plugin.Ref{Key: name}, plugin.ObjectInfo{Size: int64(len(data))}, plugin.PutOptions{})
	if err != nil {
		return nil, plugin.ObjectInfo{}, err
	}
	var parts []plugin.PartResult
	for i := 0; i*4 < len(data); i++ {
		p := data[i*4 : min(len(data), i*4+4)]
		r, err := u.WritePart(ctx, i, bytes.NewReader(p), int64(len(p)))
		if err != nil {
			return u, plugin.ObjectInfo{}, err
		}
		parts = append(parts, r)
	}
	if len(data) == 0 {
		r, err := u.WritePart(ctx, 0, bytes.NewReader(nil), 0)
		if err != nil {
			return u, plugin.ObjectInfo{}, err
		}
		parts = append(parts, r)
	}
	info, err := u.Commit(ctx, parts)
	return u, info, err
}

// The conformance suite, with each upload method.
func TestConformance(t *testing.T) {
	for _, m := range []string{"put", "glance-direct"} {
		t.Run(m, func(t *testing.T) {
			f := newFakeGlance(t, 9)
			conformance.Run(t, conformance.Target{
				Driver: driver(t), Config: f.config(`, "upload": "` + m + `"`), Credentials: good,
				BadCredentials: plugin.Credentials{"application_credential_id": "id", "application_credential_secret": "bad"},
				PartSize:       1 << 20, Prefix: "conformance-",
			})
		})
	}
}

// OS-T3: the source by ID: the version from the hash; a deactivated or
// deleted image is SourceChanged; a Glance without ranges is refused.
func TestSource(t *testing.T) {
	f := newFakeGlance(t, 9)
	c := open(t, f, "")
	ctx := context.Background()
	id := uuid.NewString()
	f.add(id, "src", []byte("0123456789"))
	ref := plugin.Ref{Key: id}
	info, err := c.Stat(ctx, ref)
	if err != nil || info.Size != 10 || !strings.HasPrefix(info.Version, "sha512:") {
		t.Fatalf("stat: %+v, %v", info, err)
	}
	r, err := c.ReadRange(ctx, ref, 2, 3, info.Version)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r)
	_ = r.Close()
	if string(b) != "234" {
		t.Fatalf("range %q", b)
	}
	f.mu.Lock()
	f.images[id]["status"] = "deactivated"
	f.mu.Unlock()
	if _, err := c.ReadRange(ctx, ref, 0, 1, info.Version); plugin.ClassOf(err) != plugin.SourceChanged {
		t.Fatalf("deactivated: %v", err)
	}
	f.mu.Lock()
	delete(f.images, id)
	f.mu.Unlock()
	if _, err := c.ReadRange(ctx, ref, 0, 1, info.Version); plugin.ClassOf(err) != plugin.SourceChanged {
		t.Fatalf("deleted: %v", err)
	}
	f.add(id, "src", []byte("other data"))
	if _, err := c.ReadRange(ctx, ref, 0, 1, info.Version); plugin.ClassOf(err) != plugin.SourceChanged {
		t.Fatalf("re-uploaded: %v", err)
	}
	f.mu.Lock()
	f.ranges = false
	f.mu.Unlock()
	if _, err := c.Stat(ctx, ref); plugin.ClassOf(err) != plugin.Permanent || !strings.Contains(err.Error(), "ranges") {
		t.Fatalf("no ranges: %v", err)
	}
}

// OS-T4: hidden until commit; the hash compared; a second image of the name
// replaced; a new attempt deletes the hidden image; a missing method or
// store refused at Begin.
func TestDestination(t *testing.T) {
	f := newFakeGlance(t, 9)
	c := open(t, f, "")
	ctx := context.Background()
	u, err := c.Begin(ctx, plugin.Ref{Key: "img"}, plugin.ObjectInfo{Size: 4}, plugin.PutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	p, err := u.WritePart(ctx, 0, strings.NewReader("data"), 4)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Stat(ctx, plugin.Ref{Key: "img"}); !errors.Is(err, plugin.ErrNotFound) {
		t.Fatalf("visible before commit: %v", err)
	}
	f.mu.Lock()
	n := len(f.images)
	f.mu.Unlock()
	if n != 1 {
		t.Fatalf("%d images", n)
	}
	info, err := u.Commit(ctx, []plugin.PartResult{p})
	if err != nil || info.Size != 4 {
		t.Fatalf("commit: %+v, %v", info, err)
	}
	if got, err := c.Stat(ctx, plugin.Ref{Key: "img"}); err != nil || got.Version != info.Version {
		t.Fatalf("stat: %+v, %v", got, err)
	}
	// a committed upload, resumed, is the object
	r, err := c.Resume(ctx, plugin.Ref{Key: "img"}, u.Token())
	if err != nil {
		t.Fatal(err)
	}
	if again, err := r.Commit(ctx, nil); err != nil || again.Version != info.Version {
		t.Fatalf("commit again: %+v, %v", again, err)
	}
	// replaced: the older image goes
	if _, _, err := write(t, c, "img", []byte("newer")); err != nil {
		t.Fatal(err)
	}
	if got, err := c.Stat(ctx, plugin.Ref{Key: "img"}); err != nil || got.Size != 5 {
		t.Fatalf("after the replacement: %+v, %v", got, err)
	}
	// an attempt that broke: Resume deletes the hidden image
	u, err = c.Begin(ctx, plugin.Ref{Key: "broken"}, plugin.ObjectInfo{Size: 8}, plugin.PutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := u.WritePart(ctx, 0, strings.NewReader("half"), 4); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Resume(ctx, plugin.Ref{Key: "broken"}, u.Token()); plugin.ClassOf(err) != plugin.ResumeInvalid {
		t.Fatalf("resume: %v", err)
	}
	p1, err := u.WritePart(ctx, 1, strings.NewReader("more"), 4)
	if err != nil {
		t.Fatal(err) // the request is still being read: the failure is at Commit
	}
	parts, _ := u.ListParts(ctx)
	if _, err := u.Commit(ctx, []plugin.PartResult{parts[0], p1}); plugin.ClassOf(err) != plugin.ResumeInvalid {
		t.Fatalf("commit into a deleted image: %v", err)
	}
	_ = u.Abort(ctx)
	f.mu.Lock()
	n = len(f.images)
	f.mu.Unlock()
	if n != 1 {
		t.Fatalf("%d images left, want 1", n)
	}
	// parts out of order, or beyond the size
	u, _ = c.Begin(ctx, plugin.Ref{Key: "order"}, plugin.ObjectInfo{Size: 4}, plugin.PutOptions{})
	if _, err := u.WritePart(ctx, 1, strings.NewReader("data"), 4); plugin.ClassOf(err) != plugin.ResumeInvalid {
		t.Fatalf("out of order: %v", err)
	}
	if _, err := u.WritePart(ctx, 0, strings.NewReader("da"), 2); err != nil {
		t.Fatal(err)
	}
	if _, err := u.WritePart(ctx, 0, strings.NewReader("da"), 2); err != nil {
		t.Fatalf("the same part again: %v", err)
	}
	if _, err := u.WritePart(ctx, 0, strings.NewReader("xx"), 2); plugin.ClassOf(err) != plugin.ResumeInvalid {
		t.Fatalf("the same part with other bytes: %v", err)
	}
	if _, err := u.WritePart(ctx, 1, strings.NewReader("datadata"), 8); plugin.ClassOf(err) != plugin.Permanent {
		t.Fatalf("beyond the size: %v", err)
	}
	_ = u.Abort(ctx)
	// a name that is a UUID, and a destination without formats
	if _, err := c.Begin(ctx, plugin.Ref{Key: uuid.NewString()}, plugin.ObjectInfo{}, plugin.PutOptions{}); plugin.ClassOf(err) != plugin.Permanent {
		t.Fatalf("UUID name: %v", err)
	}
	ca, _ := json.Marshal(f.ca())
	bare, err := driver(t).Open(ctx, plugin.Config(`{"auth_url": "`+f.srv.URL+`/v3", "region": "R", "ca_cert": `+string(ca)+`}`), good)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bare.Begin(ctx, plugin.Ref{Key: "x"}, plugin.ObjectInfo{}, plugin.PutOptions{}); plugin.ClassOf(err) != plugin.Permanent {
		t.Fatalf("no formats: %v", err)
	}
}

func TestIntegrity(t *testing.T) {
	for _, minor := range []int{6, 9} {
		f := newFakeGlance(t, minor)
		f.corrupt = true
		c := open(t, f, "")
		if _, _, err := write(t, c, "img", []byte("payload")); plugin.ClassOf(err) != plugin.Integrity {
			t.Fatalf("v2.%d: %v", minor, err)
		}
	}
}

func TestGlanceDirect(t *testing.T) {
	f := newFakeGlance(t, 9)
	c := open(t, f, `, "upload": "glance-direct", "stores": ["fast"]`)
	_, info, err := write(t, c, "img", []byte("payload"))
	if err != nil || info.Size != 7 {
		t.Fatalf("%+v, %v", info, err)
	}
	f.mu.Lock()
	var stores any
	for _, im := range f.images {
		stores = im["veho_stores"]
	}
	f.mu.Unlock()
	if stores != "fast" {
		t.Fatalf("stores %v", stores)
	}
	ctx := context.Background()
	for _, extra := range []string{`, "upload": "glance-direct", "stores": ["slow"]`} {
		if _, err := open(t, f, extra).Begin(ctx, plugin.Ref{Key: "x"}, plugin.ObjectInfo{}, plugin.PutOptions{}); plugin.ClassOf(err) != plugin.Permanent {
			t.Fatalf("%s: %v", extra, err)
		}
	}
	f.mu.Lock()
	f.methods = []string{"web-download"}
	f.mu.Unlock()
	if _, err := open(t, f, `, "upload": "glance-direct"`).Begin(ctx, plugin.Ref{Key: "x"}, plugin.ObjectInfo{}, plugin.PutOptions{}); plugin.ClassOf(err) != plugin.Permanent {
		t.Fatalf("no glance-direct: %v", err)
	}
}

// OS-T4b: a Glance of Queens: put under the temporary name and renamed,
// the MD5 compared, stores refused.
func TestQueens(t *testing.T) {
	f := newFakeGlance(t, 6)
	c := open(t, f, "")
	ctx := context.Background()
	u, err := c.Begin(ctx, plugin.Ref{Key: "img"}, plugin.ObjectInfo{Size: 4}, plugin.PutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	p, err := u.WritePart(ctx, 0, strings.NewReader("data"), 4)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	var name any
	for _, im := range f.images {
		name = im["name"]
	}
	f.mu.Unlock()
	if s, _ := name.(string); !strings.HasPrefix(s, "veho-upload-") {
		t.Fatalf("named %v while uploading", name)
	}
	info, err := u.Commit(ctx, []plugin.PartResult{p})
	if err != nil || !strings.HasPrefix(info.Version, "md5:") {
		t.Fatalf("commit: %+v, %v", info, err)
	}
	if _, err := c.Stat(ctx, plugin.Ref{Key: "img"}); err != nil {
		t.Fatal(err)
	}
	if _, err := open(t, f, `, "upload": "glance-direct", "stores": ["fast"]`).Begin(ctx, plugin.Ref{Key: "x"}, plugin.ObjectInfo{}, plugin.PutOptions{}); plugin.ClassOf(err) != plugin.Permanent {
		t.Fatalf("stores on Queens: %v", err)
	}
}

func TestOpen(t *testing.T) {
	f := newFakeGlance(t, 9)
	d := driver(t)
	for _, extra := range []string{
		`, "upload": "copy"`, `, "upload": "web-download", "web_download_url_ttl": "10s"`, `, "web_download_url_ttl": "x"`, `, "stores": ["fast"]`,
		`, "properties": {"os_hidden": "true"}`, `, "properties": {"os_glance_x": "1"}`, `, "nope": 1`,
	} {
		if _, err := d.Open(context.Background(), f.config(extra), good); plugin.ClassOf(err) != plugin.Permanent {
			t.Errorf("%s: %v", extra, err)
		}
	}
	for raw, ok := range map[string]bool{"glance://img": true, "glance://a/b": true, "glance://": false, "s3://x": false, "glance://x?y": false} {
		if _, err := d.ParseRef(raw); (err == nil) != ok {
			t.Errorf("%s: %v", raw, err)
		}
	}
	var s map[string]any
	if err := json.Unmarshal(d.ConfigSchema(), &s); err != nil || s["properties"].(map[string]any)["auth_url"] == nil {
		t.Errorf("schema: %v", err)
	}
}

// OS-T5: web-download: Glance fetches the URL; the image stays hidden while
// it is read back by its ID, and Commit publishes it.
func TestWebDownload(t *testing.T) {
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "pulled bytes") }))
	t.Cleanup(src.Close)
	f := newFakeGlance(t, 9)
	c := open(t, f, `, "upload": "web-download", "web_download_url_ttl": "30m"`)
	ctx := context.Background()
	u, err := c.Begin(ctx, plugin.Ref{Key: "img"}, plugin.ObjectInfo{Size: 12}, plugin.PutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	p, ok := u.(plugin.Puller)
	if !ok || p.PullTTL() != 30*time.Minute {
		t.Fatalf("not a puller: %v", ok)
	}
	if _, err := u.WritePart(ctx, 0, strings.NewReader("x"), 1); plugin.ClassOf(err) != plugin.Permanent {
		t.Fatalf("a part: %v", err)
	}
	if _, err := u.Commit(ctx, nil); plugin.ClassOf(err) != plugin.ResumeInvalid {
		t.Fatalf("commit before the pull: %v", err)
	}
	ref, err := p.Pull(ctx, src.URL+"/img")
	if err != nil {
		t.Fatal(err)
	}
	if again, err := p.Pull(ctx, src.URL+"/img"); err != nil || again != ref {
		t.Fatalf("pull again: %+v, %v", again, err)
	}
	if _, err := c.Stat(ctx, plugin.Ref{Key: "img"}); !errors.Is(err, plugin.ErrNotFound) {
		t.Fatalf("visible before commit: %v", err)
	}
	r, err := c.ReadRange(ctx, ref, 7, 5, "")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r)
	_ = r.Close()
	if string(b) != "bytes" {
		t.Fatalf("read back %q", b)
	}
	info, err := u.Commit(ctx, nil)
	if err != nil || info.Size != 12 {
		t.Fatalf("commit: %+v, %v", info, err)
	}
	if _, err := c.Stat(ctx, plugin.Ref{Key: "img"}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Pull(ctx, src.URL); plugin.ClassOf(err) != plugin.ResumeInvalid {
		t.Fatalf("pull after commit: %v", err)
	}
	// Glance cannot fetch the URL: the import fails
	bad, err := c.Begin(ctx, plugin.Ref{Key: "unreachable"}, plugin.ObjectInfo{Size: 1}, plugin.PutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bad.(plugin.Puller).Pull(ctx, "http://127.0.0.1:1/none"); plugin.ClassOf(err) != plugin.Transient {
		t.Fatalf("failed import: %v", err)
	}
	_ = bad.Abort(ctx)
	// a put endpoint's upload does not pull
	put, _ := open(t, f, "").Begin(ctx, plugin.Ref{Key: "other"}, plugin.ObjectInfo{Size: 1}, plugin.PutOptions{})
	if put.(plugin.Puller).PullTTL() != 0 {
		t.Fatal("a put upload pulls")
	}
	if _, err := put.(plugin.Puller).Pull(ctx, src.URL); plugin.ClassOf(err) != plugin.Permanent {
		t.Fatalf("pull on put: %v", err)
	}
	f.mu.Lock()
	f.methods = []string{"glance-direct"}
	f.mu.Unlock()
	if _, err := c.Begin(ctx, plugin.Ref{Key: "x"}, plugin.ObjectInfo{}, plugin.PutOptions{}); plugin.ClassOf(err) != plugin.Permanent {
		t.Fatalf("no web-download: %v", err)
	}
}
