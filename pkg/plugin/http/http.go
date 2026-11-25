// Package http is the built-in source driver of objects on HTTP and HTTPS
// servers (.specs/veho-sources.md §3.1): read-only, by ranges, each range
// conditional on the version Stat returned. Standard library only, so it is
// compiled into veho (veho-plugin-distribution.md §1).
package http

import (
	"context"
	"crypto/tls"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	nethttp "net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/dihedron/veho/internal/netguard"
	"github.com/dihedron/veho/pkg/plugin"
)

// Timeouts of a connection; the whole of a call is bounded by its context.
const (
	dialTimeout   = 10 * time.Second
	headerTimeout = 60 * time.Second
)

// Driver reads objects from HTTP servers, through the dial guard.
type Driver struct {
	guard *netguard.Guard
	// tlsConfig is for tests, which trust their own server
	tlsConfig *tls.Config
}

// settings are the platform's (veho-sources.md §5).
type settings struct {
	AllowCIDRs []string `json:"allow_cidrs"`
}

// Factory builds the driver from its settings: allow_cidrs, the ranges the
// dial guard allows besides the public addresses.
func Factory(raw json.RawMessage) (plugin.Driver, error) {
	var s settings
	if err := plugin.DecodeSettings(raw, &s); err != nil {
		return nil, fmt.Errorf("http: %w", err)
	}
	g, err := netguard.New(s.AllowCIDRs)
	if err != nil {
		return nil, fmt.Errorf("http: %w", err)
	}
	return Driver{guard: g}, nil
}

func (Driver) Name() string      { return "http" }
func (Driver) Schemes() []string { return []string{"http", "https"} }

func (Driver) Capabilities() plugin.Capabilities {
	return plugin.Capabilities{Source: true, RangeRead: true}
}

// schema is the endpoint configuration's JSON Schema (PLG-10).
//
//go:embed schema.json
var schema []byte

func (Driver) ConfigSchema() []byte { return schema }

// ParseRef parses an http(s) URL: the key is its path, without the leading
// slash; it is relative to the endpoint's base_url.
func (Driver) ParseRef(raw string) (plugin.Ref, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return plugin.Ref{}, plugin.Wrapf(plugin.Permanent, "http: %q is not an http(s) URL", raw)
	}
	key := strings.TrimPrefix(u.Path, "/")
	if err := checkKey(key); err != nil {
		return plugin.Ref{}, err
	}
	return plugin.Ref{Key: key}, nil
}

type config struct {
	BaseURL string            `json:"base_url"`
	Headers map[string]string `json:"headers"`
}

// reserved are the headers the driver sets itself, or that would carry
// credentials outside the secret store.
var reserved = map[string]bool{
	"Authorization": true, "Proxy-Authorization": true, "Cookie": true, "Host": true, "Range": true,
	"If-Match": true, "If-None-Match": true, "If-Modified-Since": true, "If-Unmodified-Since": true, "If-Range": true,
	"Content-Length": true, "Transfer-Encoding": true, "Connection": true, "Te": true, "Upgrade": true,
}

// Open checks the configuration and the credentials: bearer_token, or
// username and password, only over HTTPS (SRC-W2).
func (d Driver) Open(ctx context.Context, cfg plugin.Config, creds plugin.Credentials) (plugin.Client, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var c config
	dec := json.NewDecoder(strings.NewReader(string(cfg)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, plugin.Wrapf(plugin.Permanent, "http: configuration: %v", err)
	}
	base, err := url.Parse(c.BaseURL)
	switch {
	case err != nil:
		return nil, plugin.Wrapf(plugin.Permanent, "http: base_url: %v", err)
	case base.Scheme != "http" && base.Scheme != "https":
		return nil, plugin.Wrapf(plugin.Permanent, "http: base_url: want http or https")
	case base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" || strings.Contains(c.BaseURL, "#"):
		return nil, plugin.Wrapf(plugin.Permanent, "http: base_url: want a host, no user information, query or fragment")
	}
	if !strings.HasSuffix(base.Path, "/") {
		base.Path += "/"
		base.RawPath = ""
	}
	header := nethttp.Header{}
	for k, v := range c.Headers {
		ck := nethttp.CanonicalHeaderKey(k)
		if reserved[ck] || strings.ContainsAny(k+v, "\r\n\x00") || k == "" {
			return nil, plugin.Wrapf(plugin.Permanent, "http: header %q is not allowed", k)
		}
		header.Set(ck, v)
	}
	for k := range creds {
		if k != "bearer_token" && k != "username" && k != "password" {
			return nil, plugin.Wrapf(plugin.Permanent, "http: unknown credential %q", k)
		}
	}
	switch {
	case creds["bearer_token"] != "" && creds["username"] == "" && creds["password"] == "":
		header.Set("Authorization", "Bearer "+creds["bearer_token"])
	case creds["bearer_token"] == "" && creds["username"] != "" && creds["password"] != "":
		r := &nethttp.Request{Header: nethttp.Header{}}
		r.SetBasicAuth(creds["username"], creds["password"])
		header.Set("Authorization", r.Header.Get("Authorization"))
	case len(creds) != 0:
		return nil, plugin.Wrapf(plugin.Permanent, "http: credentials: bearer_token, or username and password")
	}
	if header.Get("Authorization") != "" && base.Scheme != "https" {
		return nil, plugin.Wrapf(plugin.Permanent, "http: credentials are sent only over https")
	}
	guard := d.guard
	if guard == nil {
		if guard, err = netguard.New(nil); err != nil {
			return nil, err
		}
	}
	tlsConfig := d.tlsConfig
	if tlsConfig == nil {
		tlsConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	hc := &nethttp.Client{
		Transport: &nethttp.Transport{
			Proxy:                 nil,
			DialContext:           guard.Dialer(dialTimeout).DialContext,
			TLSClientConfig:       tlsConfig,
			TLSHandshakeTimeout:   dialTimeout,
			ResponseHeaderTimeout: headerTimeout,
			MaxIdleConnsPerHost:   4,
			IdleConnTimeout:       90 * time.Second,
		},
		// no redirects: a 3xx is an answer (SEC-13, T-29)
		CheckRedirect: func(*nethttp.Request, []*nethttp.Request) error { return nethttp.ErrUseLastResponse },
	}
	return &client{base: base, header: header, hc: hc}, nil
}

// client is the driver bound to one base URL.
type client struct {
	base   *url.URL
	header nethttp.Header
	hc     *nethttp.Client
}

func (c *client) Close() error {
	c.hc.CloseIdleConnections()
	return nil
}

// PresignRead is the object's URL, for a destination that pulls it: only
// when the endpoint sends no credentials or headers, which the destination
// would not send. The version is not in the URL: the engine reads the copy
// back and compares it with the source (veho-openstack.md §3.2).
func (c *client) PresignRead(ctx context.Context, ref plugin.Ref, _ string, _ time.Duration) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if len(c.header) > 0 {
		return "", plugin.Wrapf(plugin.Permanent, "http: an endpoint with credentials or headers cannot give a URL to another service")
	}
	return locate(c.base, ref.Key)
}

// checkKey refuses a key that is not a clean relative path of non-empty
// segments (SRC-W4); dot segments, a backslash and control characters
// are refused, every other byte is escaped as part of the path.
func checkKey(key string) error {
	if key == "" || strings.HasPrefix(key, "/") || strings.ContainsAny(key, "\\") {
		return plugin.Wrapf(plugin.Permanent, "http: invalid key %q", key)
	}
	for _, seg := range strings.Split(key, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return plugin.Wrapf(plugin.Permanent, "http: invalid key %q", key)
		}
	}
	for _, r := range key {
		if r < 0x20 || r == 0x7f {
			return plugin.Wrapf(plugin.Permanent, "http: invalid key %q", key)
		}
	}
	return nil
}

// locate is the URL of key under base: each segment escaped, so that "%",
// "?" and "#" are part of the name and the URL stays under base.
func locate(base *url.URL, key string) (string, error) {
	if err := checkKey(key); err != nil {
		return "", err
	}
	segs := strings.Split(key, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	u := *base
	u.RawPath = base.EscapedPath() + strings.Join(segs, "/")
	p, err := url.PathUnescape(u.RawPath)
	if err != nil {
		return "", plugin.Wrapf(plugin.Permanent, "http: invalid key %q", key)
	}
	u.Path = p
	return u.String(), nil
}

func (c *client) request(ctx context.Context, method string, ref plugin.Ref) (*nethttp.Request, error) {
	if ref.Container != "" {
		return nil, plugin.Wrapf(plugin.Permanent, "http: no containers (got %q)", ref.Container)
	}
	u, err := locate(c.base, ref.Key)
	if err != nil {
		return nil, err
	}
	req, err := nethttp.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return nil, plugin.Wrapf(plugin.Permanent, "http: %v", err)
	}
	for k, v := range c.header {
		req.Header[k] = v
	}
	return req, nil
}

// do sends req and classifies a failure to get an answer.
func (c *client) do(ctx context.Context, req *nethttp.Request) (*nethttp.Response, error) {
	resp, err := c.hc.Do(req)
	if err == nil {
		return resp, nil
	}
	switch {
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case errors.Is(err, netguard.ErrForbidden):
		return nil, plugin.Wrap(fmt.Errorf("http: %w", netguard.ErrForbidden), plugin.Permanent)
	}
	var cert *tls.CertificateVerificationError
	if errors.As(err, &cert) {
		return nil, plugin.Wrap(fmt.Errorf("http: the server's certificate: %w", cert.Err), plugin.Permanent)
	}
	return nil, plugin.Wrap(fmt.Errorf("http: %s %s: %w", req.Method, req.URL.Redacted(), err), plugin.Transient)
}

// status classifies an answer that is not the expected one.
func status(resp *nethttp.Response, ref plugin.Ref) error {
	code := resp.StatusCode
	switch {
	case code == nethttp.StatusNotFound || code == nethttp.StatusGone:
		return fmt.Errorf("http: %s: %w", ref.Key, plugin.ErrNotFound)
	case code == nethttp.StatusPreconditionFailed:
		return plugin.Wrapf(plugin.SourceChanged, "http: %s changed", ref.Key)
	case code/100 == 3:
		return plugin.Wrapf(plugin.Permanent, "http: %s: a redirect (%d) to %q: redirects are not followed", ref.Key, code, resp.Header.Get("Location"))
	case code == nethttp.StatusTooManyRequests || code == nethttp.StatusRequestTimeout || code/100 == 5:
		return plugin.Wrapf(plugin.Transient, "http: %s: status %d", ref.Key, code)
	}
	return plugin.Wrapf(plugin.Permanent, "http: %s: status %d", ref.Key, code)
}

func discard(resp *nethttp.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()
}

// version is the strong ETag, else Last-Modified with the size; empty when
// the answer has neither (a weak ETag is no version, SRC-1).
func version(h nethttp.Header, size int64) string {
	if etag := h.Get("ETag"); etag != "" && !strings.HasPrefix(etag, "W/") {
		return "etag:" + etag
	}
	if lm := h.Get("Last-Modified"); lm != "" {
		if _, err := nethttp.ParseTime(lm); err == nil {
			return "lm:" + lm + ":" + strconv.FormatInt(size, 10)
		}
	}
	return ""
}

// Stat is a HEAD: the size from Content-Length, ranges required, and the
// version (SRC-1, veho-sources.md §2 decision 2).
func (c *client) Stat(ctx context.Context, ref plugin.Ref) (plugin.ObjectInfo, error) {
	req, err := c.request(ctx, nethttp.MethodHead, ref)
	if err != nil {
		return plugin.ObjectInfo{}, err
	}
	resp, err := c.do(ctx, req)
	if err != nil {
		return plugin.ObjectInfo{}, err
	}
	defer discard(resp)
	if resp.StatusCode != nethttp.StatusOK {
		return plugin.ObjectInfo{}, status(resp, ref)
	}
	switch {
	case resp.ContentLength < 0:
		return plugin.ObjectInfo{}, plugin.Wrapf(plugin.Permanent, "http: %s: the server gives no Content-Length", ref.Key)
	case resp.Header.Get("Accept-Ranges") != "bytes":
		return plugin.ObjectInfo{}, plugin.Wrapf(plugin.Permanent, "http: %s: the server does not serve ranges (Accept-Ranges: bytes), so a copy could not resume", ref.Key)
	}
	v := version(resp.Header, resp.ContentLength)
	if v == "" {
		return plugin.ObjectInfo{}, plugin.Wrapf(plugin.Permanent, "http: %s: the server gives neither a strong ETag nor Last-Modified, so a change could not be detected", ref.Key)
	}
	return plugin.ObjectInfo{Size: resp.ContentLength, Version: v}, nil
}

// ReadRange is a GET of the range, conditional on ifVersion; only a 206 of
// exactly the range, of that version, is read (SRC-2).
func (c *client) ReadRange(ctx context.Context, ref plugin.Ref, off, length int64, ifVersion string) (io.ReadCloser, error) {
	if off < 0 || length < 0 {
		return nil, plugin.Wrapf(plugin.Permanent, "http: range [%d, +%d)", off, length)
	}
	if length == 0 {
		if ifVersion != "" {
			info, err := c.Stat(ctx, ref)
			if err != nil {
				return nil, err
			}
			if info.Version != ifVersion {
				return nil, plugin.Wrapf(plugin.SourceChanged, "http: %s changed", ref.Key)
			}
		}
		return io.NopCloser(strings.NewReader("")), nil
	}
	req, err := c.request(ctx, nethttp.MethodGet, ref)
	if err != nil {
		return nil, err
	}
	end := off + length - 1
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", off, end))
	var size int64 = -1
	switch {
	case strings.HasPrefix(ifVersion, "etag:"):
		req.Header.Set("If-Match", strings.TrimPrefix(ifVersion, "etag:"))
	case strings.HasPrefix(ifVersion, "lm:"):
		rest := strings.TrimPrefix(ifVersion, "lm:")
		i := strings.LastIndexByte(rest, ':')
		if i < 0 {
			return nil, plugin.Wrapf(plugin.Permanent, "http: version %q", ifVersion)
		}
		if size, err = strconv.ParseInt(rest[i+1:], 10, 64); err != nil {
			return nil, plugin.Wrapf(plugin.Permanent, "http: version %q", ifVersion)
		}
		req.Header.Set("If-Unmodified-Since", rest[:i])
	case ifVersion != "":
		return nil, plugin.Wrapf(plugin.Permanent, "http: version %q", ifVersion)
	}
	resp, err := c.do(ctx, req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != nethttp.StatusPartialContent {
		defer discard(resp)
		if resp.StatusCode == nethttp.StatusOK {
			return nil, plugin.Wrapf(plugin.Permanent, "http: %s: the server answered the whole object to a range request", ref.Key)
		}
		return nil, status(resp, ref)
	}
	var gotOff, gotEnd, total int64
	if _, err := fmt.Sscanf(resp.Header.Get("Content-Range"), "bytes %d-%d/%d", &gotOff, &gotEnd, &total); err != nil || gotOff != off || gotEnd != end {
		discard(resp)
		return nil, plugin.Wrapf(plugin.Permanent, "http: %s: Content-Range %q, want bytes %d-%d", ref.Key, resp.Header.Get("Content-Range"), off, end)
	}
	if ifVersion != "" {
		// for servers that ignore the conditional headers
		if size < 0 {
			size = total
		}
		if v := version(resp.Header, size); v != ifVersion || total != size {
			discard(resp)
			return nil, plugin.Wrapf(plugin.SourceChanged, "http: %s changed", ref.Key)
		}
	}
	if resp.ContentLength >= 0 && resp.ContentLength != length {
		discard(resp)
		return nil, plugin.Wrapf(plugin.Permanent, "http: %s: Content-Length %d, want %d", ref.Key, resp.ContentLength, length)
	}
	return &exact{body: resp.Body, left: length, key: ref.Key}, nil
}

// exact reads exactly the range: no byte beyond it, and fewer is an error
// (SRC-W5).
type exact struct {
	body io.ReadCloser
	left int64
	key  string
}

func (e *exact) Read(p []byte) (int, error) {
	if e.left == 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > e.left {
		p = p[:e.left]
	}
	n, err := e.body.Read(p)
	e.left -= int64(n)
	if errors.Is(err, io.EOF) && e.left > 0 {
		return n, plugin.Wrapf(plugin.Transient, "http: %s: the answer ended %d bytes short", e.key, e.left)
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return n, plugin.Wrap(fmt.Errorf("http: %s: %w", e.key, err), plugin.Transient)
	}
	if e.left == 0 {
		return n, io.EOF
	}
	return n, nil
}

func (e *exact) Close() error { return e.body.Close() }

// The driver is a source only.

func (c *client) Begin(context.Context, plugin.Ref, plugin.ObjectInfo, plugin.PutOptions) (plugin.Upload, error) {
	return nil, plugin.Wrapf(plugin.Permanent, "http: a source only")
}

func (c *client) Resume(context.Context, plugin.Ref, []byte) (plugin.Upload, error) {
	return nil, plugin.Wrapf(plugin.Permanent, "http: a source only")
}

func (c *client) Delete(context.Context, plugin.Ref) error {
	return plugin.Wrapf(plugin.Permanent, "http: a source only")
}
