package netguard

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/dihedron/veho/pkg/plugin"
)

// ProxySettings are a driver's proxy (drivers[].settings.proxy,
// .specs/veho-proxy.md §3).
type ProxySettings struct {
	URL             string   `json:"url"`
	Hosts           []string `json:"hosts"`
	CredentialsFile string   `json:"credentials_file"`
}

// Proxy is the operator's forward proxy: https to a listed name goes
// through it by CONNECT, unresolved here; everything else is dialled
// directly through the guard (§2).
type Proxy struct {
	u        *url.URL
	addr     string // host:port, as the transport dials it
	exact    map[string]bool
	suffixes []string // ".amazonaws.com" for "*.amazonaws.com"
	auth     string   // Proxy-Authorization, if any
}

// ErrPlainProxied is plain http to a name the proxy serves: never sent.
var ErrPlainProxied = errors.New("plain http is not sent through the proxy")

// ProxyError is a CONNECT the proxy refused.
type ProxyError struct{ Status int }

func (e *ProxyError) Error() string {
	return fmt.Sprintf("the proxy refused the connection: %d %s", e.Status, http.StatusText(e.Status))
}

// Permanent: a refusal by the proxy's policy or for its credentials, not
// a failure of the proxy or of the target, which may pass.
func (e *ProxyError) Permanent() bool { return e.Status < 500 }

// host is a name as the proxy matches it.
func host(h string) string { return strings.TrimSuffix(strings.ToLower(h), ".") }

// NewProxy checks the settings; nil settings are no proxy.
func NewProxy(s *ProxySettings) (*Proxy, error) {
	if s == nil {
		return nil, nil //nolint:nilnil // no proxy is no error
	}
	u, err := url.Parse(s.URL)
	switch {
	case err != nil || u.Host == "":
		return nil, fmt.Errorf("proxy.url %q: want http://host:port", s.URL)
	case u.Scheme != "http":
		return nil, fmt.Errorf("proxy.url %q: want http:// (the tunnel is TLS with the target)", s.URL)
	case u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" && u.Path != "/":
		return nil, fmt.Errorf("proxy.url %q: want no user information, path or query (credentials go in proxy.credentials_file)", s.URL)
	case len(s.Hosts) == 0:
		return nil, errors.New("proxy.hosts: list the names the proxy reaches")
	}
	p := &Proxy{u: u, addr: u.Host, exact: map[string]bool{}}
	if u.Port() == "" {
		p.addr = net.JoinHostPort(u.Hostname(), "80")
	}
	for _, h := range s.Hosts {
		h = host(h)
		rest, wild := strings.CutPrefix(h, "*.")
		if _, err := netip.ParseAddr(rest); err == nil || rest == "" || strings.Contains(rest, "*") || wild && !strings.Contains(rest, ".") {
			return nil, fmt.Errorf("proxy.hosts %q: want a name, or *. and at least two labels (*.amazonaws.com)", h)
		}
		if wild {
			p.suffixes = append(p.suffixes, "."+rest)
		} else {
			p.exact[h] = true
		}
	}
	if s.CredentialsFile != "" {
		fi, err := os.Stat(s.CredentialsFile)
		if err != nil {
			return nil, fmt.Errorf("proxy.credentials_file: %w", err)
		}
		if fi.Mode().Perm()&0o077 != 0 {
			return nil, fmt.Errorf("proxy.credentials_file %s: mode %v, want 0600 or stricter", s.CredentialsFile, fi.Mode().Perm())
		}
		b, err := os.ReadFile(s.CredentialsFile)
		if err != nil {
			return nil, fmt.Errorf("proxy.credentials_file: %w", err)
		}
		cred := strings.TrimSpace(string(b))
		if !strings.Contains(cred, ":") {
			return nil, fmt.Errorf("proxy.credentials_file %s: want user:password", s.CredentialsFile)
		}
		p.auth = "Basic " + base64.StdEncoding.EncodeToString([]byte(cred))
	}
	return p, nil
}

// Routes says whether a request to host goes through the proxy: https, a
// name (never an address), listed (§2, decisions 2 and 3).
func (p *Proxy) Routes(scheme, h string) bool {
	if p == nil || scheme != "https" {
		return false
	}
	h = host(h)
	if _, err := netip.ParseAddr(strings.Trim(h, "[]")); err == nil {
		return false
	}
	if p.exact[h] {
		return true
	}
	for _, s := range p.suffixes {
		if strings.HasSuffix(h, s) {
			return true
		}
	}
	return false
}

// Addr is the proxy's host:port.
func (p *Proxy) Addr() string { return p.addr }

// Authorization is the Proxy-Authorization header, empty without
// credentials.
func (p *Proxy) Authorization() string { return p.auth }

// Apply sets t's routing: through the proxy what Routes says (CONNECT, the
// proxy's refusals as ProxyError), the rest dialled through g; the
// environment's proxy settings never. p may be nil: no proxy.
func (p *Proxy) Apply(t *http.Transport, g *Guard, timeout time.Duration) {
	guarded := g.Dialer(timeout)
	t.Proxy = nil
	t.DialContext = guarded.DialContext
	if p == nil {
		return
	}
	direct := &net.Dialer{Timeout: timeout} // the operator's proxy, whatever its range (§2, decision 5)
	t.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if addr == p.addr {
			return direct.DialContext(ctx, network, addr)
		}
		return guarded.DialContext(ctx, network, addr)
	}
	t.Proxy = func(r *http.Request) (*url.URL, error) {
		switch {
		case p.Routes(r.URL.Scheme, r.URL.Hostname()):
			return p.u, nil
		case r.URL.Scheme == "http" && p.Routes("https", r.URL.Hostname()):
			return nil, ErrPlainProxied
		}
		return nil, nil //nolint:nilnil // direct
	}
	if p.auth != "" {
		t.ProxyConnectHeader = http.Header{"Proxy-Authorization": {p.auth}}
	}
	t.OnProxyConnectResponse = func(_ context.Context, _ *url.URL, _ *http.Request, res *http.Response) error {
		if res.StatusCode != http.StatusOK {
			return &ProxyError{Status: res.StatusCode}
		}
		return nil
	}
}

// Classified is err's refusal by the guard or the proxy as a driver's
// error, without the address (a tenant may read it): Permanent for the
// guard, plain http and the proxy's policy, Transient for a failing proxy;
// nil if err is none of them.
func Classified(driver string, err error) error {
	var pe *ProxyError
	switch {
	case errors.Is(err, ErrForbidden):
		return plugin.Wrap(fmt.Errorf("%s: %w", driver, ErrForbidden), plugin.Permanent)
	case errors.Is(err, ErrPlainProxied):
		return plugin.Wrap(fmt.Errorf("%s: %w", driver, ErrPlainProxied), plugin.Permanent)
	case errors.As(err, &pe) && pe.Permanent():
		return plugin.Wrap(fmt.Errorf("%s: %w", driver, pe), plugin.Permanent)
	case pe != nil:
		return plugin.Wrap(fmt.Errorf("%s: %w", driver, pe), plugin.Transient)
	}
	return nil
}
