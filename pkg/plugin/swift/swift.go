// Package swift is the OpenStack Swift driver (.specs/veho-openstack.md §4):
// a source by ranges, and a destination by static large objects, each part a
// segment named by its upload, the manifest written at commit, so parts are
// parallel and resumable.
package swift

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // Swift's ETags are MD5s
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/gophercloud/gophercloud/v2"

	"github.com/dihedron/veho/pkg/plugin"
	"github.com/dihedron/veho/pkg/plugin/openstack"
)

// The limits of segments: Swift's defaults (5 GiB, max_manifest_segments).
const (
	maxPartSize     = 5 << 30
	defaultSegments = 1000
)

// Driver is the Swift driver under one platform's settings.
type Driver struct {
	cloud    *openstack.Cloud
	segments int
}

// settings are the platform's, in drivers[].settings.
type settings struct {
	openstack.Settings
	// MaxSegments is the clusters' max_manifest_segments, if not Swift's
	// default.
	MaxSegments int `json:"max_segments"`
}

// Factory builds the driver from its settings: allow_cidrs, plaintext and
// max_segments.
func Factory(raw json.RawMessage) (plugin.Driver, error) {
	var s settings
	if err := plugin.DecodeSettings(raw, &s); err != nil {
		return nil, fmt.Errorf("swift: %w", err)
	}
	if s.MaxSegments < 0 {
		return nil, fmt.Errorf("swift: max_segments %d", s.MaxSegments)
	}
	if s.MaxSegments == 0 {
		s.MaxSegments = defaultSegments
	}
	c, err := openstack.NewCloud(s.Settings)
	if err != nil {
		return nil, fmt.Errorf("swift: %w", err)
	}
	return Driver{cloud: c, segments: s.MaxSegments}, nil
}

func (Driver) Name() string      { return "swift" }
func (Driver) Schemes() []string { return []string{"swift"} }

func (d Driver) Capabilities() plugin.Capabilities {
	return plugin.Capabilities{
		Source: true, Sink: true, RangeRead: true, ResumableUpload: true, ParallelParts: true,
		NativeChecksum: []string{"md5"}, MaxParts: d.segments, MinPartSize: 1, MaxPartSize: maxPartSize,
	}
}

//go:embed schema.json
var own []byte

// schema is the endpoint configuration's JSON Schema (PLG-10): own, with
// the cloud's fields.
var schema = func() []byte {
	b, err := openstack.Schema(own)
	if err != nil {
		panic(err) // embedded files: a test catches it
	}
	return b
}()

func (Driver) ConfigSchema() []byte { return schema }

// checkName refuses names Swift or the URL would read otherwise: empty
// segments, "." and "..", control characters, more than 1,024 bytes.
func checkName(what, name string) error {
	if name == "" || len(name) > 1024 || strings.ContainsFunc(name, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return plugin.Wrapf(plugin.Permanent, "swift: %s %q is not allowed", what, name)
	}
	for seg := range strings.SplitSeq(name, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return plugin.Wrapf(plugin.Permanent, "swift: %s %q is not allowed", what, name)
		}
	}
	return nil
}

// checkContainer: a container is one segment.
func checkContainer(name string) error {
	if strings.Contains(name, "/") || len(name) > 256 {
		return plugin.Wrapf(plugin.Permanent, "swift: container %q is not allowed", name)
	}
	return checkName("container", name)
}

// ParseRef parses swift://<container>/<name>.
func (Driver) ParseRef(raw string) (plugin.Ref, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "swift" || u.Host == "" {
		return plugin.Ref{}, plugin.Wrapf(plugin.Permanent, "swift: %q is not swift://container/name", raw)
	}
	ref := plugin.Ref{Container: u.Host, Key: strings.TrimPrefix(u.Path, "/")}
	if err := checkContainer(ref.Container); err != nil {
		return plugin.Ref{}, err
	}
	if err := checkName("name", ref.Key); err != nil {
		return plugin.Ref{}, err
	}
	return ref, nil
}

type config struct {
	openstack.Endpoint
	Container        string `json:"container"`
	SegmentContainer string `json:"segment_container"`
	StoragePolicy    string `json:"storage_policy"`
}

// Open authenticates to Keystone and finds Swift in the catalog.
func (d Driver) Open(ctx context.Context, raw plugin.Config, creds plugin.Credentials) (plugin.Client, error) {
	var cfg config
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, plugin.Wrapf(plugin.Permanent, "swift: configuration: %v", err)
	}
	if err := checkContainer(cfg.Container); err != nil {
		return nil, err
	}
	if cfg.SegmentContainer != "" {
		if err := checkContainer(cfg.SegmentContainer); err != nil {
			return nil, err
		}
	}
	if err := d.cloud.CheckEndpoint(cfg.Endpoint); err != nil {
		return nil, plugin.Wrap(fmt.Errorf("swift: %w", err), plugin.Permanent)
	}
	sc, err := d.cloud.Connect(ctx, cfg.Endpoint, creds, openstack.ObjectStore)
	if err != nil {
		return nil, fmt.Errorf("swift: %w", err)
	}
	return &client{sc: sc, cfg: cfg, ready: map[string]bool{}, segments: d.segments}, nil
}

type client struct {
	sc       *gophercloud.ServiceClient
	cfg      config
	segments int

	mu    sync.Mutex
	ready map[string]bool // containers created or checked
}

// Flows is Keystone's and Swift's.
func (c *client) Flows(context.Context) ([]plugin.Flow, error) {
	return openstack.Flows(c.cfg.Endpoint, "swift", c.sc)
}

func (c *client) Close() error {
	if t, ok := c.sc.HTTPClient.Transport.(*http.Transport); ok {
		t.CloseIdleConnections()
	}
	return nil
}

// objectURL is the URL of container/name, each segment escaped.
func (c *client) objectURL(container, name string) string {
	segs := strings.Split(name, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return c.sc.ServiceURL(url.PathEscape(container), strings.Join(segs, "/"))
}

// where is the container and name of ref.
func (c *client) where(ref plugin.Ref) (string, string, error) {
	container := c.cfg.Container
	if ref.Container != "" {
		if err := checkContainer(ref.Container); err != nil {
			return "", "", err
		}
		container = ref.Container
	}
	return container, ref.Key, checkName("name", ref.Key)
}

func (c *client) segmentContainer(container string) string {
	if c.cfg.SegmentContainer != "" {
		return c.cfg.SegmentContainer
	}
	return container + "_segments"
}

// classify is openstack.Classify, with Swift's answers to conditions and
// checksums.
func classify(ctx context.Context, op string, err error) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	switch code, _ := openstack.StatusOf(err); code {
	case http.StatusPreconditionFailed:
		return plugin.Wrapf(plugin.SourceChanged, "swift: %s: the object changed", op)
	case http.StatusUnprocessableEntity:
		return plugin.Wrapf(plugin.Integrity, "swift: %s: the checksum does not match", op)
	}
	return openstack.Classify(err, "swift: "+op)
}

// do sends a request; the response's body is the caller's to close.
func (c *client) do(ctx context.Context, method, u string, headers map[string]string, body io.Reader, ok ...int) (*http.Response, error) {
	return c.sc.Request(ctx, method, u, &gophercloud.RequestOpts{RawBody: body, MoreHeaders: headers, OkCodes: ok, KeepResponseBody: true})
}

// --- source -----------------------------------------------------------------

// version is the object's ETag and timestamp: the ETag alone does not change
// when the same bytes are written again, which a reader must also notice.
func version(h http.Header) string {
	return strings.Trim(h.Get("Etag"), `"`) + "@" + h.Get("X-Timestamp")
}

func info(h http.Header, size int64) plugin.ObjectInfo {
	meta := map[string]string{}
	for k, v := range h {
		if m, ok := strings.CutPrefix(k, "X-Object-Meta-"); ok && len(v) > 0 {
			meta[strings.ToLower(m)] = v[0]
		}
	}
	if len(meta) == 0 {
		meta = nil
	}
	return plugin.ObjectInfo{Size: size, Version: version(h), Metadata: meta}
}

func (c *client) Stat(ctx context.Context, ref plugin.Ref) (plugin.ObjectInfo, error) {
	container, name, err := c.where(ref)
	if err != nil {
		return plugin.ObjectInfo{}, err
	}
	resp, err := c.do(ctx, http.MethodHead, c.objectURL(container, name), nil, nil, http.StatusOK, http.StatusNoContent)
	if err != nil {
		return plugin.ObjectInfo{}, classify(ctx, "head "+name, err)
	}
	_ = resp.Body.Close()
	size, err := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64)
	if err != nil || size < 0 {
		// a proxy in front of Swift may drop a HEAD's Content-Length: 0
		if size, err = c.listedSize(ctx, container, name); err != nil {
			return plugin.ObjectInfo{}, err
		}
	}
	return info(resp.Header, size), nil
}

// listedSize is the object's size in its container's listing.
func (c *client) listedSize(ctx context.Context, container, name string) (int64, error) {
	q := url.Values{"format": {"json"}, "prefix": {name}, "limit": {"1"}}
	resp, err := c.do(ctx, http.MethodGet, c.sc.ServiceURL(url.PathEscape(container))+"?"+q.Encode(), nil, nil, http.StatusOK, http.StatusNoContent)
	if err != nil {
		return 0, classify(ctx, "list "+name, err)
	}
	defer func() { _ = resp.Body.Close() }()
	var page []struct {
		Name  string `json:"name"`
		Bytes int64  `json:"bytes"`
	}
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
			return 0, plugin.Wrap(fmt.Errorf("swift: list %s: %w", name, err), plugin.Transient)
		}
	}
	if len(page) == 0 || page[0].Name != name {
		return 0, plugin.Wrapf(plugin.Permanent, "swift: %s: no size", name)
	}
	return page[0].Bytes, nil
}

func (c *client) ReadRange(ctx context.Context, ref plugin.Ref, off, length int64, ifVersion string) (io.ReadCloser, error) {
	if off < 0 || length < 0 {
		return nil, plugin.Wrapf(plugin.Permanent, "swift: range [%d, +%d)", off, length)
	}
	if length == 0 { // no empty Range in HTTP
		got, err := c.Stat(ctx, ref)
		switch {
		case err != nil:
			return nil, err
		case ifVersion != "" && got.Version != ifVersion:
			return nil, plugin.Wrapf(plugin.SourceChanged, "swift: %s changed", ref.Key)
		case off > got.Size:
			return nil, plugin.Wrapf(plugin.Permanent, "swift: offset %d beyond %d bytes", off, got.Size)
		}
		return io.NopCloser(bytes.NewReader(nil)), nil
	}
	container, name, err := c.where(ref)
	if err != nil {
		return nil, err
	}
	headers := map[string]string{"Range": fmt.Sprintf("bytes=%d-%d", off, off+length-1)}
	if ifVersion != "" {
		etag, _, _ := strings.Cut(ifVersion, "@")
		headers["If-Match"] = `"` + etag + `"`
	}
	resp, err := c.do(ctx, http.MethodGet, c.objectURL(container, name), headers, nil, http.StatusPartialContent)
	if err != nil {
		return nil, classify(ctx, "get "+name, err)
	}
	want := fmt.Sprintf("bytes %d-%d/", off, off+length-1)
	switch {
	case ifVersion != "" && version(resp.Header) != ifVersion:
		_ = resp.Body.Close()
		return nil, plugin.Wrapf(plugin.SourceChanged, "swift: %s changed", name)
	case !strings.HasPrefix(resp.Header.Get("Content-Range"), want) || resp.ContentLength != length:
		_ = resp.Body.Close()
		return nil, plugin.Wrapf(plugin.Permanent, "swift: range [%d, +%d) of %s: answered %q", off, length, name, resp.Header.Get("Content-Range"))
	}
	return resp.Body, nil
}

// Delete removes the object and, for an SLO, its segments.
func (c *client) Delete(ctx context.Context, ref plugin.Ref) error {
	container, name, err := c.where(ref)
	if err != nil {
		return err
	}
	return c.remove(ctx, c.objectURL(container, name)+"?multipart-manifest=delete", "delete "+name)
}

// --- uploads ----------------------------------------------------------------

type token struct {
	Container   string            `json:"container"`
	Name        string            `json:"name"`
	Segments    string            `json:"segments"`
	UploadID    string            `json:"upload_id"`
	ContentType string            `json:"content_type,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

type upload struct {
	c *client
	t token
}

// prefix is the segments' prefix in their container.
func (u *upload) prefix() string { return u.t.Name + "/" + u.t.UploadID + "/" }

func (u *upload) segment(partNo int) string { return fmt.Sprintf("%s%08d", u.prefix(), partNo) }

// marker is the empty object that says the upload is alive: made by Begin,
// removed by Commit and Abort. Swift has no uploads of its own.
func (u *upload) marker() string { return u.c.objectURL(u.t.Segments, u.prefix()+"upload") }

// alive is ResumeInvalid when the upload was committed or aborted.
func (u *upload) alive(ctx context.Context) error {
	resp, err := u.c.do(ctx, http.MethodHead, u.marker(), nil, nil, http.StatusOK, http.StatusNoContent)
	if err = classify(ctx, "upload of "+u.t.Name, err); errors.Is(err, plugin.ErrNotFound) {
		return plugin.Wrapf(plugin.ResumeInvalid, "swift: the upload of %s is gone", u.t.Name)
	} else if err != nil {
		return err
	}
	_ = resp.Body.Close()
	return nil
}

// remove deletes an object; one already gone is no error.
func (c *client) remove(ctx context.Context, u, op string) error {
	resp, err := c.do(ctx, http.MethodDelete, u, nil, nil, http.StatusNoContent, http.StatusOK)
	if err = classify(ctx, op, err); errors.Is(err, plugin.ErrNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	_ = resp.Body.Close()
	return nil
}

// ensureContainer creates the container with the storage policy, or checks
// the policy of one that exists, once per client.
func (c *client) ensureContainer(ctx context.Context, name string) error {
	c.mu.Lock()
	done := c.ready[name]
	c.mu.Unlock()
	if done {
		return nil
	}
	u := c.sc.ServiceURL(url.PathEscape(name))
	resp, err := c.do(ctx, http.MethodHead, u, nil, nil, http.StatusNoContent, http.StatusOK)
	switch {
	case err == nil:
		_ = resp.Body.Close()
		if p := resp.Header.Get("X-Storage-Policy"); c.cfg.StoragePolicy != "" && p != c.cfg.StoragePolicy {
			return plugin.Wrapf(plugin.Permanent, "swift: container %s has the storage policy %s, not %s", name, p, c.cfg.StoragePolicy)
		}
	case errors.Is(classify(ctx, "", err), plugin.ErrNotFound):
		headers := map[string]string{}
		if c.cfg.StoragePolicy != "" {
			headers["X-Storage-Policy"] = c.cfg.StoragePolicy
		}
		resp, err := c.do(ctx, http.MethodPut, u, headers, nil, http.StatusCreated, http.StatusAccepted)
		if err != nil {
			return classify(ctx, "create container "+name, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusAccepted { // made meanwhile, by someone else: check it
			return c.ensureContainer(ctx, name)
		}
	default:
		return classify(ctx, "container "+name, err)
	}
	c.mu.Lock()
	c.ready[name] = true
	c.mu.Unlock()
	return nil
}

func (c *client) Begin(ctx context.Context, ref plugin.Ref, _ plugin.ObjectInfo, opts plugin.PutOptions) (plugin.Upload, error) {
	container, name, err := c.where(ref)
	if err != nil {
		return nil, err
	}
	segments := c.segmentContainer(container)
	for _, ct := range []string{container, segments} {
		if err := c.ensureContainer(ctx, ct); err != nil {
			return nil, err
		}
	}
	id := make([]byte, 16)
	_, _ = rand.Read(id) // never fails
	u := &upload{c: c, t: token{Container: container, Name: name, Segments: segments, UploadID: hex.EncodeToString(id),
		ContentType: opts.ContentType, Metadata: opts.Metadata}}
	resp, err := c.do(ctx, http.MethodPut, u.marker(), map[string]string{"Etag": emptyMD5}, bytes.NewReader(nil), http.StatusCreated)
	if err != nil {
		return nil, classify(ctx, "begin "+name, err)
	}
	_ = resp.Body.Close()
	return u, nil
}

// Resume re-attaches to an upload: its segments are where the token says.
func (c *client) Resume(ctx context.Context, ref plugin.Ref, raw []byte) (plugin.Upload, error) {
	container, name, err := c.where(ref)
	if err != nil {
		return nil, err
	}
	var t token
	if err := json.Unmarshal(raw, &t); err != nil || len(t.UploadID) != 32 || checkContainer(t.Segments) != nil {
		return nil, plugin.Wrapf(plugin.ResumeInvalid, "swift: malformed upload token")
	}
	if t.Container != container || t.Name != name {
		return nil, plugin.Wrapf(plugin.ResumeInvalid, "swift: the upload token is of %s/%s", t.Container, t.Name)
	}
	u := &upload{c: c, t: t}
	if err := u.alive(ctx); err != nil {
		return nil, err
	}
	return u, nil
}

func (u *upload) Token() []byte {
	b, _ := json.Marshal(u.t) // strings only: cannot fail
	return b
}

// counted counts what it reads.
type counted struct {
	r io.Reader
	n int64
}

func (c *counted) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// WritePart streams the part to its segment, hashing it on the way, and
// compares the MD5 with the ETag Swift computed of what it stored.
func (u *upload) WritePart(ctx context.Context, partNo int, r io.Reader, length int64) (plugin.PartResult, error) {
	if partNo < 0 || partNo >= u.c.segments || length < 0 || length > maxPartSize {
		return plugin.PartResult{}, plugin.Wrapf(plugin.Permanent, "swift: part %d of %d bytes is out of range", partNo, length)
	}
	if length == 0 {
		// Swift refuses empty segments: an empty object is one empty part,
		// published by Commit as a plain object
		if partNo != 0 {
			return plugin.PartResult{}, plugin.Wrapf(plugin.Permanent, "swift: part %d is empty: only an empty object has an empty part", partNo)
		}
		return plugin.PartResult{PartNo: 0, Size: 0, ETag: emptyMD5, SHA256: emptySHA256}, nil
	}
	if err := u.alive(ctx); err != nil {
		return plugin.PartResult{}, err
	}
	md, sha := md5.New(), sha256.New() //nolint:gosec // Swift's ETag
	body := &counted{r: io.TeeReader(io.LimitReader(r, length), io.MultiWriter(md, sha))}
	resp, err := u.c.do(ctx, http.MethodPut, u.c.objectURL(u.t.Segments, u.segment(partNo)), nil, body, http.StatusCreated)
	if err != nil {
		return plugin.PartResult{}, classify(ctx, fmt.Sprintf("part %d of %s", partNo, u.t.Name), err)
	}
	_ = resp.Body.Close()
	sum := hex.EncodeToString(md.Sum(nil))
	switch {
	case body.n != length:
		return plugin.PartResult{}, plugin.Wrapf(plugin.Integrity, "swift: part %d: %d bytes of %d", partNo, body.n, length)
	case strings.Trim(resp.Header.Get("Etag"), `"`) != sum:
		return plugin.PartResult{}, plugin.Wrapf(plugin.Integrity, "swift: part %d: Swift stored other bytes than those sent", partNo)
	}
	return plugin.PartResult{PartNo: partNo, Size: length, ETag: sum, SHA256: hex.EncodeToString(sha.Sum(nil))}, nil
}

// ListParts lists the upload's segments, by part number.
func (u *upload) ListParts(ctx context.Context) ([]plugin.PartResult, error) {
	var parts []plugin.PartResult
	marker := ""
	for {
		q := url.Values{"format": {"json"}, "prefix": {u.prefix()}, "marker": {marker}}
		resp, err := u.c.do(ctx, http.MethodGet, u.c.sc.ServiceURL(url.PathEscape(u.t.Segments))+"?"+q.Encode(), nil, nil, http.StatusOK, http.StatusNoContent)
		if err = classify(ctx, "list parts of "+u.t.Name, err); errors.Is(err, plugin.ErrNotFound) {
			return nil, nil
		} else if err != nil {
			return nil, err
		}
		var page []struct {
			Name  string `json:"name"`
			Bytes int64  `json:"bytes"`
			Hash  string `json:"hash"`
		}
		if resp.StatusCode == http.StatusOK {
			err = json.NewDecoder(resp.Body).Decode(&page)
		}
		_ = resp.Body.Close()
		if err != nil {
			return nil, plugin.Wrap(fmt.Errorf("swift: list parts of %s: %w", u.t.Name, err), plugin.Transient)
		}
		if len(page) == 0 {
			return parts, nil
		}
		for _, s := range page {
			n, err := strconv.Atoi(strings.TrimPrefix(s.Name, u.prefix()))
			if err != nil || n < 0 {
				continue // not a segment of veho's
			}
			parts = append(parts, plugin.PartResult{PartNo: n, Size: s.Bytes, ETag: s.Hash})
		}
		marker = page[len(page)-1].Name
	}
}

// The digests of zero bytes.
const (
	emptyMD5    = "d41d8cd98f00b204e9800998ecf8427e"
	emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

// headers are the object's content type and metadata, on the manifest.
func (u *upload) headers() map[string]string {
	h := map[string]string{}
	if u.t.ContentType != "" {
		h["Content-Type"] = u.t.ContentType
	}
	for k, v := range u.t.Metadata {
		h["X-Object-Meta-"+k] = v
	}
	return h
}

// Commit writes the manifest, which Swift checks against each segment's
// size and ETag, and which makes the object visible at once.
func (u *upload) Commit(ctx context.Context, parts []plugin.PartResult) (plugin.ObjectInfo, error) {
	ref := plugin.Ref{Container: u.t.Container, Key: u.t.Name}
	target := u.c.objectURL(u.t.Container, u.t.Name)
	if len(parts) == 1 && parts[0].PartNo == 0 && parts[0].Size == 0 {
		h := u.headers()
		h["Etag"] = emptyMD5
		resp, err := u.c.do(ctx, http.MethodPut, target, h, bytes.NewReader(nil), http.StatusCreated)
		if err != nil {
			return plugin.ObjectInfo{}, classify(ctx, "commit "+u.t.Name, err)
		}
		_ = resp.Body.Close()
		return u.c.Stat(ctx, ref)
	}
	type entry struct {
		Path string `json:"path"`
		Etag string `json:"etag"`
		Size int64  `json:"size_bytes"`
	}
	manifest := make([]entry, len(parts))
	for i, p := range parts {
		if p.PartNo != i {
			return plugin.ObjectInfo{}, plugin.Wrapf(plugin.Permanent, "swift: parts must be 0 to %d in order, got %d at %d", len(parts)-1, p.PartNo, i)
		}
		manifest[i] = entry{Path: "/" + u.t.Segments + "/" + u.segment(i), Etag: p.ETag, Size: p.Size}
	}
	b, _ := json.Marshal(manifest) // strings and numbers: cannot fail
	old, ours, err := u.c.oldSegments(ctx, target, "/"+u.t.Segments+"/"+u.prefix())
	if err != nil {
		return plugin.ObjectInfo{}, err
	}
	if ours == len(parts) && len(old) == 0 { // committed before
		return u.c.Stat(ctx, ref)
	}
	resp, err := u.c.do(ctx, http.MethodPut, target+"?multipart-manifest=put", u.headers(), bytes.NewReader(b), http.StatusCreated)
	if err != nil {
		// the segments are not those given (another execution rewrote one,
		// or aborted the upload): begin again
		if e, ok := openstack.ResponseError(err); ok && e.Actual == http.StatusBadRequest &&
			(bytes.Contains(e.Body, []byte("Etag Mismatch")) || bytes.Contains(e.Body, []byte("Size Mismatch")) || bytes.Contains(e.Body, []byte("404 Not Found"))) {
			return plugin.ObjectInfo{}, plugin.Wrapf(plugin.ResumeInvalid, "swift: commit %s: the segments are not those of the parts", u.t.Name)
		}
		return plugin.ObjectInfo{}, classify(ctx, "commit "+u.t.Name, err)
	}
	_ = resp.Body.Close()
	// the replaced object's data goes with it, as Delete does; failures leave
	// garbage, not a wrong object
	for _, seg := range old {
		_ = u.c.remove(ctx, u.c.sc.ServiceURL(strings.TrimPrefix(seg, "/")), "commit "+u.t.Name)
	}
	if err := u.c.remove(ctx, u.marker(), "commit "+u.t.Name); err != nil {
		return plugin.ObjectInfo{}, err
	}
	return u.c.Stat(ctx, ref)
}

// oldSegments are the segments of the SLO at target, if one is there: the
// other uploads' (with their markers), and how many are under ours (a
// commit again). shortcut: a commit by another upload between this and the
// new manifest leaks that upload's segments; fine while one transfer writes
// a name at a time.
func (c *client) oldSegments(ctx context.Context, target, ours string) ([]string, int, error) {
	resp, err := c.do(ctx, http.MethodGet, target+"?multipart-manifest=get", nil, nil, http.StatusOK)
	if err = classify(ctx, "manifest", err); errors.Is(err, plugin.ErrNotFound) {
		return nil, 0, nil
	} else if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if !strings.EqualFold(resp.Header.Get("X-Static-Large-Object"), "true") {
		return nil, 0, nil
	}
	var segs []struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&segs); err != nil {
		return nil, 0, plugin.Wrap(fmt.Errorf("swift: manifest: %w", err), plugin.Transient)
	}
	var out []string
	n := 0
	for _, s := range segs {
		switch {
		case strings.HasPrefix(s.Name, ours):
			n++
		case checkName("segment", strings.TrimPrefix(s.Name, "/")) == nil:
			out = append(out, s.Name)
		}
	}
	return out, n, nil
}

// Abort deletes the upload's segments; those already gone are no error.
func (u *upload) Abort(ctx context.Context) error {
	parts, err := u.ListParts(ctx)
	if err != nil {
		return err
	}
	for _, p := range parts {
		if err := u.c.remove(ctx, u.c.objectURL(u.t.Segments, u.segment(p.PartNo)), "abort "+u.t.Name); err != nil {
			return err
		}
	}
	return u.c.remove(ctx, u.marker(), "abort "+u.t.Name)
}
