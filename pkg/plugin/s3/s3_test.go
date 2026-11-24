package s3

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

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
