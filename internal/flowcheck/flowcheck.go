// Package flowcheck probes an endpoint's network flows from this node
// with netcheck (.specs/veho-network-checks.md): each host resolved here,
// each address checked with the dial guard and probed by address, so a
// resolution cannot change between the check and the probe (NC-1).
package flowcheck

import (
	"bufio"
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"time"

	"github.com/dihedron/netcheck/v2/pkg/netcheck"
	"github.com/dihedron/veho/internal/netguard"
	"github.com/dihedron/veho/pkg/plugin"
)

// MaxAddresses bounds the addresses probed per flow (NC-3).
const MaxAddresses = 4

// Status is a flow's outcome from one node at one address.
type Status string

// The outcomes: Skipped is not a failure, its Reason says why.
const (
	Reachable   Status = "reachable"
	Unreachable Status = "unreachable"
	Skipped     Status = "skipped"
)

// Result is one flow's outcome at one address.
type Result struct {
	Purpose   string `json:"purpose"`
	Host      string `json:"host"`
	Port      int    `json:"port,omitempty"`
	Address   string `json:"address,omitempty"`  // the address probed; empty when none was
	Protocol  string `json:"protocol,omitempty"` // tcp, tls, ssh
	Status    Status `json:"status"`
	Reason    string `json:"reason,omitempty"`
	LatencyMS int64  `json:"latency_ms,omitempty"`
}

// Options tune a run.
type Options struct {
	Guard   *netguard.Guard // the driver's
	Proxy   *netguard.Proxy // the driver's, or nil (veho-proxy.md §6)
	Resolve func(ctx context.Context, host string) ([]netip.Addr, error)
	Timeout time.Duration // per attempt; default 5 s
	Retries int           // attempts in total; default 2
}

// Check probes the flows and returns, in their order, one result per flow
// and address (one per flow when none was probed).
func Check(ctx context.Context, flows []plugin.Flow, o Options) []Result {
	if o.Resolve == nil {
		o.Resolve = func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		}
	}
	if o.Timeout <= 0 {
		o.Timeout = 5 * time.Second
	}
	if o.Retries <= 0 {
		o.Retries = 2
	}
	var results []Result
	var checks []netcheck.Check
	index := map[string]int{} // check key to its place in checks
	probe := map[int]int{}    // result to its check
	for _, f := range flows {
		base := Result{Purpose: f.Purpose, Host: f.Host, Port: f.Port}
		if f.Unprobed == "" && f.Handshake == "" && f.TLS && o.Proxy.Routes("https", f.Host) {
			// through the proxy, as a transfer goes: not resolved here
			r := base
			c, err := proxied(f, o)
			if err != nil {
				r.Status, r.Reason = Skipped, err.Error()
				results = append(results, r)
				continue
			}
			r.Address, r.Protocol = o.Proxy.Addr(), "tls via proxy"
			probe[len(results)] = len(checks)
			checks = append(checks, c)
			results = append(results, r)
			continue
		}
		addrs, skip := resolve(ctx, f, o)
		if skip != nil {
			results = append(results, *skip)
			continue
		}
		guard := o.Guard
		for _, a := range addrs {
			r := base
			if guard == nil || !guard.Allowed(a) {
				// without the address: whether it would answer is not told (NC-1)
				r.Status, r.Reason = Skipped, "the dial guard refuses the address"
				results = append(results, r)
				continue
			}
			c, err := check(f, a, guard, o)
			if err != nil {
				r.Status, r.Reason = Skipped, err.Error()
				results = append(results, r)
				continue
			}
			r.Address, r.Protocol = a.String(), string(c.Protocol)
			key := fmt.Sprintf("%s %s %s %d", c.Protocol, c.Address, f.ServerName, len(f.CACert))
			i, ok := index[key]
			if !ok {
				i = len(checks)
				index[key] = i
				checks = append(checks, c)
			}
			probe[len(results)] = i
			results = append(results, r)
		}
	}
	if len(checks) == 0 {
		return results
	}
	rep, err := netcheck.Run(ctx, &netcheck.Bundle{Checks: checks, Concurrency: 8},
		netcheck.WithLimits(netcheck.Limits{MaxRetries: 3, MaxTimeout: netcheck.Duration(30 * time.Second), MaxConcurrency: 8, MaxChecks: plugin.MaxFlows * MaxAddresses}))
	for ri, ci := range probe {
		r := &results[ri]
		switch {
		case err != nil:
			r.Status, r.Reason = Skipped, "not probed: "+err.Error()
		case rep.Results[ci].Status == netcheck.Passed:
			r.Status, r.LatencyMS = Reachable, rep.Results[ci].Latency.Milliseconds()
			if r.Protocol == "tcp" && r.Purpose == "ftp control" {
				r.Reason = "TCP only: explicit FTPS upgrades after AUTH TLS"
			}
		case rep.Results[ci].Status == netcheck.Skipped:
			r.Status, r.Reason = Skipped, "interrupted"
		default:
			r.Status, r.Reason = Unreachable, "failed"
			if e := rep.Results[ci].Err; e != nil {
				r.Reason = e.Error()
			}
		}
	}
	return results
}

// resolve is the flow's addresses, at most MaxAddresses, or the result of
// a flow that is not probed.
func resolve(ctx context.Context, f plugin.Flow, o Options) ([]netip.Addr, *Result) {
	r := Result{Purpose: f.Purpose, Host: f.Host, Port: f.Port}
	switch {
	case f.Unprobed != "":
		r.Status, r.Reason = Skipped, f.Unprobed
		return nil, &r
	case f.Port < 1 || f.Port > 65535:
		r.Status, r.Reason = Skipped, "no port to probe"
		return nil, &r
	case ctx.Err() != nil:
		r.Status, r.Reason = Skipped, "interrupted"
		return nil, &r
	}
	if a, err := netip.ParseAddr(f.Host); err == nil {
		return []netip.Addr{a.Unmap()}, nil
	}
	addrs, err := o.Resolve(ctx, f.Host)
	if err != nil || len(addrs) == 0 {
		r.Status, r.Reason = Unreachable, "the name does not resolve here"
		return nil, &r
	}
	for i := range addrs {
		addrs[i] = addrs[i].Unmap()
	}
	slices.SortFunc(addrs, func(a, b netip.Addr) int { return a.Compare(b) })
	addrs = slices.Compact(addrs)
	return addrs[:min(len(addrs), MaxAddresses)], nil
}

// check is the netcheck check of a flow at an address, the guard on the
// address it connects to too.
func check(f plugin.Flow, a netip.Addr, guard *netguard.Guard, o Options) (netcheck.Check, error) {
	c := netcheck.Check{
		Name: f.Purpose, Address: net.JoinHostPort(a.String(), strconv.Itoa(f.Port)), Protocol: netcheck.TCP,
		Timeout: netcheck.Duration(o.Timeout), Retries: o.Retries, Wait: netcheck.Duration(200 * time.Millisecond),
		Hooks: &netcheck.Hooks{BeforeConnect: func(_ context.Context, ip netip.Addr) error {
			if !guard.Allowed(ip) {
				return netguard.ErrForbidden
			}
			return nil
		}},
	}
	switch {
	case f.Handshake == "ssh":
		c.Protocol = netcheck.SSH
	case f.Handshake != "":
		return c, fmt.Errorf("the handshake %q is not one veho probes", f.Handshake)
	case f.TLS:
		opts, err := tlsOptions(f)
		if err != nil {
			return c, err
		}
		c.Protocol, c.TLS = netcheck.TLS, opts
	}
	return c, nil
}

// tlsOptions verify the flow's server name with its CA and the system's.
func tlsOptions(f plugin.Flow) (*netcheck.TLSOptions, error) {
	roots, err := x509.SystemCertPool()
	if err != nil {
		roots = x509.NewCertPool()
	}
	if f.CACert != "" && !roots.AppendCertsFromPEM([]byte(f.CACert)) {
		return nil, errors.New("the endpoint's ca_cert has no certificate")
	}
	name := f.ServerName
	if name == "" {
		name = f.Host
	}
	return &netcheck.TLSOptions{ServerName: name, RootCAs: roots}, nil
}

// proxied is the check of a flow through the proxy: a connection to it, a
// CONNECT to the flow's host and port, then TLS with the flow's name.
func proxied(f plugin.Flow, o Options) (netcheck.Check, error) {
	opts, err := tlsOptions(f)
	if err != nil {
		return netcheck.Check{}, err
	}
	target := net.JoinHostPort(f.Host, strconv.Itoa(f.Port))
	return netcheck.Check{
		Name: f.Purpose, Address: o.Proxy.Addr(), Protocol: netcheck.TLS, TLS: opts,
		Timeout: netcheck.Duration(o.Timeout), Retries: o.Retries, Wait: netcheck.Duration(200 * time.Millisecond),
		Hooks: &netcheck.Hooks{AfterConnect: func(ctx context.Context, conn net.Conn) error {
			return connect(ctx, conn, target, o.Proxy.Authorization())
		}},
	}, nil
}

// connect asks the proxy for a tunnel to target, as Go's transport does.
func connect(ctx context.Context, conn net.Conn, target, auth string) error {
	if d, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(d)
		defer func() { _ = conn.SetDeadline(time.Time{}) }()
	}
	req := &http.Request{Method: http.MethodConnect, URL: &url.URL{Opaque: target}, Host: target, Header: http.Header{}}
	if auth != "" {
		req.Header.Set("Proxy-Authorization", auth)
	}
	if err := req.Write(conn); err != nil {
		return err
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return &netguard.ProxyError{Status: resp.StatusCode}
	}
	return nil
}
