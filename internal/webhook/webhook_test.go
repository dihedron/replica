package webhook

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

// WH-T1: signature vectors, a tampered body, an old timestamp, two secrets.
func TestSignAndVerify(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	body := []byte(`{"id":"e1"}`)
	old, cur := []byte("old-secret"), []byte("new-secret")
	h := Sign(now, body, cur, old)
	if !strings.HasPrefix(h, "t=1700000000,v1=") || strings.Count(h, "v1=") != 2 {
		t.Fatalf("header %q: want a timestamp and two v1 signatures", h)
	}
	// the receiver holds either secret
	for _, s := range [][]byte{old, cur} {
		if err := Verify(h, body, s, now.Add(time.Minute), 5*time.Minute); err != nil {
			t.Errorf("verify with %q: %v", s, err)
		}
	}
	if err := Verify(h, []byte(`{"id":"e2"}`), cur, now, time.Minute); !errors.Is(err, ErrSignature) {
		t.Errorf("tampered body: %v", err)
	}
	if err := Verify(h, body, []byte("other"), now, time.Minute); !errors.Is(err, ErrSignature) {
		t.Errorf("wrong secret: %v", err)
	}
	if err := Verify(h, body, cur, now.Add(6*time.Minute), 5*time.Minute); !errors.Is(err, ErrStale) {
		t.Errorf("old timestamp: %v", err)
	}
	// the timestamp is inside the signed string
	forged := strings.Replace(h, "t=1700000000", "t=1700000001", 1)
	if err := Verify(forged, body, cur, now, time.Minute); !errors.Is(err, ErrSignature) {
		t.Errorf("forged timestamp: %v", err)
	}
	for _, bad := range []string{"", "v1=ab", "t=x,v1=ab", "t=1700000000"} {
		if err := Verify(bad, body, cur, now, time.Hour); err == nil {
			t.Errorf("header %q accepted", bad)
		}
	}
}

// WH-T1 known vector: HMAC-SHA256(key "k", "1700000000.{}").
func TestSignVector(t *testing.T) {
	got := Sign(time.Unix(1_700_000_000, 0), []byte("{}"), []byte("k"))
	// openssl: printf '1700000000.{}' | openssl dgst -sha256 -hmac k
	const want = "t=1700000000,v1=c6ef4734033cee2367414a0939385eca0397754aa7a59716da7463a9298b5146"
	if got != want {
		t.Fatalf("%q, want %q", got, want)
	}
}

func TestValidateURL(t *testing.T) {
	for _, ok := range []string{"https://hooks.example.com/veho", "https://hooks.example.com:8443/a?b=c"} {
		if err := ValidateURL(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "http://hooks.example.com/", "https://", "https://user:pw@hooks.example.com/", "ftp://x/", "https://x/#frag",
		"https://" + strings.Repeat("a", 2100), "//hooks.example.com", "https://hooks example.com/"} {
		if err := ValidateURL(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// WH-T2: addresses that are never dialled, unless an operator lists them.
func TestAllowed(t *testing.T) {
	g, err := NewGuard(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range []string{"127.0.0.1", "::1", "10.1.2.3", "172.16.0.1", "192.168.1.1", "169.254.169.254", "fd00::1", "fe80::1",
		"0.0.0.0", "::", "224.0.0.1", "ff02::1", "100.64.0.1", "::ffff:127.0.0.1", "::ffff:10.0.0.1", "169.254.170.2", "fd00:ec2::254", "198.18.0.1", "192.0.0.1"} {
		if g.Allowed(netip.MustParseAddr(a)) {
			t.Errorf("%s allowed", a)
		}
	}
	for _, a := range []string{"8.8.8.8", "93.184.216.34", "2606:2800:220:1::1"} {
		if !g.Allowed(netip.MustParseAddr(a)) {
			t.Errorf("%s refused", a)
		}
	}
	// an operator-listed CIDR opens a private range, not the metadata address
	g, err = NewGuard([]string{"10.20.0.0/16", "169.254.169.254/32"})
	if err != nil {
		t.Fatal(err)
	}
	if !g.Allowed(netip.MustParseAddr("10.20.1.1")) || g.Allowed(netip.MustParseAddr("10.21.1.1")) {
		t.Error("the listed CIDR is not honoured exactly")
	}
	if g.Allowed(netip.MustParseAddr("169.254.169.254")) {
		t.Error("the metadata address can never be allowed")
	}
	if _, err := NewGuard([]string{"nonsense"}); err == nil {
		t.Error("a bad CIDR was accepted")
	}
}

// WH-T2: the address actually dialled is checked, so a name that resolves to
// a private address, or that changes its answer, never reaches the target.
func TestClientRefusesInternal(t *testing.T) {
	hit := make(chan struct{}, 1)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit <- struct{}{} }))
	defer srv.Close()
	g, _ := NewGuard(nil)
	c := g.Client(2 * time.Second)
	_, err := c.Post(srv.URL, "application/json", strings.NewReader("{}")) // 127.0.0.1
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("a loopback target: %v", err)
	}
	select {
	case <-hit:
		t.Fatal("the target was reached")
	default:
	}
	// "localhost" resolves to the loopback
	if _, err := c.Post("https://localhost:"+portOf(t, srv.URL), "application/json", nil); !errors.Is(err, ErrForbidden) {
		t.Fatalf("localhost: %v", err)
	}
}

// WH-T2: a redirect to an internal URL is not followed.
func TestNoRedirects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	defer srv.Close()
	g, _ := NewGuard([]string{"127.0.0.0/8"})
	resp, err := g.Client(2*time.Second).Post(srv.URL, "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusFound {
		t.Errorf("status %d: the redirect must be returned, not followed", resp.StatusCode)
	}
}

func TestDeliver(t *testing.T) {
	var got *http.Request
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(strings.Repeat("x", 5000)))
	}))
	defer srv.Close()
	g, _ := NewGuard([]string{"127.0.0.0/8"})
	g.tlsConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // the test's own server
	ev := Event{ID: "e1", Type: "job.succeeded", OccurredAt: time.Unix(1_700_000_000, 0).UTC(), Body: []byte(`{"id":"e1"}`)}
	res := g.Deliver(context.Background(), srv.URL, ev, "veho/test", time.Unix(1_700_000_000, 0), []byte("s"))
	if res.OK || res.Status != 500 || res.Class != "status" {
		t.Fatalf("result %+v", res)
	}
	if got.Header.Get("Veho-Event-Id") != "e1" || got.Header.Get("Veho-Event-Type") != "job.succeeded" ||
		!strings.HasPrefix(got.Header.Get("Veho-Signature"), "t=1700000000,v1=") || got.Header.Get("User-Agent") != "veho/test" {
		t.Errorf("headers %v", got.Header)
	}
	// a refused target is classified, with no address in the text
	g2, _ := NewGuard(nil)
	res = g2.Deliver(context.Background(), srv.URL, ev, "veho/test", time.Now(), []byte("s"))
	if res.OK || res.Class != "forbidden" || strings.Contains(res.Message, "127.0.0.1") {
		t.Errorf("refused target: %+v", res)
	}
}

func portOf(t *testing.T, u string) string {
	t.Helper()
	i := strings.LastIndex(u, ":")
	return u[i+1:]
}

func TestBackoff(t *testing.T) {
	want := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, 32 * time.Minute, time.Hour, time.Hour}
	for i, w := range want {
		if got := Backoff(i + 1); got != w {
			t.Errorf("attempt %d: %v, want %v", i+1, got, w)
		}
	}
}
