// Package webhook is the delivery side of veho's webhooks
// (.specs/veho-webhooks.md): the signature, the checks that keep a tenant's
// URL from reaching anything internal (SEC-12, SEC-13) and one delivery.
// It knows nothing of the database: the engine owns the outbox.
package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/dihedron/veho/internal/netguard"
)

// Header names of a delivery (§3.3).
const (
	HeaderSignature = "Veho-Signature"
	HeaderEventID   = "Veho-Event-Id"
	HeaderEventType = "Veho-Event-Type"
)

// Limits (§3.3, §3.4).
const (
	// Timeout is the whole of a delivery.
	Timeout = 10 * time.Second
	// MaxURL is the longest URL accepted.
	MaxURL = 2048
	// MaxResponse is how much of an answer is read.
	MaxResponse = 1024
	// MaxAttempts is the deliveries tried before an event is abandoned.
	MaxAttempts = 12
	// Tolerance is the age of a signature a receiver accepts.
	Tolerance = 5 * time.Minute
)

// Errors.
var (
	// ErrSignature is a signature that does not match.
	ErrSignature = errors.New("webhook: bad signature")
	// ErrStale is a signature older (or newer) than the tolerance.
	ErrStale = errors.New("webhook: stale timestamp")
	// ErrForbidden is a target the guard refuses (W-1).
	ErrForbidden = netguard.ErrForbidden
)

func mac(secret []byte, t int64, body []byte) string {
	h := hmac.New(sha256.New, secret)
	h.Write([]byte(strconv.FormatInt(t, 10)))
	h.Write([]byte("."))
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

// Sign is the value of the veho-signature header: the timestamp and one v1
// signature per secret (two during a rotation, §2). The timestamp is inside
// the signed string.
func Sign(now time.Time, body []byte, secrets ...[]byte) string {
	t := now.Unix()
	parts := []string{"t=" + strconv.FormatInt(t, 10)}
	for _, s := range secrets {
		parts = append(parts, "v1="+mac(s, t, body))
	}
	return strings.Join(parts, ",")
}

// Verify is what a receiver does: the header must carry a timestamp within
// tolerance of now and a v1 signature that the secret reproduces.
func Verify(header string, body, secret []byte, now time.Time, tolerance time.Duration) error {
	var t int64
	var sigs []string
	haveT := false
	for _, part := range strings.Split(header, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		switch {
		case !ok:
		case k == "t":
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return fmt.Errorf("%w: timestamp %q", ErrSignature, v)
			}
			t, haveT = n, true
		case k == "v1":
			sigs = append(sigs, v)
		}
	}
	if !haveT || len(sigs) == 0 {
		return fmt.Errorf("%w: want t= and v1=", ErrSignature)
	}
	want := []byte(mac(secret, t, body))
	match := 0
	for _, s := range sigs {
		match |= subtle.ConstantTimeCompare(want, []byte(s))
	}
	if match != 1 {
		return ErrSignature
	}
	if d := now.Sub(time.Unix(t, 0)); d > tolerance || d < -tolerance {
		return ErrStale
	}
	return nil
}

// Backoff is the wait after the nth failed attempt: 30 s, doubling, to 1 h (§2).
func Backoff(n int) time.Duration {
	d := 30 * time.Second
	for i := 1; i < n && d < time.Hour; i++ {
		d *= 2
	}
	return min(d, time.Hour)
}

// ValidateURL accepts an https URL with a host, no user information and no
// fragment, of at most MaxURL bytes (W-1, W-9). The address is checked when
// it is dialled, not here.
func ValidateURL(raw string) error {
	if raw == "" || len(raw) > MaxURL {
		return fmt.Errorf("url: want 1 to %d bytes", MaxURL)
	}
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		return fmt.Errorf("url: %w", err)
	case u.Scheme != "https":
		return errors.New("url: only https is accepted")
	case u.Hostname() == "" || strings.ContainsAny(u.Host, " \t\r\n"):
		return errors.New("url: a host is required")
	case u.User != nil:
		return errors.New("url: user information is not accepted")
	case u.Fragment != "" || strings.Contains(raw, "#"):
		return errors.New("url: a fragment is not accepted")
	}
	return nil
}

// Guard decides which addresses a webhook may reach (W-1), with the dial
// guard of internal/netguard.
type Guard struct {
	ng *netguard.Guard
	// tlsConfig is for tests, which trust their own server
	tlsConfig *tls.Config
}

// NewGuard is a guard that also allows the operator's CIDRs
// (notifications.allow_cidrs).
func NewGuard(allow []string) (*Guard, error) {
	ng, err := netguard.New(allow)
	if err != nil {
		return nil, err
	}
	return &Guard{ng: ng}, nil
}

// WithTLS sets the TLS configuration of the deliveries; for tests, which
// trust their own receiver.
func (g *Guard) WithTLS(c *tls.Config) *Guard {
	g.tlsConfig = c
	return g
}

// Client is an HTTP client for deliveries: TLS 1.2 or later, no proxy, no
// redirect, no connection kept, the guard on every dial.
func (g *Guard) Client(timeout time.Duration) *http.Client {
	d := g.ng.Dialer(timeout)
	cfg := g.tlsConfig
	if cfg == nil {
		cfg = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:               nil,
			DialContext:         d.DialContext,
			TLSClientConfig:     cfg,
			DisableKeepAlives:   true,
			TLSHandshakeTimeout: timeout,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// Event is what one delivery carries.
type Event struct {
	ID, Type   string
	OccurredAt time.Time
	Body       []byte
}

// Result is the outcome of a delivery, as the tenant may see it (W-2): a
// class and a status, never a body or an address.
type Result struct {
	OK      bool
	Status  int
	Class   string // ok, status, forbidden, timeout, connect, tls
	Message string
}

// Deliver posts ev to rawURL, signed with the secrets, and classifies what
// happens. The guard checks the address; a redirect is a failure.
func (g *Guard) Deliver(ctx context.Context, rawURL string, ev Event, userAgent string, now time.Time, secrets ...[]byte) Result {
	if err := ValidateURL(rawURL); err != nil {
		return Result{Class: "forbidden", Message: err.Error()}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(ev.Body))
	if err != nil {
		return Result{Class: "connect", Message: "bad request"}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set(HeaderEventID, ev.ID)
	req.Header.Set(HeaderEventType, ev.Type)
	req.Header.Set(HeaderSignature, Sign(now, ev.Body, secrets...))
	resp, err := g.Client(Timeout).Do(req)
	if err != nil {
		return classify(err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, MaxResponse))
	if resp.StatusCode/100 == 2 {
		return Result{OK: true, Status: resp.StatusCode, Class: "ok"}
	}
	return Result{Status: resp.StatusCode, Class: "status", Message: "status " + strconv.Itoa(resp.StatusCode)}
}

func classify(err error) Result {
	var (
		netErr net.Error
		tlsErr *tls.CertificateVerificationError
	)
	switch {
	case errors.Is(err, ErrForbidden):
		return Result{Class: "forbidden", Message: "the target address is not allowed"}
	case errors.As(err, &tlsErr):
		return Result{Class: "tls", Message: "tls error"}
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()):
		return Result{Class: "timeout", Message: "timeout"}
	}
	return Result{Class: "connect", Message: "connect error"}
}
