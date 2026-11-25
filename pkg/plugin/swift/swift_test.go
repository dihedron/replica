package swift

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/dihedron/veho/pkg/plugin"
	"github.com/dihedron/veho/pkg/plugin/conformance"
)

var good = plugin.Credentials{"application_credential_id": "id", "application_credential_secret": "good"}

func (f *fakeSwift) config(extra string) plugin.Config {
	ca, _ := json.Marshal(f.ca())
	return plugin.Config(`{"auth_url": "` + f.srv.URL + `/v3", "region": "R", "ca_cert": ` + string(ca) + `, "container": "images"` + extra + `}`)
}

func driver(t *testing.T) plugin.Driver {
	t.Helper()
	d, err := Factory(json.RawMessage(`{"allow_cidrs": ["127.0.0.1/32"]}`))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func open(t *testing.T, f *fakeSwift, extra string) plugin.Client {
	t.Helper()
	c, err := driver(t).Open(context.Background(), f.config(extra), good)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// OS-T6 against the fake: the conformance suite as source and sink.
func TestConformance(t *testing.T) {
	f := newFakeSwift(t)
	conformance.Run(t, conformance.Target{
		Driver: driver(t), Config: f.config(""), Credentials: good,
		BadCredentials: plugin.Credentials{"application_credential_id": "id", "application_credential_secret": "bad"},
		Container:      "images", PartSize: 1 << 20,
	})
}

func TestParseRef(t *testing.T) {
	d := driver(t)
	for raw, want := range map[string]plugin.Ref{
		"swift://images/a/b.qcow2": {Container: "images", Key: "a/b.qcow2"},
		"swift://c/x":              {Container: "c", Key: "x"},
	} {
		if got, err := d.ParseRef(raw); err != nil || got != want {
			t.Errorf("%s: %+v, %v", raw, got, err)
		}
	}
	for _, raw := range []string{"s3://b/k", "swift://c", "swift://c/", "swift:///k", "swift://c/a/../b", "swift://c/./b", "swift://c/a//b"} {
		if _, err := d.ParseRef(raw); plugin.ClassOf(err) != plugin.Permanent {
			t.Errorf("%s accepted: %v", raw, err)
		}
	}
}

func put(t *testing.T, c plugin.Client, ref plugin.Ref, parts ...[]byte) plugin.ObjectInfo {
	t.Helper()
	ctx := context.Background()
	u, err := c.Begin(ctx, ref, plugin.ObjectInfo{}, plugin.PutOptions{ContentType: "application/octet-stream", Metadata: map[string]string{"origin": "veho"}})
	if err != nil {
		t.Fatal(err)
	}
	var res []plugin.PartResult
	for i, p := range parts {
		r, err := u.WritePart(ctx, i, bytes.NewReader(p), int64(len(p)))
		if err != nil {
			t.Fatal(err)
		}
		res = append(res, r)
	}
	info, err := u.Commit(ctx, res)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

// The SLO: segments of the upload, invisible until the manifest; Swift
// checks each segment's ETag; the metadata is set; a delete removes the
// segments.
func TestSLO(t *testing.T) {
	f := newFakeSwift(t)
	c := open(t, f, "")
	ctx := context.Background()
	ref := plugin.Ref{Key: "dir/img"}
	u, err := c.Begin(ctx, ref, plugin.ObjectInfo{}, plugin.PutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	p0, err := u.WritePart(ctx, 0, strings.NewReader("hello "), 6)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Stat(ctx, ref); !errors.Is(err, plugin.ErrNotFound) {
		t.Fatalf("visible before commit: %v", err)
	}
	// resumed from the token, in another process
	u2, err := c.Resume(ctx, ref, u.Token())
	if err != nil {
		t.Fatal(err)
	}
	p1, err := u2.WritePart(ctx, 1, strings.NewReader("world"), 5)
	if err != nil {
		t.Fatal(err)
	}
	if parts, err := u2.ListParts(ctx); err != nil || len(parts) != 2 || parts[0].ETag != p0.ETag || parts[1].Size != 5 {
		t.Fatalf("parts: %+v, %v", parts, err)
	}
	// a part whose ETag the manifest does not match: another execution
	// rewrote it, so the upload is to begin again
	bad := p1
	bad.ETag = strings.Repeat("0", 32)
	if _, err := u2.Commit(ctx, []plugin.PartResult{p0, bad}); plugin.ClassOf(err) != plugin.ResumeInvalid {
		t.Fatalf("bad ETag: %v", err)
	}
	info, err := u2.Commit(ctx, []plugin.PartResult{p0, p1})
	if err != nil || info.Size != 11 {
		t.Fatalf("commit: %+v, %v", info, err)
	}
	// committing again returns the object
	if again, err := u2.Commit(ctx, []plugin.PartResult{p0, p1}); err != nil || again.Version != info.Version {
		t.Fatalf("commit again: %+v, %v", again, err)
	}
	r, err := c.ReadRange(ctx, ref, 3, 5, info.Version)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r)
	_ = r.Close()
	if string(b) != "lo wo" {
		t.Fatalf("range: %q", b)
	}
	// overwritten by another upload: the old segments go
	put(t, c, ref, []byte("new "), []byte("data"))
	f.mu.Lock()
	n := len(f.objects)
	f.mu.Unlock()
	if n != 3 {
		t.Fatalf("%d objects after the overwrite, want the manifest and 2 segments (no markers)", n)
	}
	if err := c.Delete(ctx, ref); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	n = len(f.objects)
	f.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d objects left after the delete", n)
	}
	if err := c.Delete(ctx, ref); err != nil {
		t.Fatalf("delete of a missing object: %v", err)
	}
}

// A segment stored with other bytes than those sent is an Integrity error.
func TestSegmentIntegrity(t *testing.T) {
	f := newFakeSwift(t)
	f.putBody = func(name string, b []byte) []byte {
		if strings.Contains(name, "_segments") || strings.Contains(name, "/00000000") {
			return append([]byte{}, append(b, 'x')...)
		}
		return b
	}
	c := open(t, f, "")
	ctx := context.Background()
	u, err := c.Begin(ctx, plugin.Ref{Key: "k"}, plugin.ObjectInfo{}, plugin.PutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := u.WritePart(ctx, 0, strings.NewReader("abc"), 3); plugin.ClassOf(err) != plugin.Integrity {
		t.Fatalf("got %v", err)
	}
	if _, err := u.WritePart(ctx, 1, strings.NewReader("ab"), 3); plugin.ClassOf(err) != plugin.Integrity {
		t.Fatalf("short read: %v", err)
	}
}

// A changed object is SourceChanged, by its ETag or its timestamp.
func TestSourceChanged(t *testing.T) {
	f := newFakeSwift(t)
	c := open(t, f, "")
	ctx := context.Background()
	ref := plugin.Ref{Key: "obj"}
	info := put(t, c, ref, []byte("one"))
	put(t, c, ref, []byte("two"))
	if _, err := c.ReadRange(ctx, ref, 0, 3, info.Version); plugin.ClassOf(err) != plugin.SourceChanged {
		t.Fatalf("other content: %v", err)
	}
	same := put(t, c, ref, []byte("one")) // the same bytes, written again
	if same.Version == info.Version {
		t.Fatal("the version ignores the timestamp")
	}
	if _, err := c.ReadRange(ctx, ref, 0, 3, info.Version); plugin.ClassOf(err) != plugin.SourceChanged {
		t.Fatalf("rewritten: %v", err)
	}
	if got, err := c.Stat(ctx, ref); err != nil || got.Metadata["origin"] != "veho" {
		t.Fatalf("metadata: %+v, %v", got, err)
	}
}

// storage_policy: containers made with it; an existing one with another is
// refused.
func TestStoragePolicy(t *testing.T) {
	f := newFakeSwift(t)
	c := open(t, f, `, "storage_policy": "gold"`)
	put(t, c, plugin.Ref{Key: "a"}, []byte("a"))
	f.mu.Lock()
	got, seg := f.containers["images"], f.containers["images_segments"]
	f.mu.Unlock()
	if got != "gold" || seg != "gold" {
		t.Fatalf("policies %q, %q", got, seg)
	}
	other := open(t, f, `, "storage_policy": "silver"`)
	if _, err := other.Begin(context.Background(), plugin.Ref{Key: "b"}, plugin.ObjectInfo{}, plugin.PutOptions{}); plugin.ClassOf(err) != plugin.Permanent {
		t.Fatalf("got %v", err)
	}
}

func TestOpen(t *testing.T) {
	f := newFakeSwift(t)
	d := driver(t)
	for _, cfg := range []string{
		`{"auth_url": "https://k/v3"}`,
		`{"auth_url": "https://k/v3", "container": "c", "unknown": 1}`,
		`{"auth_url": "http://k/v3", "container": "c"}`,
		`{"auth_url": "https://k/v3", "container": "a/b"}`,
		`not json`,
	} {
		if _, err := d.Open(context.Background(), plugin.Config(cfg), good); plugin.ClassOf(err) != plugin.Permanent {
			t.Errorf("%s: %v", cfg, err)
		}
	}
	if _, err := Factory(json.RawMessage(`{"max_segments": -1}`)); err == nil {
		t.Error("max_segments -1 accepted")
	}
	if _, err := Factory(json.RawMessage(`{"nope": 1}`)); err == nil {
		t.Error("unknown setting accepted")
	}
	var s map[string]any
	if err := json.Unmarshal(d.ConfigSchema(), &s); err != nil || s["properties"].(map[string]any)["auth_url"] == nil {
		t.Errorf("schema: %v", err)
	}
	_ = f
}

// OS-T8: a parsed name never escapes its container in the URL.
func FuzzParseRef(f *testing.F) {
	for _, s := range []string{"swift://c/a/b", "swift://c/..", "swift://c/%2e%2e/x", "swift://c/a%2Fb", "swift://c/a?b#c"} {
		f.Add(s)
	}
	d, _ := Factory(nil)
	f.Fuzz(func(t *testing.T, raw string) {
		ref, err := d.ParseRef(raw)
		if err != nil {
			return
		}
		for seg := range strings.SplitSeq(ref.Key, "/") {
			if seg == "" || seg == "." || seg == ".." {
				t.Fatalf("%q: segment %q", raw, seg)
			}
		}
		if strings.ContainsAny(ref.Container, "/") {
			t.Fatalf("%q: container %q", raw, ref.Container)
		}
	})
}
