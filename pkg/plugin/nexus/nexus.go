// Package nexus is the Sonatype Nexus Repository 3 driver (.specs/veho-nexus.md):
// a source of any repository by content path, and a destination of raw
// hosted repositories, one PUT per attempt. The endpoint's format decides
// what a key means (§2.1); raw, or none, is the one implemented.
package nexus

import (
	"bytes"
	"context"
	"crypto/sha1" //nolint:gosec // Nexus's ETag is the content's SHA-1
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/dihedron/veho/internal/netguard"
	"github.com/dihedron/veho/pkg/plugin"
	httpdriver "github.com/dihedron/veho/pkg/plugin/http"
	"github.com/dihedron/veho/pkg/plugin/stream"
)

// Timeouts of a connection; the whole of a call is bounded by its context.
const (
	dialTimeout   = 10 * time.Second
	headerTimeout = 60 * time.Second
)

// Driver is the Nexus driver under one platform's settings.
type Driver struct {
	guard     *netguard.Guard
	plaintext bool
}

type settings struct {
	AllowCIDRs []string `json:"allow_cidrs"`
	Plaintext  bool     `json:"plaintext"`
}

// Factory builds the driver from its settings: allow_cidrs and plaintext.
func Factory(raw json.RawMessage) (plugin.Driver, error) {
	var s settings
	if err := plugin.DecodeSettings(raw, &s); err != nil {
		return nil, fmt.Errorf("nexus: %w", err)
	}
	g, err := netguard.New(s.AllowCIDRs)
	if err != nil {
		return nil, fmt.Errorf("nexus: %w", err)
	}
	return Driver{guard: g, plaintext: s.Plaintext}, nil
}

func (Driver) Name() string      { return "nexus" }
func (Driver) Schemes() []string { return []string{"nexus"} }

// Capabilities: an upload is one request, not resumable.
func (Driver) Capabilities() plugin.Capabilities {
	return plugin.Capabilities{Source: true, Sink: true, RangeRead: true}
}

//go:embed schema.json
var schema []byte

func (Driver) ConfigSchema() []byte { return schema }

// The formats of §2.1: raw (and none) implemented, the others planned.
var planned = map[string]bool{"maven2": true, "npm": true, "pypi": true}

func checkRepository(name string) error {
	if name == "" || len(name) > 200 || strings.ContainsAny(name, "/\\%?#") || name == "." || name == ".." ||
		strings.ContainsFunc(name, func(r rune) bool { return r < 0x21 || r == 0x7f }) {
		return plugin.Wrapf(plugin.Permanent, "nexus: repository %q is not allowed", name)
	}
	return nil
}

// ParseRef parses nexus://<repository>/<key>; the key's syntax is the
// endpoint's format's, checked by the client.
func (Driver) ParseRef(raw string) (plugin.Ref, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "nexus" || u.RawQuery != "" || u.Fragment != "" {
		return plugin.Ref{}, plugin.Wrapf(plugin.Permanent, "nexus: %q is not nexus://repository/key", raw)
	}
	if err := checkRepository(u.Host); err != nil {
		return plugin.Ref{}, err
	}
	ref := plugin.Ref{Container: u.Host, Key: strings.TrimPrefix(u.Path, "/")}
	if _, err := httpdriver.Locate(&url.URL{Scheme: "https", Host: "x", Path: "/"}, ref.Key); err != nil {
		return plugin.Ref{}, plugin.Wrapf(plugin.Permanent, "nexus: invalid key %q", ref.Key)
	}
	return ref, nil
}

type config struct {
	URL        string `json:"url"`
	Repository string `json:"repository"`
	Format     string `json:"format"`
	CACert     string `json:"ca_cert"`
}

// Open checks the configuration and, with a format, that the repository
// has it.
func (d Driver) Open(ctx context.Context, raw plugin.Config, creds plugin.Credentials) (plugin.Client, error) {
	var cfg config
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, plugin.Wrapf(plugin.Permanent, "nexus: configuration: %v", err)
	}
	switch {
	case planned[cfg.Format]:
		return nil, plugin.Wrapf(plugin.Permanent, "nexus: format %s is not supported yet (veho-nexus.md §2.1)", cfg.Format)
	case cfg.Format != "" && cfg.Format != "raw":
		return nil, plugin.Wrapf(plugin.Permanent, "nexus: format: want raw, maven2, npm or pypi")
	}
	if err := checkRepository(cfg.Repository); err != nil {
		return nil, err
	}
	base, err := url.Parse(cfg.URL)
	switch {
	case err != nil || base.Host == "":
		return nil, plugin.Wrapf(plugin.Permanent, "nexus: url: want a URL")
	case base.Scheme != "https" && (base.Scheme != "http" || !d.plaintext):
		return nil, plugin.Wrapf(plugin.Permanent, "nexus: url: want https (http only with the driver's plaintext setting)")
	case base.User != nil || base.RawQuery != "" || base.Fragment != "":
		return nil, plugin.Wrapf(plugin.Permanent, "nexus: url: want no user information, query or fragment")
	}
	if !strings.HasSuffix(base.Path, "/") {
		base.Path += "/"
		base.RawPath = ""
	}
	for k := range creds {
		if k != "username" && k != "password" {
			return nil, plugin.Wrapf(plugin.Permanent, "nexus: unknown credential %q", k)
		}
	}
	if (creds["username"] == "") != (creds["password"] == "") {
		return nil, plugin.Wrapf(plugin.Permanent, "nexus: credentials: username and password (a user token's name and passcode), or none")
	}
	roots, err := x509.SystemCertPool()
	if err != nil {
		roots = x509.NewCertPool()
	}
	if cfg.CACert != "" && !roots.AppendCertsFromPEM([]byte(cfg.CACert)) {
		return nil, plugin.Wrapf(plugin.Permanent, "nexus: ca_cert: no PEM certificate")
	}
	c := &client{
		base: base, repo: cfg.Repository, format: cfg.Format, user: creds["username"], password: creds["password"],
		hc: &http.Client{
			Transport: &http.Transport{
				DialContext:           d.guard.Dialer(dialTimeout).DialContext,
				TLSClientConfig:       &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
				TLSHandshakeTimeout:   dialTimeout,
				ResponseHeaderTimeout: headerTimeout,
			},
			// credentials never leave url's host (SEC-13)
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
	if cfg.Format != "" {
		info, err := c.info(ctx)
		if err != nil {
			_ = c.Close()
			return nil, err
		}
		if info.Format != cfg.Format {
			_ = c.Close()
			return nil, plugin.Wrapf(plugin.Permanent, "nexus: repository %s is %s, not %s", c.repo, info.Format, cfg.Format)
		}
	}
	return c, nil
}

type client struct {
	base           *url.URL
	repo, format   string
	user, password string
	hc             *http.Client

	once    sync.Once
	repo0   repository
	repoErr error
}

func (c *client) Close() error {
	c.hc.CloseIdleConnections()
	return nil
}

// repository is what Nexus says of a repository.
type repository struct {
	Name   string `json:"name"`
	Format string `json:"format"`
	Type   string `json:"type"`
}

// info is the repository's format and type, asked once.
func (c *client) info(ctx context.Context) (repository, error) {
	c.once.Do(func() {
		u := c.base.JoinPath("service/rest/v1/repositories", c.repo)
		resp, err := c.do(ctx, http.MethodGet, u.String(), nil, nil, 0)
		if err != nil {
			c.repoErr = err
			return
		}
		defer discard(resp)
		if resp.StatusCode != http.StatusOK {
			if resp.StatusCode == http.StatusNotFound {
				c.repoErr = plugin.Wrapf(plugin.Permanent, "nexus: no repository %s (or no right to read it)", c.repo)
				return
			}
			c.repoErr = status(resp, "repository "+c.repo)
			return
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&c.repo0); err != nil {
			c.repoErr = plugin.Wrap(fmt.Errorf("nexus: repository %s: %w", c.repo, err), plugin.Transient)
		}
	})
	return c.repo0, c.repoErr
}

// content is the URL of the key in the repository, each segment escaped.
func (c *client) content(ref plugin.Ref) (string, error) {
	if ref.Container != "" && ref.Container != c.repo {
		return "", plugin.Wrapf(plugin.Permanent, "nexus: the endpoint's repository is %s, not %s", c.repo, ref.Container)
	}
	base := c.base.JoinPath("repository", c.repo)
	base.Path, base.RawPath = base.Path+"/", ""
	u, err := httpdriver.Locate(base, ref.Key)
	if err != nil {
		return "", plugin.Wrapf(plugin.Permanent, "nexus: invalid key %q", ref.Key)
	}
	return u, nil
}

// do sends a request with the endpoint's credentials and classifies a
// failure to get an answer.
func (c *client) do(ctx context.Context, method, u string, header http.Header, body io.Reader, length int64) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, plugin.Wrapf(plugin.Permanent, "nexus: %v", err)
	}
	if body != nil {
		req.ContentLength = length
	}
	for k, v := range header {
		req.Header[k] = v
	}
	if c.user != "" {
		req.SetBasicAuth(c.user, c.password)
	}
	resp, err := c.hc.Do(req)
	switch {
	case err == nil:
		return resp, nil
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case errors.Is(err, netguard.ErrForbidden):
		return nil, plugin.Wrap(fmt.Errorf("nexus: %w", netguard.ErrForbidden), plugin.Permanent)
	}
	var cert *tls.CertificateVerificationError
	if errors.As(err, &cert) {
		return nil, plugin.Wrap(fmt.Errorf("nexus: the server's certificate: %w", cert.Err), plugin.Permanent)
	}
	return nil, plugin.Wrap(fmt.Errorf("nexus: %s %s: %w", method, req.URL.Redacted(), err), plugin.Transient)
}

// status classifies an answer that is not the expected one.
func status(resp *http.Response, what string) error {
	code := resp.StatusCode
	switch {
	case code == http.StatusNotFound:
		return fmt.Errorf("nexus: %s: %w", what, plugin.ErrNotFound)
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return plugin.Wrapf(plugin.Permanent, "nexus: %s: not authorised (%d)", what, code)
	case code/100 == 3:
		return plugin.Wrapf(plugin.Permanent, "nexus: %s: a redirect (%d): redirects are not followed", what, code)
	case code == http.StatusTooManyRequests || code == http.StatusRequestTimeout || code/100 == 5:
		return plugin.Wrapf(plugin.Transient, "nexus: %s: status %d", what, code)
	}
	return plugin.Wrapf(plugin.Permanent, "nexus: %s: status %d", what, code)
}

func discard(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()
}

// --- source -----------------------------------------------------------------

// head is the asset's size and ETag, the version: Nexus derives it from the
// content's SHA-1 ("{SHA1{<hex>}}"), so it changes with the content.
func (c *client) head(ctx context.Context, ref plugin.Ref) (plugin.ObjectInfo, error) {
	u, err := c.content(ref)
	if err != nil {
		return plugin.ObjectInfo{}, err
	}
	resp, err := c.do(ctx, http.MethodHead, u, nil, nil, 0)
	if err != nil {
		return plugin.ObjectInfo{}, err
	}
	defer discard(resp)
	if resp.StatusCode != http.StatusOK {
		return plugin.ObjectInfo{}, status(resp, ref.Key)
	}
	etag := resp.Header.Get("ETag")
	switch {
	case resp.ContentLength < 0:
		return plugin.ObjectInfo{}, plugin.Wrapf(plugin.Permanent, "nexus: %s: no Content-Length", ref.Key)
	case etag == "" || strings.HasPrefix(etag, "W/"):
		return plugin.ObjectInfo{}, plugin.Wrapf(plugin.Permanent, "nexus: %s: no strong ETag to tell a changed asset", ref.Key)
	}
	return plugin.ObjectInfo{Size: resp.ContentLength, Version: etag}, nil
}

// Stat is the asset; its ranges are probed, so a Nexus that ignores them
// is refused before any transfer.
func (c *client) Stat(ctx context.Context, ref plugin.Ref) (plugin.ObjectInfo, error) {
	info, err := c.head(ctx, ref)
	if err != nil || info.Size == 0 {
		return info, err
	}
	r, err := c.ReadRange(ctx, ref, 0, 1, info.Version)
	if err != nil {
		return plugin.ObjectInfo{}, err
	}
	_ = r.Close()
	return info, nil
}

// ReadRange is [off, off+length) exactly; the answer's ETag must be the
// version (Nexus does not promise to honour If-Match).
func (c *client) ReadRange(ctx context.Context, ref plugin.Ref, off, length int64, ifVersion string) (io.ReadCloser, error) {
	if off < 0 || length < 0 {
		return nil, plugin.Wrapf(plugin.Permanent, "nexus: range [%d, +%d)", off, length)
	}
	if length == 0 {
		info, err := c.head(ctx, ref)
		switch {
		case err != nil:
			return nil, err
		case ifVersion != "" && info.Version != ifVersion:
			return nil, plugin.Wrapf(plugin.SourceChanged, "nexus: %s changed", ref.Key)
		case off > info.Size:
			return nil, plugin.Wrapf(plugin.Permanent, "nexus: offset %d beyond %d bytes", off, info.Size)
		}
		return io.NopCloser(bytes.NewReader(nil)), nil
	}
	u, err := c.content(ref)
	if err != nil {
		return nil, err
	}
	rng := fmt.Sprintf("bytes=%d-%d", off, off+length-1)
	resp, err := c.do(ctx, http.MethodGet, u, http.Header{"Range": {rng}}, nil, 0)
	if err != nil {
		return nil, err
	}
	switch {
	case resp.StatusCode == http.StatusNotFound && ifVersion != "":
		discard(resp)
		return nil, plugin.Wrapf(plugin.SourceChanged, "nexus: %s is gone", ref.Key)
	case resp.StatusCode == http.StatusOK:
		discard(resp)
		return nil, plugin.Wrapf(plugin.Permanent, "nexus: this Nexus does not serve ranges of %s", ref.Key)
	case resp.StatusCode != http.StatusPartialContent:
		discard(resp)
		return nil, status(resp, ref.Key)
	case ifVersion != "" && resp.Header.Get("ETag") != ifVersion:
		discard(resp)
		return nil, plugin.Wrapf(plugin.SourceChanged, "nexus: %s changed", ref.Key)
	case !strings.HasPrefix(resp.Header.Get("Content-Range"), fmt.Sprintf("bytes %d-%d/", off, off+length-1)) || resp.ContentLength != length:
		discard(resp)
		return nil, plugin.Wrapf(plugin.Permanent, "nexus: range %s of %s: answered %q", rng, ref.Key, resp.Header.Get("Content-Range"))
	}
	return resp.Body, nil
}

// PresignRead is the content URL, for a destination that pulls it: only
// for an anonymous endpoint, since the destination would send no
// credentials.
func (c *client) PresignRead(ctx context.Context, ref plugin.Ref, _ string, _ time.Duration) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if c.user != "" {
		return "", plugin.Wrapf(plugin.Permanent, "nexus: an endpoint with credentials cannot give a URL to another service")
	}
	return c.content(ref)
}

// Delete deletes the asset; one already gone is no error.
func (c *client) Delete(ctx context.Context, ref plugin.Ref) error {
	u, err := c.content(ref)
	if err != nil {
		return err
	}
	resp, err := c.do(ctx, http.MethodDelete, u, nil, nil, 0)
	if err != nil {
		return err
	}
	defer discard(resp)
	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusOK, http.StatusNotFound:
		return nil
	default:
		return status(resp, ref.Key)
	}
}

// --- destination ------------------------------------------------------------

// Begin checks that the repository is raw and hosted (once per client) and
// starts the upload: the parts go into one PUT, and Nexus creates the asset
// only when it ends.
func (c *client) Begin(ctx context.Context, ref plugin.Ref, info plugin.ObjectInfo, _ plugin.PutOptions) (plugin.Upload, error) {
	if c.user == "" {
		return nil, plugin.Wrapf(plugin.Permanent, "nexus: a destination needs credentials")
	}
	target, err := c.content(ref)
	if err != nil {
		return nil, err
	}
	repo, err := c.info(ctx)
	if err != nil {
		return nil, err
	}
	if repo.Format != "raw" || repo.Type != "hosted" {
		return nil, plugin.Wrapf(plugin.Permanent, "nexus: repository %s is %s %s: a destination is a raw hosted repository", c.repo, repo.Type, repo.Format)
	}
	u := &upload{c: c, key: ref.Key, target: target, sha1: sha1.New()} //nolint:gosec // Nexus's ETag
	u.s = stream.New("nexus: upload of "+ref.Key, info.Size, u.sha1, u.send)
	return u, nil
}

// Resume: an upload to Nexus cannot resume; the engine begins again.
func (c *client) Resume(context.Context, plugin.Ref, []byte) (plugin.Upload, error) {
	return nil, plugin.Wrapf(plugin.ResumeInvalid, "nexus: an upload to Nexus cannot resume: begin again")
}

type upload struct {
	c           *client
	key, target string
	s           *stream.Stream
	sha1        hash.Hash
	committed   *plugin.ObjectInfo
	mu          sync.Mutex
}

func (u *upload) Token() []byte {
	b, _ := json.Marshal(map[string]string{"key": u.key}) // cannot fail
	return b
}

func (u *upload) send(ctx context.Context, body io.Reader, length int64) error {
	if body == nil {
		body = http.NoBody
	}
	resp, err := u.c.do(ctx, http.MethodPut, u.target, http.Header{"Content-Type": {"application/octet-stream"}}, body, length)
	if err != nil {
		return err
	}
	defer discard(resp)
	switch resp.StatusCode {
	case http.StatusCreated, http.StatusOK, http.StatusNoContent:
		return nil
	case http.StatusBadRequest:
		// what Nexus answers to a redeploy the repository's write policy refuses
		return plugin.Wrapf(plugin.Permanent, "nexus: upload of %s refused (400): the repository may not allow redeploying an asset", u.key)
	case http.StatusRequestEntityTooLarge:
		return plugin.Wrapf(plugin.Permanent, "nexus: upload of %s: too large for Nexus or its proxy", u.key)
	}
	return status(resp, "upload of "+u.key)
}

func (u *upload) WritePart(ctx context.Context, partNo int, r io.Reader, length int64) (plugin.PartResult, error) {
	return u.s.Write(ctx, partNo, r, length)
}

func (u *upload) ListParts(context.Context) ([]plugin.PartResult, error) { return u.s.Parts(), nil }

// Commit ends the PUT and compares the SHA-1 of the bytes sent with the
// asset's ETag (NX-3).
func (u *upload) Commit(ctx context.Context, parts []plugin.PartResult) (plugin.ObjectInfo, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.committed != nil {
		return *u.committed, nil
	}
	if err := u.s.Close(parts); err != nil {
		return plugin.ObjectInfo{}, err
	}
	info, err := u.c.head(ctx, plugin.Ref{Key: u.key})
	if err != nil {
		return plugin.ObjectInfo{}, err
	}
	sum := hex.EncodeToString(u.sha1.Sum(nil))
	if !strings.Contains(strings.ToLower(info.Version), sum) {
		return plugin.ObjectInfo{}, plugin.Wrapf(plugin.Integrity, "nexus: %s: the asset's SHA-1 is not that of the bytes sent", u.key)
	}
	u.committed = &info
	return info, nil
}

// Abort stops the PUT: Nexus keeps nothing of an unfinished request.
func (u *upload) Abort(context.Context) error {
	u.s.Abort()
	return nil
}
