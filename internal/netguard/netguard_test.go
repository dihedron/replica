package netguard

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"
	"time"
)

// TestAllowed: SRC-T2 and WH-T2, the addresses that are never dialled
// unless the driver's or the webhooks' allow_cidrs list them; the metadata
// addresses never.
func TestAllowed(t *testing.T) {
	g, err := New(nil)
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
	// a listed CIDR opens a private range, not the metadata addresses
	g, err = New([]string{"10.20.0.0/16", "169.254.169.254/32", "fd00:ec2::/64"})
	if err != nil {
		t.Fatal(err)
	}
	if !g.Allowed(netip.MustParseAddr("10.20.1.1")) || g.Allowed(netip.MustParseAddr("10.21.1.1")) {
		t.Error("the listed CIDR is not honoured exactly")
	}
	for _, a := range []string{"169.254.169.254", "fd00:ec2::254"} {
		if g.Allowed(netip.MustParseAddr(a)) {
			t.Errorf("the metadata address %s was allowed", a)
		}
	}
	if _, err := New([]string{"nonsense"}); err == nil {
		t.Error("a bad CIDR was accepted")
	}
}

// TestDialer: SRC-T2, the address actually dialled is the one checked,
// after the name is resolved: a name that resolves to the loopback
// ("localhost") is refused as the address itself is, so a DNS answer that
// changes between a check and the dial cannot get past the guard.
func TestDialer(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(l.Addr().String())
	ctx := context.Background()

	g, _ := New(nil)
	for _, addr := range []string{"127.0.0.1:" + port, "localhost:" + port} {
		if _, err := g.Dialer(time.Second).DialContext(ctx, "tcp", addr); !errors.Is(err, ErrForbidden) {
			t.Errorf("%s: %v, want ErrForbidden", addr, err)
		}
	}
	open, _ := New([]string{"127.0.0.0/8"})
	c, err := open.Dialer(time.Second).DialContext(ctx, "tcp", "127.0.0.1:"+port)
	if err != nil {
		t.Fatalf("an allowed address: %v", err)
	}
	_ = c.Close()
	if err := g.Control("tcp", "not an address", nil); !errors.Is(err, ErrForbidden) {
		t.Errorf("a malformed address: %v", err)
	}
}
