package s3

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dihedron/veho/pkg/plugin"
)

// fake answers every request with status and an S3 error of code.
func fake(t *testing.T, status int, code string) plugin.Client {
	t.Helper()
	return fakeMsg(t, status, code, "m")
}

// fakeMsg is fake with the error's message.
func fakeMsg(t *testing.T, status int, code, msg string) plugin.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(status)
		if r.Method != http.MethodHead {
			_, _ = fmt.Fprintf(w, `<?xml version="1.0"?><Error><Code>%s</Code><Message>%s</Message></Error>`, code, msg)
		}
	}))
	t.Cleanup(srv.Close)
	cfg, _ := json.Marshal(config{Endpoint: srv.URL, Region: "r", Bucket: "b", PathStyle: true})
	c, err := New().Open(context.Background(), cfg, plugin.Credentials{"access_key_id": "a", "secret_access_key": "s"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// TestClassify: the error table of veho-plugins.md §7, by the answers of a
// fake service.
func TestClassify(t *testing.T) {
	ctx := context.Background()
	ref := plugin.Ref{Key: "k"}
	for _, c := range []struct {
		status int
		code   string
		class  plugin.Class
	}{
		{503, "SlowDown", plugin.Transient},
		{500, "InternalError", plugin.Transient},
		{429, "TooManyRequests", plugin.Transient},
		{403, "AccessDenied", plugin.Permanent},
		{400, "InvalidArgument", plugin.Permanent},
		{404, "NoSuchBucket", plugin.Permanent},
		{400, "BadDigest", plugin.Integrity},
		{412, "PreconditionFailed", plugin.SourceChanged},
	} {
		_, err := fake(t, c.status, c.code).ReadRange(ctx, ref, 0, 10, "")
		if got := plugin.ClassOf(err); got != c.class {
			t.Errorf("%d %s: class %v (%v), want %v", c.status, c.code, got, err, c.class)
		}
	}
	if _, err := fake(t, 404, "NoSuchKey").ReadRange(ctx, ref, 0, 10, ""); !errors.Is(err, plugin.ErrNotFound) {
		t.Errorf("NoSuchKey: %v, want ErrNotFound", err)
	}
	if _, err := fake(t, 404, "").Stat(ctx, ref); !errors.Is(err, plugin.ErrNotFound) {
		t.Errorf("HEAD 404: %v, want ErrNotFound", err)
	}
	up := &upload{c: fake(t, 404, "NoSuchUpload").(*client), t: token{Bucket: "b", Key: "k", UploadID: "u"}}
	if _, err := up.WritePart(ctx, 0, bytes.NewReader([]byte("x")), 1); plugin.ClassOf(err) != plugin.ResumeInvalid {
		t.Errorf("NoSuchUpload: %v", err)
	}
	// nothing listens: the network
	cfg, _ := json.Marshal(config{Endpoint: "http://127.0.0.1:1", Region: "r", Bucket: "b", PathStyle: true})
	cl, err := New().Open(ctx, cfg, plugin.Credentials{"access_key_id": "a", "secret_access_key": "s"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cl.Stat(ctx, ref); plugin.ClassOf(err) != plugin.Transient {
		t.Errorf("connection refused: %v", err)
	}
}

func TestMultipartETag(t *testing.T) {
	// two parts of "a" and "b": md5(md5("a") || md5("b")) - 2
	parts := []plugin.PartResult{{ETag: `"0cc175b9c0f1b6a831c399e269772661"`}, {ETag: "92eb5ffee6ae2fec3ad71c777531578f"}}
	if got := multipartETag(parts); got != "96e024ba2074fe77e8e965ba43a704be-2" {
		t.Errorf("multipartETag = %q", got)
	}
	if multipartETag([]plugin.PartResult{{ETag: "zz"}}) != "" {
		t.Error("a malformed ETag")
	}
}

func TestParseRef(t *testing.T) {
	if ref, err := New().ParseRef("s3://bucket/a/b"); err != nil || ref != (plugin.Ref{Container: "bucket", Key: "a/b"}) {
		t.Errorf("ParseRef = %+v, %v", ref, err)
	}
	for _, bad := range []string{"s3://bucket", "s3://bucket/", "file:///x", "s3:///k"} {
		if _, err := New().ParseRef(bad); err == nil {
			t.Errorf("ParseRef(%q) accepted", bad)
		}
	}
}

// A commit the service refuses because the upload does not hold the parts
// given (another execution began the upload again) begins the upload again
// instead of failing the transfer (found by the soak, AC-17).
func TestCommitRefused(t *testing.T) {
	ctx := context.Background()
	parts := []plugin.PartResult{{PartNo: 0, Size: 5 << 20, ETag: `"e"`}}
	for _, c := range []struct {
		code, msg string
		class     plugin.Class
	}{
		{"InvalidPart", "One or more of the specified parts could not be found", plugin.ResumeInvalid},
		{"InvalidPartOrder", "m", plugin.ResumeInvalid},
		{"InvalidRequest", "Bad request: No data was uploaded", plugin.ResumeInvalid},
		{"InvalidRequest", "Bad request: something else", plugin.Permanent},
	} {
		up := &upload{c: fakeMsg(t, 400, c.code, c.msg).(*client), t: token{Bucket: "b", Key: "k", UploadID: "u"}}
		if _, err := up.Commit(ctx, parts); plugin.ClassOf(err) != c.class {
			t.Errorf("%s %q: %v, want %v", c.code, c.msg, err, c.class)
		}
	}
}

// open opens a client on url with cfg's extra keys.
func open(t *testing.T, url string, extra map[string]any) (plugin.Client, error) {
	t.Helper()
	m := map[string]any{"endpoint": url, "region": "r", "bucket": "b", "path_style": true}
	for k, v := range extra {
		m[k] = v
	}
	cfg, _ := json.Marshal(m)
	c, err := New().Open(context.Background(), cfg, plugin.Credentials{"access_key_id": "a", "secret_access_key": "s"})
	if err == nil {
		t.Cleanup(func() { _ = c.Close() })
	}
	return c, err
}

// TestOpenStorageClass: archive classes and unknown keys are refused
// (SC-T1, SC-T2, veho-storage-class.md).
func TestOpenStorageClass(t *testing.T) {
	for _, c := range []struct {
		extra map[string]any
		ok    bool
	}{
		{map[string]any{"storage_class": "GLACIER"}, false},
		{map[string]any{"storage_class": "DEEP_ARCHIVE"}, false},
		{map[string]any{"storage_class": "GLACIER_IR"}, true},
		{map[string]any{"storage_class": "STANDARD_IA"}, true},
		{map[string]any{"storage_class": ""}, true},
		{map[string]any{"storage_clas": "STANDARD_IA"}, false},
	} {
		_, err := open(t, "http://127.0.0.1:1", c.extra)
		if (err == nil) != c.ok || (err != nil && plugin.ClassOf(err) != plugin.Permanent) {
			t.Errorf("%v: %v, want ok %v", c.extra, err, c.ok)
		}
	}
}

// recorder is a fake S3 service that records the storage class of each
// write and reports class on HEAD ("" sends no header).
type recorder struct {
	class string
	sent  map[string]string // "create", "put" → x-amz-storage-class received
}

func (rec *recorder) serve(t *testing.T) string {
	t.Helper()
	rec.sent = map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch {
		case r.Method == http.MethodPost && q.Has("uploads"):
			rec.sent["create"] = r.Header.Get("x-amz-storage-class")
			_, _ = fmt.Fprint(w, `<?xml version="1.0"?><InitiateMultipartUploadResult><Bucket>b</Bucket><Key>k</Key><UploadId>u</UploadId></InitiateMultipartUploadResult>`)
		case r.Method == http.MethodPost && q.Has("uploadId"):
			_, _ = fmt.Fprint(w, `<?xml version="1.0"?><CompleteMultipartUploadResult><Bucket>b</Bucket><Key>k</Key><ETag>"e-1"</ETag></CompleteMultipartUploadResult>`)
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPut:
			rec.sent["put"] = r.Header.Get("x-amz-storage-class")
			w.Header().Set("ETag", emptyMD5)
		case r.Method == http.MethodHead:
			if rec.class != "" {
				w.Header().Set("x-amz-storage-class", rec.class)
			}
			w.Header().Set("ETag", `"e-1"`)
			w.Header().Set("Content-Length", "0")
		default:
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestStorageClassSent: the class goes on CreateMultipartUpload and on the
// empty object's PutObject, nothing when unset (SC-T3).
func TestStorageClassSent(t *testing.T) {
	ctx := context.Background()
	for _, class := range []string{"STANDARD_IA", ""} {
		rec := &recorder{class: class}
		c, err := open(t, rec.serve(t), map[string]any{"storage_class": class})
		if err != nil {
			t.Fatal(err)
		}
		up, err := c.Begin(ctx, plugin.Ref{Key: "k"}, plugin.ObjectInfo{}, plugin.PutOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := up.Commit(ctx, []plugin.PartResult{{PartNo: 0}}); err != nil {
			t.Fatal(err)
		}
		if rec.sent["create"] != class || rec.sent["put"] != class {
			t.Errorf("class %q: sent %v", class, rec.sent)
		}
	}
}

// TestStorageClassChecked: after a commit the reported class must be the
// configured one; no header means STANDARD; unset checks nothing (SC-T4).
func TestStorageClassChecked(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		configured, reported string
		ok                   bool
	}{
		{"STANDARD_IA", "STANDARD_IA", true},
		{"STANDARD_IA", "", false},
		{"STANDARD_IA", "STANDARD", false},
		{"STANDARD", "", true},
		{"", "STANDARD_IA", true},
	} {
		rec := &recorder{class: c.reported}
		cl, err := open(t, rec.serve(t), map[string]any{"storage_class": c.configured})
		if err != nil {
			t.Fatal(err)
		}
		for _, parts := range [][]plugin.PartResult{
			{{PartNo: 0}}, // the empty object
			{{PartNo: 0, Size: 5 << 20, ETag: `"e"`}},
		} {
			up := &upload{c: cl.(*client), t: token{Bucket: "b", Key: "k", UploadID: "u"}}
			_, err := up.Commit(ctx, parts)
			if (err == nil) != c.ok || (err != nil && plugin.ClassOf(err) != plugin.Permanent) {
				t.Errorf("%q reported %q, %d bytes: %v, want ok %v", c.configured, c.reported, parts[0].Size, err, c.ok)
			}
		}
	}
}

// TestFactory: the driver has no settings yet, so any key is refused
// (veho-plugin-distribution.md §4).
func TestFactory(t *testing.T) {
	d, err := Factory(json.RawMessage("{}"))
	if err != nil || d.Name() != "s3" {
		t.Fatalf("factory: %v %v", d, err)
	}
	if _, err := Factory(json.RawMessage(`{"nope": 1}`)); err == nil {
		t.Error("an unknown setting was accepted")
	}
}

// OS-T5: a presigned GET for the destination that pulls; the TTL bounded.
func TestPresignRead(t *testing.T) {
	cfg, _ := json.Marshal(config{Endpoint: "https://s3.example", Region: "r", Bucket: "b", PathStyle: true})
	c, err := New().Open(context.Background(), cfg, plugin.Credentials{"access_key_id": "a", "secret_access_key": "s"})
	if err != nil {
		t.Fatal(err)
	}
	p := c.(plugin.Presigner)
	u, err := p.PresignRead(context.Background(), plugin.Ref{Key: "dir/img"}, `"etag"`, time.Hour)
	if err != nil || !strings.HasPrefix(u, "https://s3.example/b/dir/img?") || !strings.Contains(u, "X-Amz-Expires=3600") {
		t.Fatalf("%s, %v", u, err)
	}
	for _, ttl := range []time.Duration{0, 8 * 24 * time.Hour} {
		if _, err := p.PresignRead(context.Background(), plugin.Ref{Key: "k"}, "", ttl); plugin.ClassOf(err) != plugin.Permanent {
			t.Errorf("ttl %s: %v", ttl, err)
		}
	}
}

func TestPresignReadEmptyKey(t *testing.T) {
	cfg, _ := json.Marshal(config{Endpoint: "https://s3.example", Region: "r", Bucket: "b"})
	c, _ := New().Open(context.Background(), cfg, plugin.Credentials{"access_key_id": "a", "secret_access_key": "s"})
	if _, err := c.(plugin.Presigner).PresignRead(context.Background(), plugin.Ref{}, "", time.Hour); plugin.ClassOf(err) != plugin.Permanent {
		t.Fatalf("empty key: %v", err)
	}
}
