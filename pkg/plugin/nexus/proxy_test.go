package nexus

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"testing"

	"github.com/dihedron/veho/pkg/plugin"
)

// connectProxy is a CONNECT proxy whose DNS alone knows name: it is to.
func connectProxy(t *testing.T, name, to string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				req, err := http.ReadRequest(bufio.NewReader(c))
				if err != nil || req.Method != http.MethodConnect {
					return
				}
				host, _, _ := net.SplitHostPort(req.Host)
				if host != name {
					_, _ = io.WriteString(c, "HTTP/1.1 403 Forbidden\r\n\r\n")
					return
				}
				up, err := net.Dial("tcp", to)
				if err != nil {
					return
				}
				defer func() { _ = up.Close() }()
				_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\n\r\n")
				go func() { _, _ = io.Copy(up, c) }()
				_, _ = io.Copy(c, up)
			}()
		}
	}()
	return ln.Addr().String()
}

// PX-T3 for a driver: Nexus under a name only the proxy resolves, reached
// through it; the same Nexus by its address, dialled directly and refused
// by the guard.
func TestProxy(t *testing.T) {
	f := newFake(t)
	u, _ := url.Parse(f.srv.URL)
	proxy := connectProxy(t, "example.com", u.Host)
	settings, _ := json.Marshal(map[string]any{"proxy": map[string]any{"url": "http://" + proxy, "hosts": []string{"example.com"}}})
	d, err := Factory(settings)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	named := f.config("files", "")
	var cfg map[string]any
	_ = json.Unmarshal(named, &cfg)
	cfg["url"] = "https://example.com:" + u.Port()
	named, _ = json.Marshal(cfg)
	c, err := d.Open(ctx, named, good)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := put(t, c, "via/proxy", []byte("data")); err != nil {
		t.Fatalf("through the proxy: %v", err)
	}
	if info, err := c.Stat(ctx, plugin.Ref{Key: "via/proxy"}); err != nil || info.Size != 4 {
		t.Fatalf("stat through the proxy: %+v, %v", info, err)
	}
	direct, err := d.Open(ctx, f.config("files", ""), good)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := direct.Stat(ctx, plugin.Ref{Key: "via/proxy"}); plugin.ClassOf(err) != plugin.Permanent {
		t.Fatalf("the address, direct: %v", err)
	}
	if _, err := Factory(json.RawMessage(`{"proxy": {"url": "http://p:3128", "hosts": ["*.com"]}}`)); err == nil {
		t.Fatal("a pattern too broad")
	}
}
