package netguard

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeProxy answers CONNECT as a corporate proxy does: it alone resolves
// the outside names (dns), and may refuse with a status.
type fakeProxy struct {
	ln     net.Listener
	dns    map[string]string // host to the address it really is
	refuse int
	mu     sync.Mutex
	seen   []string // the CONNECT targets
	auth   []string // their Proxy-Authorization
}

func newFakeProxy(t *testing.T, dns map[string]string) *fakeProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &fakeProxy{ln: ln, dns: dns}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go p.serve(c)
		}
	}()
	return p
}

func (p *fakeProxy) serve(c net.Conn) {
	defer func() { _ = c.Close() }()
	req, err := http.ReadRequest(bufio.NewReader(c))
	if err != nil || req.Method != http.MethodConnect {
		_, _ = io.WriteString(c, "HTTP/1.1 405 Method Not Allowed\r\n\r\n")
		return
	}
	p.mu.Lock()
	p.seen = append(p.seen, req.Host)
	p.auth = append(p.auth, req.Header.Get("Proxy-Authorization"))
	refuse := p.refuse
	p.mu.Unlock()
	if refuse != 0 {
		_, _ = fmt.Fprintf(c, "HTTP/1.1 %d %s\r\n\r\n", refuse, http.StatusText(refuse))
		return
	}
	host, port, _ := net.SplitHostPort(req.Host)
	addr, ok := p.dns[host]
	if !ok {
		_, _ = io.WriteString(c, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
		return
	}
	_, realPort, _ := net.SplitHostPort(addr)
	if port != realPort {
		addr = net.JoinHostPort(strings.Split(addr, ":")[0], port)
	}
	up, err := net.Dial("tcp", addr)
	if err != nil {
		_, _ = io.WriteString(c, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
		return
	}
	defer func() { _ = up.Close() }()
	_, _ = io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n")
	go func() { _, _ = io.Copy(up, c) }()
	_, _ = io.Copy(c, up)
}

func (p *fakeProxy) targets() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.seen...)
}

func credentials(t *testing.T, mode os.FileMode) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "proxy")
	if err := os.WriteFile(f, []byte("veho:s3cret\n"), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(f, mode); err != nil {
		t.Fatal(err)
	}
	return f
}

// PX-T1: the settings.
func TestProxySettings(t *testing.T) {
	if p, err := NewProxy(nil); p != nil || err != nil {
		t.Fatalf("none: %v, %v", p, err)
	}
	good := credentials(t, 0o600)
	for name, s := range map[string]ProxySettings{
		"no url":        {Hosts: []string{"*.amazonaws.com"}},
		"https url":     {URL: "https://proxy.corp:3128", Hosts: []string{"*.amazonaws.com"}},
		"no hosts":      {URL: "http://proxy.corp:3128"},
		"too broad":     {URL: "http://proxy.corp:3128", Hosts: []string{"*.com"}},
		"a star alone":  {URL: "http://proxy.corp:3128", Hosts: []string{"*"}},
		"an address":    {URL: "http://proxy.corp:3128", Hosts: []string{"10.1.2.3"}},
		"a star inside": {URL: "http://proxy.corp:3128", Hosts: []string{"s3.*.amazonaws.com"}},
		"user in url":   {URL: "http://u:p@proxy.corp:3128", Hosts: []string{"*.amazonaws.com"}},
		"loose file":    {URL: "http://proxy.corp:3128", Hosts: []string{"*.amazonaws.com"}, CredentialsFile: credentials(t, 0o644)},
		"no such file":  {URL: "http://proxy.corp:3128", Hosts: []string{"*.amazonaws.com"}, CredentialsFile: "/nonexistent"},
	} {
		if _, err := NewProxy(&s); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	p, err := NewProxy(&ProxySettings{URL: "http://proxy.corp:3128", Hosts: []string{"*.AmazonAWS.com", "s3.backup.example.net."}, CredentialsFile: good})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		scheme, host string
		want         bool
	}{
		{"https", "bucket.s3.eu-west-1.amazonaws.com", true}, {"https", "S3.Backup.Example.Net.", true},
		{"https", "amazonaws.com", false}, {"https", "evilamazonaws.com", false}, {"https", "other.example.net", false},
		{"https", "52.95.1.1", false}, {"https", "169.254.169.254", false}, {"https", "::1", false},
		{"http", "bucket.s3.amazonaws.com", false},
	} {
		if got := p.Routes(tc.scheme, tc.host); got != tc.want {
			t.Errorf("Routes(%s, %s) = %v", tc.scheme, tc.host, got)
		}
	}
}

// PX-T2, PX-T3: a listed name over https goes through the proxy unresolved,
// TLS verified for the name through the tunnel, the credentials to the
// proxy only; an unlisted name and an IP literal are dialled directly
// through the guard; a listed name over http is refused; a refusal of the
// proxy is a ProxyError with its status.
func TestProxyTransport(t *testing.T) {
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "auth="+r.Header.Get("Proxy-Authorization"))
	}))
	t.Cleanup(target.Close)
	_, port, _ := net.SplitHostPort(target.Listener.Addr().String())
	fp := newFakeProxy(t, map[string]string{"example.com": target.Listener.Addr().String()})
	p, err := NewProxy(&ProxySettings{URL: "http://" + fp.ln.Addr().String(), Hosts: []string{"example.com"}, CredentialsFile: credentials(t, 0o600)})
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(target.Certificate())
	public, _ := New(nil) // the loopback target is refused when dialled directly
	tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}
	p.Apply(tr, public, 5*time.Second)
	hc := &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	get := func(u string) (string, error) {
		resp, err := hc.Get(u)
		if err != nil {
			return "", err
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return string(b), nil
	}

	body, err := get("https://example.com:" + port + "/")
	if err != nil || body != "auth=" {
		t.Fatalf("through the proxy: %q, %v", body, err)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("veho:s3cret"))
	if seen := fp.targets(); len(seen) != 1 || seen[0] != "example.com:"+port || fp.auth[0] != want {
		t.Fatalf("the proxy saw %v, %v", seen, fp.auth)
	}
	for _, u := range []string{"https://127.0.0.1:" + port + "/", "https://169.254.169.254/", "https://unlisted.example:" + port + "/"} {
		if _, err := get(u); !errors.Is(err, ErrForbidden) && !strings.Contains(err.Error(), "no such host") {
			t.Errorf("%s: %v, want direct and refused", u, err)
		}
	}
	if _, err := get("http://example.com:" + port + "/"); !errors.Is(err, ErrPlainProxied) {
		t.Errorf("plain http to a listed name: %v", err)
	}
	if n := len(fp.targets()); n != 1 {
		t.Errorf("the proxy saw %d requests, want 1", n)
	}
	for _, code := range []int{http.StatusForbidden, http.StatusProxyAuthRequired, http.StatusBadGateway} {
		fp.mu.Lock()
		fp.refuse = code
		fp.mu.Unlock()
		tr.CloseIdleConnections()
		_, err := get("https://example.com:" + port + "/")
		var pe *ProxyError
		if !errors.As(err, &pe) || pe.Status != code || pe.Permanent() != (code != http.StatusBadGateway) {
			t.Errorf("refused %d: %v", code, err)
		}
	}
	// the context still bounds a proxied request
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://example.com:"+port+"/", nil)
	if _, err := hc.Do(req); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %v", err)
	}
}
