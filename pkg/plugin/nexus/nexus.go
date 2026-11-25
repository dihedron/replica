// Package nexus is the Sonatype Nexus Repository 3 driver (.specs/veho-nexus.md):
// a source of any repository by content path, and a destination of hosted
// repositories, one request per attempt. The endpoint's format decides
// what a key means and how an asset is written (§2.1, §2.2).
package nexus

import (
	"bytes"
	"context"
	"crypto/sha1" //nolint:gosec // Nexus's ETag is the content's SHA-1
	"crypto/sha256"
	"crypto/sha512"
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"maps"
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
	guard      *netguard.Guard
	proxy      *netguard.Proxy // the operator's, or nil
	plaintext  bool
	maxNPMSize int64
}

type settings struct {
	AllowCIDRs []string                `json:"allow_cidrs"`
	Plaintext  bool                    `json:"plaintext"`
	MaxNPMSize int64                   `json:"max_npm_size"`
	Proxy      *netguard.ProxySettings `json:"proxy"`
}

// defaultMaxNPMSize bounds an npm upload, held in memory (NX-10).
const defaultMaxNPMSize = 100 << 20

// Factory builds the driver from its settings: allow_cidrs, plaintext and
// max_npm_size.
func Factory(raw json.RawMessage) (plugin.Driver, error) {
	var s settings
	if err := plugin.DecodeSettings(raw, &s); err != nil {
		return nil, fmt.Errorf("nexus: %w", err)
	}
	g, err := netguard.New(s.AllowCIDRs)
	if err != nil {
		return nil, fmt.Errorf("nexus: %w", err)
	}
	switch {
	case s.MaxNPMSize < 0:
		return nil, fmt.Errorf("nexus: max_npm_size %d", s.MaxNPMSize)
	case s.MaxNPMSize == 0:
		s.MaxNPMSize = defaultMaxNPMSize
	}
	p, err := netguard.NewProxy(s.Proxy)
	if err != nil {
		return nil, fmt.Errorf("nexus: %w", err)
	}
	return Driver{guard: g, proxy: p, plaintext: s.Plaintext, maxNPMSize: s.MaxNPMSize}, nil
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

// format is what a repository format changes (§2.2): what a key names,
// how it is written and deleted.
type format interface {
	// resolve is the key's asset, looked up afresh.
	resolve(ctx context.Context, c *client, key string) (asset, error)
	// sink starts the upload of the key.
	sink(ctx context.Context, c *client, key string, size int64) (*upload, error)
	// remove deletes the key; one already gone is no error.
	remove(ctx context.Context, c *client, key string) error
}

// asset is a key's content URL and, where an index gives one, the
// publisher's digest of it (algorithm, hex).
type asset struct {
	url       string
	algo, sum string
}

// formats are the implemented formats; "" is the content path.
var formats = map[string]format{"": raw{}, "raw": raw{}, "maven2": maven2{}, "npm": npm{}, "pypi": pypi{}}

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
	NPMDistTag string `json:"npm_dist_tag"`
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
	fm, known := formats[cfg.Format]
	switch {
	case !known:
		return nil, plugin.Wrapf(plugin.Permanent, "nexus: format: want raw, maven2, npm or pypi")
	case cfg.NPMDistTag != "" && cfg.Format != "npm":
		return nil, plugin.Wrapf(plugin.Permanent, "nexus: npm_dist_tag is only for format npm")
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
	transport := &http.Transport{
		TLSClientConfig:       &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:   dialTimeout,
		ResponseHeaderTimeout: headerTimeout,
	}
	d.proxy.Apply(transport, d.guard, dialTimeout) // the guard, and the operator's proxy if any (veho-proxy.md)
	c := &client{
		base: base, repo: cfg.Repository, format: cfg.Format, fm: fm, user: creds["username"], password: creds["password"],
		maxNPMSize: d.maxNPMSize, distTag: cfg.NPMDistTag, ca: cfg.CACert, assets: map[string]asset{}, verified: map[string]string{},
		hc: &http.Client{
			Transport: transport,
			// credentials never leave url's host (SEC-13)
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
	if c.distTag == "" {
		c.distTag = "latest"
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
	fm             format
	user, password string
	maxNPMSize     int64
	distTag        string
	ca             string // the endpoint's ca_cert
	hc             *http.Client

	once    sync.Once
	repo0   repository
	repoErr error

	mu       sync.Mutex
	assets   map[string]asset  // resolved keys, by key
	verified map[string]string // the ETag whose content matched the index's digest, by key
}

// Flows is Nexus's.
func (c *client) Flows(context.Context) ([]plugin.Flow, error) {
	f, err := plugin.URLFlow("nexus", c.base.String(), c.ca)
	return []plugin.Flow{f}, err
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

// root is the repository's content URL, with a final slash.
func (c *client) root() *url.URL {
	u := c.base.JoinPath("repository", c.repo)
	u.Path, u.RawPath = u.Path+"/", ""
	return u
}

// content is the URL of a path in the repository, each segment escaped.
func (c *client) content(path string) (string, error) {
	u, err := httpdriver.Locate(c.root(), path)
	if err != nil {
		return "", plugin.Wrapf(plugin.Permanent, "nexus: invalid key %q", path)
	}
	return u, nil
}

// inside checks that a URL an index gave is in the endpoint's repository
// (NX-6), resolving it against the index's own URL.
func (c *client) inside(index, link string) (string, error) {
	base, err := url.Parse(index)
	if err != nil {
		return "", plugin.Wrapf(plugin.Permanent, "nexus: %v", err)
	}
	ref, err := url.Parse(link)
	if err != nil {
		return "", plugin.Wrapf(plugin.Permanent, "nexus: the index links %q", link)
	}
	u := base.ResolveReference(ref)
	u.Fragment, u.RawFragment = "", ""
	root := c.root()
	if u.Scheme != root.Scheme || u.Host != root.Host || u.User != nil || u.RawQuery != "" ||
		!strings.HasPrefix(u.EscapedPath(), root.EscapedPath()) || strings.Contains(u.Path, "/../") {
		return "", plugin.Wrapf(plugin.Permanent, "nexus: the index links %s, outside repository %s", u.Redacted(), c.repo)
	}
	return u.String(), nil
}

// mine refuses a ref of another repository.
func (c *client) mine(ref plugin.Ref) error {
	if ref.Container != "" && ref.Container != c.repo {
		return plugin.Wrapf(plugin.Permanent, "nexus: the endpoint's repository is %s, not %s", c.repo, ref.Container)
	}
	return nil
}

// newHash is the hash of an index's digest algorithm.
func newHash(algo string) (hash.Hash, error) {
	switch algo {
	case "sha1":
		return sha1.New(), nil //nolint:gosec // npm's shasum
	case "sha256":
		return sha256.New(), nil
	case "sha512":
		return sha512.New(), nil
	}
	return nil, plugin.Wrapf(plugin.Permanent, "nexus: the index's digest is %s, which veho does not know", algo)
}

// asset is the ref's asset, resolved once per client for the range reads.
func (c *client) asset(ctx context.Context, ref plugin.Ref, fresh bool) (asset, error) {
	if err := c.mine(ref); err != nil {
		return asset{}, err
	}
	c.mu.Lock()
	a, ok := c.assets[ref.Key]
	c.mu.Unlock()
	if ok && !fresh {
		return a, nil
	}
	a, err := c.fm.resolve(ctx, c, ref.Key)
	if err != nil {
		return asset{}, err
	}
	c.mu.Lock()
	c.assets[ref.Key] = a
	c.mu.Unlock()
	return a, nil
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
	case netguard.Classified("nexus", err) != nil:
		return nil, netguard.Classified("nexus", err)
	}
	var cert *tls.CertificateVerificationError
	if errors.As(err, &cert) {
		return nil, plugin.Wrap(fmt.Errorf("nexus: the server's certificate: %w", cert.Err), plugin.Permanent)
	}
	return nil, plugin.Wrap(fmt.Errorf("nexus: %s %s: %w", method, req.URL.Redacted(), err), plugin.Transient)
}

// get reads a document of at most limit bytes (an index: NX-11).
func (c *client) get(ctx context.Context, u, what string, limit int64) ([]byte, error) {
	resp, err := c.do(ctx, http.MethodGet, u, nil, nil, 0)
	if err != nil {
		return nil, err
	}
	defer discard(resp)
	if resp.StatusCode != http.StatusOK {
		return nil, status(resp, what)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	switch {
	case err != nil:
		return nil, plugin.Wrap(fmt.Errorf("nexus: %s: %w", what, err), plugin.Transient)
	case int64(len(b)) > limit:
		return nil, plugin.Wrapf(plugin.Permanent, "nexus: %s is larger than %d bytes", what, limit)
	}
	return b, nil
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

// --- the formats without an index -------------------------------------------

// raw: the key is the content path.
type raw struct{}

func (raw) resolve(_ context.Context, c *client, key string) (asset, error) {
	u, err := c.content(key)
	return asset{url: u}, err
}

func (raw) sink(_ context.Context, c *client, key string, size int64) (*upload, error) {
	target, err := c.content(key)
	if err != nil {
		return nil, err
	}
	return c.putUpload(key, target, size, "the repository may not allow redeploying an asset"), nil
}

func (raw) remove(ctx context.Context, c *client, key string) error {
	u, err := c.content(key)
	if err != nil {
		return err
	}
	return c.deleteURL(ctx, u, key)
}

// maven2: the key is a Maven path or coordinates.
type maven2 struct{}

func (maven2) resolve(_ context.Context, c *client, key string) (asset, error) {
	m, err := mavenPath(key)
	if err != nil {
		return asset{}, err
	}
	u, err := c.content(m.path)
	return asset{url: u}, err
}

func (maven2) sink(_ context.Context, c *client, key string, size int64) (*upload, error) {
	m, err := mavenPath(key)
	if err != nil {
		return nil, err
	}
	if m.generated() {
		return nil, plugin.Wrapf(plugin.Permanent, "nexus: %s: Nexus writes the checksums itself (NX-8)", key)
	}
	target, err := c.content(m.path)
	if err != nil {
		return nil, err
	}
	return c.putUpload(key, target, size, "the repository's version policy (release or snapshot) or its write policy (redeploy) refuses it"), nil
}

func (maven2) remove(ctx context.Context, c *client, key string) error {
	a, err := maven2{}.resolve(ctx, c, key)
	if err != nil {
		return err
	}
	return c.deleteURL(ctx, a.url, key)
}

// deleteURL deletes an asset; one already gone is no error.
func (c *client) deleteURL(ctx context.Context, u, key string) error {
	resp, err := c.do(ctx, http.MethodDelete, u, nil, nil, 0)
	if err != nil {
		return err
	}
	defer discard(resp)
	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusOK, http.StatusNotFound:
		return nil
	default:
		return status(resp, key)
	}
}

// --- source -----------------------------------------------------------------

// head is the asset's size and ETag, the version: Nexus derives it from the
// content's SHA-1 ("{SHA1{<hex>}}"), so it changes with the content.
func (c *client) head(ctx context.Context, u, key string) (plugin.ObjectInfo, error) {
	resp, err := c.do(ctx, http.MethodHead, u, nil, nil, 0)
	if err != nil {
		return plugin.ObjectInfo{}, err
	}
	defer discard(resp)
	if resp.StatusCode != http.StatusOK {
		return plugin.ObjectInfo{}, status(resp, key)
	}
	etag := resp.Header.Get("ETag")
	switch {
	case resp.ContentLength < 0:
		return plugin.ObjectInfo{}, plugin.Wrapf(plugin.Permanent, "nexus: %s: no Content-Length", key)
	case etag == "" || strings.HasPrefix(etag, "W/"):
		return plugin.ObjectInfo{}, plugin.Wrapf(plugin.Permanent, "nexus: %s: no strong ETag to tell a changed asset", key)
	}
	return plugin.ObjectInfo{Size: resp.ContentLength, Version: etag}, nil
}

// Stat is the asset, resolved afresh; its ranges are probed, so a Nexus
// that ignores them is refused before any transfer; and its content is
// compared once with the index's digest, where there is one.
func (c *client) Stat(ctx context.Context, ref plugin.Ref) (plugin.ObjectInfo, error) {
	a, err := c.asset(ctx, ref, true)
	if err != nil {
		return plugin.ObjectInfo{}, err
	}
	info, err := c.head(ctx, a.url, ref.Key)
	if err != nil || info.Size == 0 {
		return info, err
	}
	r, err := c.ReadRange(ctx, ref, 0, 1, info.Version)
	if err != nil {
		return plugin.ObjectInfo{}, err
	}
	_ = r.Close()
	if err := c.checkIndex(ctx, ref.Key, a, info.Version); err != nil {
		return plugin.ObjectInfo{}, err
	}
	return info, nil
}

// ReadRange is [off, off+length) exactly; the answer's ETag must be the
// version (Nexus does not promise to honour If-Match).
func (c *client) ReadRange(ctx context.Context, ref plugin.Ref, off, length int64, ifVersion string) (io.ReadCloser, error) {
	if off < 0 || length < 0 {
		return nil, plugin.Wrapf(plugin.Permanent, "nexus: range [%d, +%d)", off, length)
	}
	a, err := c.asset(ctx, ref, false)
	if err != nil {
		if errors.Is(err, plugin.ErrNotFound) && ifVersion != "" {
			return nil, plugin.Wrapf(plugin.SourceChanged, "nexus: %s is gone", ref.Key)
		}
		return nil, err
	}
	if length == 0 {
		info, err := c.head(ctx, a.url, ref.Key)
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
	rng := fmt.Sprintf("bytes=%d-%d", off, off+length-1)
	resp, err := c.do(ctx, http.MethodGet, a.url, http.Header{"Range": {rng}}, nil, 0)
	if err != nil {
		return nil, err
	}
	switch {
	case resp.StatusCode == http.StatusNotFound && ifVersion != "":
		discard(resp)
		return nil, plugin.Wrapf(plugin.SourceChanged, "nexus: %s is gone", ref.Key)
	case resp.StatusCode == http.StatusOK && c.format == "npm":
		// shortcut: Nexus serves no ranges of npm tarballs: the range is read
		// from the start, fine for packages bounded by max_npm_size; upgrade
		// if Nexus learns ranges for npm.
		return c.skipTo(resp, ref.Key, off, length, ifVersion)
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

// skipTo is [off, off+length) of a whole answer, its ETag checked.
func (c *client) skipTo(resp *http.Response, key string, off, length int64, ifVersion string) (io.ReadCloser, error) {
	switch {
	case ifVersion != "" && resp.Header.Get("ETag") != ifVersion:
		discard(resp)
		return nil, plugin.Wrapf(plugin.SourceChanged, "nexus: %s changed", key)
	case resp.ContentLength >= 0 && off+length > resp.ContentLength:
		discard(resp)
		return nil, plugin.Wrapf(plugin.Permanent, "nexus: range [%d, +%d) beyond %d bytes of %s", off, length, resp.ContentLength, key)
	}
	if _, err := io.CopyN(io.Discard, resp.Body, off); err != nil {
		_ = resp.Body.Close()
		return nil, plugin.Wrap(fmt.Errorf("nexus: %s: %w", key, err), plugin.Transient)
	}
	return struct {
		io.Reader
		io.Closer
	}{io.LimitReader(resp.Body, length), resp.Body}, nil
}

// search is the ids of the REST search's items (kind: "assets" or
// "components") whose path keep accepts (nil: all), across its pages.
func (c *client) search(ctx context.Context, kind string, q url.Values, keep func(path string) bool) ([]string, error) {
	var ids []string
	for page := 0; page < 100; page++ {
		u := c.base.JoinPath("service/rest/v1/search")
		if kind == "assets" {
			u = u.JoinPath("assets")
		}
		u.RawQuery = q.Encode()
		b, err := c.get(ctx, u.String(), "the search of repository "+c.repo, 4<<20)
		if err != nil {
			return nil, err
		}
		var found struct {
			Items []struct {
				ID   string `json:"id"`
				Path string `json:"path"`
			} `json:"items"`
			Token string `json:"continuationToken"`
		}
		if err := json.Unmarshal(b, &found); err != nil {
			return nil, plugin.Wrap(fmt.Errorf("nexus: the search of repository %s: %w", c.repo, err), plugin.Transient)
		}
		for _, it := range found.Items {
			if keep == nil || keep(it.Path) {
				ids = append(ids, it.ID)
			}
		}
		if found.Token == "" {
			return ids, nil
		}
		q.Set("continuationToken", found.Token)
	}
	return nil, plugin.Wrapf(plugin.Permanent, "nexus: the search of repository %s has more than 100 pages", c.repo)
}

// searchLag bounds the wait for Nexus's search to list a new asset: its
// index is updated after the upload.
var searchLag = time.Minute

// searchFound is search's ids once there are some: an asset that exists
// may not be listed yet.
func (c *client) searchFound(ctx context.Context, kind string, q url.Values, keep func(path string) bool) ([]string, error) {
	wait, deadline := 250*time.Millisecond, time.Now().Add(searchLag)
	for {
		ids, err := c.search(ctx, kind, url.Values(maps.Clone(q)), keep)
		if err != nil || len(ids) > 0 {
			return ids, err
		}
		if time.Now().After(deadline) {
			return nil, plugin.Wrapf(plugin.Transient, "nexus: repository %s: Nexus's search does not list the asset yet", c.repo)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
		wait = min(2*wait, 2*time.Second)
	}
}

// checkIndex reads the asset whole and compares it with the index's
// digest, once per key and ETag (§2.2).
func (c *client) checkIndex(ctx context.Context, key string, a asset, etag string) error {
	if a.algo == "" {
		return nil
	}
	c.mu.Lock()
	done := c.verified[key] == etag
	c.mu.Unlock()
	if done {
		return nil
	}
	h, err := newHash(a.algo)
	if err != nil {
		return err
	}
	resp, err := c.do(ctx, http.MethodGet, a.url, nil, nil, 0)
	if err != nil {
		return err
	}
	defer discard(resp)
	if resp.StatusCode != http.StatusOK {
		return status(resp, key)
	}
	if _, err := io.Copy(h, resp.Body); err != nil {
		return plugin.Wrap(fmt.Errorf("nexus: %s: %w", key, err), plugin.Transient)
	}
	switch {
	case resp.Header.Get("ETag") != etag:
		return plugin.Wrapf(plugin.SourceChanged, "nexus: %s changed", key)
	case hex.EncodeToString(h.Sum(nil)) != a.sum:
		return plugin.Wrapf(plugin.Integrity, "nexus: %s: the content is not what the index's %s says", key, a.algo)
	}
	c.mu.Lock()
	c.verified[key] = etag
	c.mu.Unlock()
	return nil
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
	a, err := c.asset(ctx, ref, false)
	return a.url, err
}

// Delete deletes the asset; one already gone is no error.
func (c *client) Delete(ctx context.Context, ref plugin.Ref) error {
	if err := c.mine(ref); err != nil {
		return err
	}
	c.mu.Lock()
	delete(c.assets, ref.Key)
	c.mu.Unlock()
	return c.fm.remove(ctx, c, ref.Key)
}

// --- destination ------------------------------------------------------------

// Begin checks that the repository is hosted and of the endpoint's format
// (raw without one), once per client, and starts the upload: one request,
// and Nexus creates the asset only when it ends.
func (c *client) Begin(ctx context.Context, ref plugin.Ref, info plugin.ObjectInfo, _ plugin.PutOptions) (plugin.Upload, error) {
	if c.user == "" {
		return nil, plugin.Wrapf(plugin.Permanent, "nexus: a destination needs credentials")
	}
	if err := c.mine(ref); err != nil {
		return nil, err
	}
	repo, err := c.info(ctx)
	if err != nil {
		return nil, err
	}
	want := c.format
	if want == "" {
		want = "raw"
	}
	if repo.Format != want || repo.Type != "hosted" {
		return nil, plugin.Wrapf(plugin.Permanent, "nexus: repository %s is %s %s: a destination is a %s hosted repository", c.repo, repo.Type, repo.Format, want)
	}
	c.mu.Lock()
	delete(c.assets, ref.Key)
	c.mu.Unlock()
	return c.fm.sink(ctx, c, ref.Key, info.Size)
}

// Resume: an upload to Nexus cannot resume; the engine begins again.
func (c *client) Resume(context.Context, plugin.Ref, []byte) (plugin.Upload, error) {
	return nil, plugin.Wrapf(plugin.ResumeInvalid, "nexus: an upload to Nexus cannot resume: begin again")
}

// upload is one request's upload: the stream of its parts, and what checks
// the new asset once the request has ended.
type upload struct {
	key       string
	s         *stream.Stream
	finish    func(ctx context.Context) (plugin.ObjectInfo, error)
	limit     int64 // the most bytes the upload takes, if not 0 (npm: NX-10)
	committed *plugin.ObjectInfo
	mu        sync.Mutex
}

// putUpload is the PUT of the bytes to target (raw, maven2), whose SHA-1
// is compared with the new asset's ETag (NX-3); refused says why Nexus
// may answer 400.
func (c *client) putUpload(key, target string, size int64, refused string) *upload {
	sum := sha1.New() //nolint:gosec // Nexus's ETag
	u := &upload{key: key}
	u.s = stream.New("nexus: upload of "+key, size, sum, func(ctx context.Context, body io.Reader, length int64) error {
		if body == nil {
			body = http.NoBody
		}
		resp, err := c.do(ctx, http.MethodPut, target, http.Header{"Content-Type": {"application/octet-stream"}}, body, length)
		if err != nil {
			return err
		}
		defer discard(resp)
		return uploaded(resp, key, refused)
	})
	u.finish = func(ctx context.Context) (plugin.ObjectInfo, error) {
		return c.checkSHA1(ctx, target, key, hex.EncodeToString(sum.Sum(nil)))
	}
	return u
}

// uploaded classifies the answer to an upload.
func uploaded(resp *http.Response, key, refused string) error {
	switch resp.StatusCode {
	case http.StatusCreated, http.StatusOK, http.StatusNoContent:
		return nil
	case http.StatusBadRequest:
		return plugin.Wrapf(plugin.Permanent, "nexus: upload of %s refused (400): %s", key, refused)
	case http.StatusRequestEntityTooLarge:
		return plugin.Wrapf(plugin.Permanent, "nexus: upload of %s: too large for Nexus or its proxy", key)
	}
	return status(resp, "upload of "+key)
}

// checkSHA1 compares the new asset's ETag with the SHA-1 of the bytes sent.
func (c *client) checkSHA1(ctx context.Context, u, key, sum string) (plugin.ObjectInfo, error) {
	info, err := c.head(ctx, u, key)
	if err != nil {
		return plugin.ObjectInfo{}, err
	}
	if !strings.Contains(strings.ToLower(info.Version), sum) {
		return plugin.ObjectInfo{}, plugin.Wrapf(plugin.Integrity, "nexus: %s: the asset's SHA-1 is not that of the bytes sent", key)
	}
	return info, nil
}

func (u *upload) Token() []byte {
	b, _ := json.Marshal(map[string]string{"key": u.key}) // cannot fail
	return b
}

func (u *upload) WritePart(ctx context.Context, partNo int, r io.Reader, length int64) (plugin.PartResult, error) {
	if u.limit > 0 {
		done := length
		for _, p := range u.s.Parts() {
			if p.PartNo < partNo {
				done += p.Size
			}
		}
		if done > u.limit {
			return plugin.PartResult{}, plugin.Wrapf(plugin.Permanent, "nexus: upload of %s: beyond %d bytes", u.key, u.limit)
		}
	}
	return u.s.Write(ctx, partNo, r, length)
}

func (u *upload) ListParts(context.Context) ([]plugin.PartResult, error) { return u.s.Parts(), nil }

// Commit ends the request and checks the new asset.
func (u *upload) Commit(ctx context.Context, parts []plugin.PartResult) (plugin.ObjectInfo, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.committed != nil {
		return *u.committed, nil
	}
	if err := u.s.Close(parts); err != nil {
		return plugin.ObjectInfo{}, err
	}
	info, err := u.finish(ctx)
	if err != nil {
		return plugin.ObjectInfo{}, err
	}
	u.committed = &info
	return info, nil
}

// Abort stops the request: Nexus keeps nothing of an unfinished one.
func (u *upload) Abort(context.Context) error {
	u.s.Abort()
	return nil
}
