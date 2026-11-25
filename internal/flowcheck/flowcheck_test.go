package flowcheck

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dihedron/veho/internal/netguard"
	"github.com/dihedron/veho/pkg/plugin"
	"golang.org/x/crypto/ssh"
)

// listener counts the connections it accepts.
type listener struct {
	net.Listener
	n atomic.Int32
}

func listen(t *testing.T) *listener {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	ln := &listener{Listener: l}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			ln.n.Add(1)
			_ = c.Close()
		}
	}()
	return ln
}

func port(t *testing.T, addr string) int {
	t.Helper()
	_, p, _ := net.SplitHostPort(addr)
	n, _ := strconv.Atoi(p)
	return n
}

func loopback(t *testing.T) Options {
	t.Helper()
	g, err := netguard.New([]string{"127.0.0.0/8"})
	if err != nil {
		t.Fatal(err)
	}
	return Options{Guard: g, Timeout: time.Second, Retries: 1}
}

func one(t *testing.T, f plugin.Flow, o Options) Result {
	t.Helper()
	r := Check(context.Background(), []plugin.Flow{f}, o)
	if len(r) != 1 {
		t.Fatalf("%d results: %+v", len(r), r)
	}
	return r[0]
}

// NC-T2: reachable and unreachable over TCP.
func TestTCP(t *testing.T) {
	l := listen(t)
	r := one(t, plugin.Flow{Purpose: "x", Host: "127.0.0.1", Port: port(t, l.Addr().String())}, loopback(t))
	if r.Status != Reachable || r.Protocol != "tcp" || r.Address != "127.0.0.1" {
		t.Fatalf("open port: %+v", r)
	}
	closed := listen(t)
	p := port(t, closed.Addr().String())
	_ = closed.Close()
	if r := one(t, plugin.Flow{Purpose: "x", Host: "127.0.0.1", Port: p}, loopback(t)); r.Status != Unreachable || r.Reason == "" {
		t.Fatalf("closed port: %+v", r)
	}
}

// NC-T2: TLS verified with the flow's CA and server name.
func TestTLS(t *testing.T) {
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	ca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
	flow := plugin.Flow{Purpose: "s3", Host: "127.0.0.1", Port: port(t, u.Host), TLS: true, ServerName: "example.com", CACert: ca}
	if r := one(t, flow, loopback(t)); r.Status != Reachable || r.Protocol != "tls" {
		t.Fatalf("trusted: %+v", r)
	}
	wrong := flow
	wrong.ServerName = "other.example"
	if r := one(t, wrong, loopback(t)); r.Status != Unreachable {
		t.Fatalf("another name: %+v", r)
	}
	untrusted := flow
	untrusted.CACert = ""
	if r := one(t, untrusted, loopback(t)); r.Status != Unreachable {
		t.Fatalf("untrusted: %+v", r)
	}
}

// NC-T2: SSH's handshake.
func TestSSH(t *testing.T) {
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(key)
	cfg := &ssh.ServerConfig{PasswordCallback: func(ssh.ConnMetadata, []byte) (*ssh.Permissions, error) { return nil, errors.New("no") }}
	cfg.AddHostKey(signer)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() { _, _, _, _ = ssh.NewServerConn(c, cfg) }()
		}
	}()
	if r := one(t, plugin.Flow{Purpose: "sftp", Host: "127.0.0.1", Port: port(t, l.Addr().String()), Handshake: "ssh"}, loopback(t)); r.Status != Reachable || r.Protocol != "ssh" {
		t.Fatalf("ssh: %+v", r)
	}
	plain := listen(t) // closes at once: no SSH banner
	if r := one(t, plugin.Flow{Purpose: "sftp", Host: "127.0.0.1", Port: port(t, plain.Addr().String()), Handshake: "ssh"}, loopback(t)); r.Status != Unreachable {
		t.Fatalf("not ssh: %+v", r)
	}
}

// NC-T2, NC-1: an address the guard refuses is skipped, never dialled, and
// its result does not name it; the metadata addresses even when every
// range is allowed.
func TestGuard(t *testing.T) {
	l := listen(t)
	p := port(t, l.Addr().String())
	public, _ := netguard.New(nil)
	o := Options{Guard: public, Timeout: time.Second, Retries: 1}
	r := one(t, plugin.Flow{Purpose: "x", Host: "127.0.0.1", Port: p}, o)
	if r.Status != Skipped || r.Address != "" || l.n.Load() != 0 {
		t.Fatalf("refused: %+v, %d connections", r, l.n.Load())
	}
	all, _ := netguard.New([]string{"0.0.0.0/0", "::/0"})
	o.Guard = all
	if r := one(t, plugin.Flow{Purpose: "s3", Host: "127.0.0.1", Port: p}, o); r.Status != Reachable {
		t.Fatalf("every range allowed: %+v", r)
	}
	if r := one(t, plugin.Flow{Purpose: "s3", Host: "169.254.169.254", Port: 80}, o); r.Status != Skipped {
		t.Fatalf("metadata, every range allowed: %+v", r)
	}
}

// NC-T2: unprobed flows are skipped with their reason; a name resolves to
// at most 4 addresses, each probed; a name that does not resolve is
// unreachable.
func TestAddresses(t *testing.T) {
	l := listen(t)
	p := port(t, l.Addr().String())
	o := loopback(t)
	o.Resolve = func(_ context.Context, host string) ([]netip.Addr, error) {
		if host == "nowhere.example" {
			return nil, errors.New("no such host")
		}
		var as []netip.Addr
		for i := 1; i <= 6; i++ {
			as = append(as, netip.AddrFrom4([4]byte{127, 0, 0, byte(i)}))
		}
		return as, nil
	}
	rs := Check(context.Background(), []plugin.Flow{
		{Purpose: "s3", Host: "storage.example", Port: p},
		{Purpose: "ftp data", Host: "ftp.example", Unprobed: "passive ports"},
		{Purpose: "s3", Host: "nowhere.example", Port: p},
	}, o)
	var probed, skipped, unresolved int
	for _, r := range rs {
		switch {
		case r.Host == "storage.example":
			probed++
		case r.Host == "ftp.example" && r.Status == Skipped && r.Reason == "passive ports":
			skipped++
		case r.Host == "nowhere.example" && r.Status == Unreachable:
			unresolved++
		}
	}
	if probed != MaxAddresses || skipped != 1 || unresolved != 1 {
		t.Fatalf("%d probed, %d skipped, %d unresolved: %+v", probed, skipped, unresolved, rs)
	}
}

// NC-T2: two flows to the same place are probed once; the context ends the
// run.
func TestSharedAndCancelled(t *testing.T) {
	l := listen(t)
	p := port(t, l.Addr().String())
	rs := Check(context.Background(), []plugin.Flow{{Purpose: "keystone", Host: "127.0.0.1", Port: p}, {Purpose: "swift", Host: "127.0.0.1", Port: p}}, loopback(t))
	if len(rs) != 2 || rs[0].Status != Reachable || rs[1].Status != Reachable || l.n.Load() != 1 {
		t.Fatalf("%+v, %d connections", rs, l.n.Load())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if r := Check(ctx, []plugin.Flow{{Purpose: "x", Host: "127.0.0.1", Port: p}}, loopback(t)); r[0].Status != Skipped {
		t.Fatalf("cancelled: %+v", r)
	}
}

func FuzzCheck(f *testing.F) {
	f.Add("storage.example", 443, true, "", "")
	f.Add("10.0.0.1", 22, false, "ssh", "")
	f.Add("[::1]", 0, false, "", "ports")
	g, _ := netguard.New(nil)
	f.Fuzz(func(t *testing.T, host string, p int, tls bool, handshake, unprobed string) {
		if _, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
			return // a literal address is not resolved: it could be a real host
		}
		o := Options{Guard: g, Timeout: time.Millisecond, Retries: 1,
			Resolve: func(context.Context, string) ([]netip.Addr, error) {
				return []netip.Addr{netip.MustParseAddr("192.0.2.1")}, nil
			}}
		rs := Check(context.Background(), []plugin.Flow{{Purpose: "f", Host: host, Port: p, TLS: tls, Handshake: handshake, Unprobed: unprobed}}, o)
		for _, r := range rs {
			if r.Status != Skipped && r.Status != Unreachable && r.Status != Reachable {
				t.Fatalf("%+v", r)
			}
			if r.Status == Reachable {
				t.Fatalf("reachable through a refused address: %+v", r)
			}
		}
	})
}
