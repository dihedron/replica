// Package file is the file-system storage driver, for tests and local
// copies (.specs/veho-plugins.md §7). Every path goes through os.Root, so no
// key can escape the endpoint's root. An upload is a staging directory with
// one file per part; Commit concatenates them and renames the result into
// place, atomically.
package file

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/dihedron/veho/pkg/plugin"
)

// staging is the directory of the uploads under the root; no key may use it.
const staging = ".veho-staging"

// maxParts bounds the part numbers.
const maxParts = 10000

// Driver is the file driver. With restricted, an endpoint's root must be
// one of roots or under one (veho-sources.md §3.5, SRC-W7).
type Driver struct {
	roots      []string
	restricted bool
}

// New returns the file driver without restriction, for tests and libraries;
// veho's nodes build it with Factory.
func New() Driver { return Driver{} }

type settings struct {
	// Roots are the directories an endpoint's root may be, or be under;
	// empty, no file endpoint can be created.
	Roots []string `json:"roots"`
}

// Factory builds the driver from its settings, roots (veho-sources.md §5).
func Factory(raw json.RawMessage) (plugin.Driver, error) {
	var s settings
	if err := plugin.DecodeSettings(raw, &s); err != nil {
		return nil, fmt.Errorf("file: %w", err)
	}
	for _, r := range s.Roots {
		if !path.IsAbs(r) || path.Clean(r) != r {
			return nil, fmt.Errorf("file: settings: roots: %q is not a clean absolute path", r)
		}
	}
	return Driver{roots: s.Roots, restricted: true}, nil
}

// under reports whether dir is one of the roots or under one.
func (d Driver) under(dir string) bool {
	for _, r := range d.roots {
		if dir == r || r == "/" || strings.HasPrefix(dir, r+"/") {
			return true
		}
	}
	return false
}

// CheckConfig refuses a root that is not a clean absolute path, or not
// under the driver's roots; the engine calls it when an endpoint is created.
func (d Driver) CheckConfig(_ context.Context, cfg plugin.Config) error {
	var c config
	if err := json.Unmarshal(cfg, &c); err != nil || !path.IsAbs(c.Root) || path.Clean(c.Root) != c.Root {
		return plugin.Wrapf(plugin.Permanent, "file: the configuration needs a clean absolute root")
	}
	if d.restricted && !d.under(c.Root) {
		return plugin.Wrapf(plugin.Permanent, "file: root %q is not under the driver's roots on this node (drivers[].settings.roots)", c.Root)
	}
	return nil
}

func (Driver) Name() string { return "file" }

var _ plugin.ConfigChecker = Driver{}

func (Driver) Schemes() []string { return []string{"file"} }

func (Driver) Capabilities() plugin.Capabilities {
	return plugin.Capabilities{
		Source: true, Sink: true, RangeRead: true, ResumableUpload: true, ParallelParts: true,
		MaxParts: maxParts, MaxPartSize: 5 << 30,
	}
}

// schema is the endpoint configuration's JSON Schema (PLG-10).
//
//go:embed schema.json
var schema []byte

func (Driver) ConfigSchema() []byte { return schema }

// ParseRef parses file:///key (the key is relative to the endpoint's root).
func (Driver) ParseRef(url string) (plugin.Ref, error) {
	key, ok := strings.CutPrefix(url, "file:///")
	if !ok {
		return plugin.Ref{}, plugin.Wrapf(plugin.Permanent, "file: %q is not a file:/// URL", url)
	}
	if err := checkKey(key); err != nil {
		return plugin.Ref{}, err
	}
	return plugin.Ref{Key: key}, nil
}

type config struct {
	Root string `json:"root"`
}

// Open binds a client to the root directory of the configuration; the file
// driver takes no credentials.
func (d Driver) Open(ctx context.Context, cfg plugin.Config, _ plugin.Credentials) (plugin.Client, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var c config
	if err := json.Unmarshal(cfg, &c); err != nil || !path.IsAbs(c.Root) {
		return nil, plugin.Wrapf(plugin.Permanent, "file: the configuration needs an absolute root (%v)", err)
	}
	if d.restricted {
		if err := d.CheckConfig(ctx, cfg); err != nil {
			return nil, err
		}
		// the root as the node sees it: a link must not lead out of the roots
		real, err := filepath.EvalSymlinks(c.Root)
		if err == nil && !d.under(real) {
			return nil, plugin.Wrapf(plugin.Permanent, "file: root %q leads to %q, not under the driver's roots", c.Root, real)
		}
	}
	root, err := os.OpenRoot(c.Root)
	if err != nil {
		return nil, plugin.Wrap(fmt.Errorf("file: %w", err), plugin.Permanent)
	}
	return &client{root: root}, nil
}

// client is the driver bound to one root.
type client struct {
	root *os.Root
}

func (c *client) Close() error { return c.root.Close() }

// checkKey refuses keys that are not clean relative paths, or that reach the
// staging directory.
func checkKey(key string) error {
	if key == "" || key == "." || path.IsAbs(key) || path.Clean(key) != key || key == ".." || strings.HasPrefix(key, "../") ||
		key == staging || strings.HasPrefix(key, staging+"/") {
		return plugin.Wrapf(plugin.Permanent, "file: invalid key %q", key)
	}
	return nil
}

func checkRef(ref plugin.Ref) error {
	if ref.Container != "" {
		return plugin.Wrapf(plugin.Permanent, "file: no containers (got %q)", ref.Container)
	}
	return checkKey(ref.Key)
}

// classify maps a file-system error.
func classify(err error, ref plugin.Ref) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("file: %s: %w", ref.Key, plugin.ErrNotFound)
	case errors.Is(err, fs.ErrPermission):
		return plugin.Wrap(fmt.Errorf("file: %w", err), plugin.Permanent)
	default:
		return plugin.Wrap(fmt.Errorf("file: %w", err), plugin.Transient)
	}
}

func version(fi fs.FileInfo) string {
	return strconv.FormatInt(fi.Size(), 10) + "-" + strconv.FormatInt(fi.ModTime().UnixNano(), 10)
}

func (c *client) Stat(ctx context.Context, ref plugin.Ref) (plugin.ObjectInfo, error) {
	if err := ctx.Err(); err != nil {
		return plugin.ObjectInfo{}, err
	}
	if err := checkRef(ref); err != nil {
		return plugin.ObjectInfo{}, err
	}
	fi, err := c.root.Stat(ref.Key)
	if err != nil {
		return plugin.ObjectInfo{}, classify(err, ref)
	}
	if !fi.Mode().IsRegular() {
		return plugin.ObjectInfo{}, plugin.Wrapf(plugin.Permanent, "file: %s is not a regular file", ref.Key)
	}
	return plugin.ObjectInfo{Size: fi.Size(), Version: version(fi)}, nil
}

func (c *client) ReadRange(ctx context.Context, ref plugin.Ref, off, length int64, ifVersion string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := checkRef(ref); err != nil {
		return nil, err
	}
	f, err := c.root.Open(ref.Key)
	if err != nil {
		return nil, classify(err, ref)
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, classify(err, ref)
	}
	if ifVersion != "" && version(fi) != ifVersion {
		_ = f.Close()
		return nil, plugin.Wrapf(plugin.SourceChanged, "file: %s changed (version %s, expected %s)", ref.Key, version(fi), ifVersion)
	}
	if off < 0 || length < 0 || off+length > fi.Size() {
		_ = f.Close()
		return nil, plugin.Wrapf(plugin.Permanent, "file: range [%d, +%d) outside %s (%d bytes)", off, length, ref.Key, fi.Size())
	}
	return struct {
		io.Reader
		io.Closer
	}{io.NewSectionReader(f, off, length), f}, nil
}

func (c *client) Delete(ctx context.Context, ref plugin.Ref) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := checkRef(ref); err != nil {
		return err
	}
	if err := c.root.Remove(ref.Key); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return classify(err, ref)
	}
	return nil
}

// --- uploads -----------------------------------------------------------------

// token is the serialised form of an upload.
type token struct {
	ID string `json:"upload_id"`
}

var idRE = regexp.MustCompile(`^[0-9a-f]{32}$`)

// upload is a staging directory .veho-staging/<id>, holding the file "key"
// (the final key) and one file per part.
type upload struct {
	c   *client
	ref plugin.Ref
	id  string
}

func (u *upload) dir() string                { return staging + "/" + u.id }
func (u *upload) partName(partNo int) string { return fmt.Sprintf("%s/part-%06d", u.dir(), partNo) }

func (c *client) Begin(ctx context.Context, ref plugin.Ref, _ plugin.ObjectInfo, _ plugin.PutOptions) (plugin.Upload, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := checkRef(ref); err != nil {
		return nil, err
	}
	u := &upload{c: c, ref: ref, id: randomID()}
	if err := c.root.MkdirAll(u.dir(), 0o700); err != nil {
		return nil, classify(err, ref)
	}
	if err := c.root.WriteFile(u.dir()+"/key", []byte(ref.Key), 0o600); err != nil {
		return nil, classify(err, ref)
	}
	return u, nil
}

// randomID is a 128-bit hex identifier.
func randomID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b) // never fails (crypto/rand)
	return hex.EncodeToString(b)
}

func (c *client) Resume(ctx context.Context, ref plugin.Ref, tok []byte) (plugin.Upload, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := checkRef(ref); err != nil {
		return nil, err
	}
	var t token
	if err := json.Unmarshal(tok, &t); err != nil || !idRE.MatchString(t.ID) {
		return nil, plugin.Wrapf(plugin.ResumeInvalid, "file: malformed upload token")
	}
	u := &upload{c: c, ref: ref, id: t.ID}
	key, err := c.root.ReadFile(u.dir() + "/key")
	if errors.Is(err, fs.ErrNotExist) {
		return nil, plugin.Wrapf(plugin.ResumeInvalid, "file: upload %s is gone", t.ID)
	}
	if err != nil {
		return nil, classify(err, ref)
	}
	if string(key) != ref.Key {
		return nil, plugin.Wrapf(plugin.ResumeInvalid, "file: upload %s is of another key", t.ID)
	}
	return u, nil
}

func (u *upload) Token() []byte {
	b, _ := json.Marshal(token{ID: u.id}) // a struct of a string: cannot fail
	return b
}

// gone is ResumeInvalid when the upload's directory is missing.
func (u *upload) gone(err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return plugin.Wrapf(plugin.ResumeInvalid, "file: upload %s is gone", u.id)
	}
	return classify(err, u.ref)
}

func (u *upload) WritePart(ctx context.Context, partNo int, r io.Reader, length int64) (plugin.PartResult, error) {
	if err := ctx.Err(); err != nil {
		return plugin.PartResult{}, err
	}
	if partNo < 0 || partNo >= maxParts {
		return plugin.PartResult{}, plugin.Wrapf(plugin.Permanent, "file: part number %d out of range", partNo)
	}
	tmp := fmt.Sprintf("%s.tmp-%s", u.partName(partNo), randomID())
	f, err := u.c.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return plugin.PartResult{}, u.gone(err)
	}
	h := sha256.New()
	n, err := io.CopyN(io.MultiWriter(f, h), contextReader{ctx, r}, length)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = u.c.root.Remove(tmp)
		if ctx.Err() != nil {
			return plugin.PartResult{}, ctx.Err()
		}
		if errors.Is(err, io.EOF) {
			return plugin.PartResult{}, plugin.Wrapf(plugin.Integrity, "file: part %d: %d bytes of %d", partNo, n, length)
		}
		return plugin.PartResult{}, classify(err, u.ref)
	}
	if err := u.c.root.Rename(tmp, u.partName(partNo)); err != nil {
		_ = u.c.root.Remove(tmp)
		return plugin.PartResult{}, u.gone(err)
	}
	sum := hex.EncodeToString(h.Sum(nil))
	return plugin.PartResult{PartNo: partNo, Size: length, ETag: sum, SHA256: sum}, nil
}

// contextReader stops reading once ctx is done.
type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

func (u *upload) ListParts(ctx context.Context) ([]plugin.PartResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	d, err := u.c.root.Open(u.dir())
	if err != nil {
		return nil, u.gone(err)
	}
	defer func() { _ = d.Close() }()
	entries, err := d.ReadDir(-1)
	if err != nil {
		return nil, classify(err, u.ref)
	}
	var parts []plugin.PartResult
	for _, e := range entries {
		num, ok := strings.CutPrefix(e.Name(), "part-")
		if !ok || strings.Contains(num, ".") {
			continue
		}
		n, err := strconv.Atoi(num)
		if err != nil {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			return nil, classify(err, u.ref)
		}
		parts = append(parts, plugin.PartResult{PartNo: n, Size: fi.Size()})
	}
	slices.SortFunc(parts, func(a, b plugin.PartResult) int { return a.PartNo - b.PartNo })
	return parts, nil
}

func (u *upload) Commit(ctx context.Context, parts []plugin.PartResult) (plugin.ObjectInfo, error) {
	if err := ctx.Err(); err != nil {
		return plugin.ObjectInfo{}, err
	}
	var total int64
	for i, p := range parts {
		if p.PartNo != i {
			return plugin.ObjectInfo{}, plugin.Wrapf(plugin.Permanent, "file: parts must be 0 to %d in order, got %d at %d", len(parts)-1, p.PartNo, i)
		}
		total += p.Size
	}
	if _, err := u.c.root.Stat(u.dir()); errors.Is(err, fs.ErrNotExist) {
		// committed before (the directory goes last): the object is the answer
		info, err := u.c.Stat(ctx, u.ref)
		if err == nil && info.Size == total {
			return info, nil
		}
		return plugin.ObjectInfo{}, plugin.Wrapf(plugin.ResumeInvalid, "file: upload %s is gone", u.id)
	}
	tmp := u.dir() + "/object"
	out, err := u.c.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return plugin.ObjectInfo{}, u.gone(err)
	}
	err = u.concat(ctx, out, parts)
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return plugin.ObjectInfo{}, err
	}
	if dir := path.Dir(u.ref.Key); dir != "." {
		if err := u.c.root.MkdirAll(dir, 0o755); err != nil {
			return plugin.ObjectInfo{}, classify(err, u.ref)
		}
	}
	if err := u.c.root.Rename(tmp, u.ref.Key); err != nil {
		return plugin.ObjectInfo{}, classify(err, u.ref)
	}
	if err := u.c.root.RemoveAll(u.dir()); err != nil {
		return plugin.ObjectInfo{}, classify(err, u.ref)
	}
	return u.c.Stat(ctx, u.ref)
}

// concat appends the parts to out, checking each one's size.
func (u *upload) concat(ctx context.Context, out io.Writer, parts []plugin.PartResult) error {
	for _, p := range parts {
		if err := ctx.Err(); err != nil {
			return err
		}
		f, err := u.c.root.Open(u.partName(p.PartNo))
		if err != nil {
			return plugin.Wrap(fmt.Errorf("file: part %d: %w", p.PartNo, err), plugin.Integrity)
		}
		n, err := io.Copy(out, f)
		_ = f.Close()
		if err != nil {
			return classify(err, u.ref)
		}
		if n != p.Size {
			return plugin.Wrapf(plugin.Integrity, "file: part %d has %d bytes, %d expected", p.PartNo, n, p.Size)
		}
	}
	return nil
}

func (u *upload) Abort(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := u.c.root.RemoveAll(u.dir()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return classify(err, u.ref)
	}
	return nil
}
