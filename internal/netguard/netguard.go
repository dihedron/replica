// Package netguard is the dial guard of every connection veho opens to a
// host a tenant names: webhooks and source drivers (.specs/veho-sources.md
// §4, SEC-13). The address is checked after the name is resolved, on the
// address actually dialled, so DNS rebinding cannot get past it.
package netguard

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"syscall"
	"time"
)

// ErrForbidden is an address the guard refuses.
var ErrForbidden = errors.New("the target address is not allowed")

// Guard decides which addresses may be dialled.
type Guard struct {
	allow []netip.Prefix
}

// never are the addresses no configuration opens: the metadata services.
var never = []netip.Prefix{
	netip.MustParsePrefix("169.254.169.254/32"), netip.MustParsePrefix("169.254.170.2/32"),
	netip.MustParsePrefix("fd00:ec2::254/128"), netip.MustParsePrefix("100.100.100.200/32"),
}

// special are ranges that are not public: refused unless the operator lists
// them.
var special = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24",
		"192.0.2.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/3",
		"::/128", "::1/128", "64:ff9b::/96", "100::/64", "2001:db8::/32", "fc00::/7", "fe80::/10", "ff00::/8"} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

// New is a guard that also allows the operator's CIDRs (a driver's or the
// webhooks' allow_cidrs); they never open the metadata addresses.
func New(allow []string) (*Guard, error) {
	g := &Guard{}
	for _, s := range allow {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return nil, fmt.Errorf("allow_cidrs %q: %w", s, err)
		}
		g.allow = append(g.allow, p.Masked())
	}
	return g, nil
}

func within(ps []netip.Prefix, a netip.Addr) bool {
	for _, p := range ps {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// Allowed reports whether a may be dialled.
func (g *Guard) Allowed(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsValid() || within(never, a) {
		return false
	}
	if within(g.allow, a) {
		return true
	}
	return !within(special, a) && !a.IsLoopback() && !a.IsPrivate() && !a.IsLinkLocalUnicast() && !a.IsMulticast() && !a.IsUnspecified()
}

// Control is a net.Dialer's Control: it runs after the name is resolved and
// before the connection, on the address actually dialled.
func (g *Guard) Control(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return ErrForbidden
	}
	a, err := netip.ParseAddr(host)
	if err != nil || !g.Allowed(a) {
		return ErrForbidden // without the address: a tenant may read the error
	}
	return nil
}

// Dialer is a dialer with the guard on every connection.
func (g *Guard) Dialer(timeout time.Duration) *net.Dialer {
	return &net.Dialer{Timeout: timeout, Control: g.Control}
}
