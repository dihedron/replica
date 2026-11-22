package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// upstream records what reaches Garage's stand-in.
type upstream struct {
	mu     sync.Mutex
	bodies [][]byte
	hosts  []string
}

func (u *upstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	u.mu.Lock()
	u.bodies = append(u.bodies, b)
	u.hosts = append(u.hosts, r.Host)
	u.mu.Unlock()
	_, _ = w.Write([]byte("ok"))
}

func (u *upstream) last() ([]byte, string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.bodies[len(u.bodies)-1], u.hosts[len(u.hosts)-1]
}

func setup(t *testing.T) (*upstream, *httptest.Server, *httptest.Server) {
	t.Helper()
	up := &upstream{}
	us := httptest.NewServer(up)
	t.Cleanup(us.Close)
	p, err := newProxy(us.URL)
	if err != nil {
		t.Fatal(err)
	}
	ps := httptest.NewServer(p)
	t.Cleanup(ps.Close)
	cs := httptest.NewServer(p.control())
	t.Cleanup(cs.Close)
	return up, ps, cs
}

func setFault(t *testing.T, control string, f fault) {
	t.Helper()
	b, _ := json.Marshal(f)
	resp, err := http.Post(control+"/fault", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("set fault: %s", resp.Status)
	}
}

func put(t *testing.T, url string, body []byte) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPut, url, bytes.NewReader(body))
	req.Host = "s3.example:3901"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// L3-T6: no fault forwards unchanged, keeping the Host the client signed.
func TestForward(t *testing.T) {
	up, ps, _ := setup(t)
	body := []byte("part data")
	if resp := put(t, ps.URL+"/dest-1/obj?partNumber=1&uploadId=u", body); resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	got, host := up.last()
	if !bytes.Equal(got, body) || host != "s3.example:3901" {
		t.Errorf("forwarded %q to host %q", got, host)
	}
}

// L3-T6: a status fault answers an S3 error, for its duration only.
func TestStatus(t *testing.T) {
	_, ps, cs := setup(t)
	setFault(t, cs.URL, fault{Mode: "status", Code: 503, For: duration(300 * time.Millisecond)})
	resp := put(t, ps.URL+"/dest-1/obj", []byte("x"))
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 503 || !strings.Contains(string(b), "<Code>SlowDown</Code>") {
		t.Errorf("during the fault: %d %s", resp.StatusCode, b)
	}
	time.Sleep(400 * time.Millisecond)
	if resp := put(t, ps.URL+"/dest-1/obj", []byte("x")); resp.StatusCode != 200 {
		t.Errorf("after the fault: %d", resp.StatusCode)
	}
	setFault(t, cs.URL, fault{Mode: "status", Code: 403})
	resp = put(t, ps.URL+"/dest-1/obj", []byte("x"))
	b, _ = io.ReadAll(resp.Body)
	if resp.StatusCode != 403 || !strings.Contains(string(b), "<Code>AccessDenied</Code>") {
		t.Errorf("403: %d %s", resp.StatusCode, b)
	}
}

// L3-T6: corrupt flips one byte of the next N uploads, keeping their length.
func TestCorrupt(t *testing.T) {
	up, ps, cs := setup(t)
	setFault(t, cs.URL, fault{Mode: "corrupt", Count: 1})
	body := bytes.Repeat([]byte("a"), 1000)
	put(t, ps.URL+"/dest-1/obj?partNumber=1&uploadId=u", body)
	got, _ := up.last()
	if len(got) != len(body) || bytes.Equal(got, body) {
		t.Fatalf("not corrupted: %d bytes, equal %v", len(got), bytes.Equal(got, body))
	}
	diff := 0
	for i := range got {
		if got[i] != body[i] {
			diff++
		}
	}
	if diff != 1 {
		t.Errorf("%d bytes differ, want 1", diff)
	}
	put(t, ps.URL+"/dest-1/obj?partNumber=2&uploadId=u", body)
	if got, _ := up.last(); !bytes.Equal(got, body) {
		t.Error("the second upload was corrupted too")
	}
}

// L3-T6: a bucket filter leaves the other buckets alone; clear restores.
func TestBucketAndClear(t *testing.T) {
	_, ps, cs := setup(t)
	setFault(t, cs.URL, fault{Mode: "status", Code: 503, Bucket: "dest-2"})
	if resp := put(t, ps.URL+"/dest-1/obj", []byte("x")); resp.StatusCode != 200 {
		t.Errorf("other bucket: %d", resp.StatusCode)
	}
	if resp := put(t, ps.URL+"/dest-2/obj", []byte("x")); resp.StatusCode != 503 {
		t.Errorf("faulted bucket: %d", resp.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodDelete, cs.URL+"/fault", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp := put(t, ps.URL+"/dest-2/obj", []byte("x")); resp.StatusCode != 200 {
		t.Errorf("after clear: %d", resp.StatusCode)
	}
}

// L3-T6: delay holds each response; a bad fault is refused.
func TestDelayAndValidation(t *testing.T) {
	_, ps, cs := setup(t)
	setFault(t, cs.URL, fault{Mode: "delay", Delay: duration(200 * time.Millisecond)})
	start := time.Now()
	put(t, ps.URL+"/dest-1/obj", []byte("x"))
	if d := time.Since(start); d < 200*time.Millisecond {
		t.Errorf("answered after %v", d)
	}
	for _, body := range []string{`{"mode": "nope"}`, `{"mode": "status", "code": 200}`, `{"mode": "corrupt"}`, `{"mode": "delay"}`, `{`} {
		resp, err := http.Post(cs.URL+"/fault", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: %s", body, resp.Status)
		}
	}
}
