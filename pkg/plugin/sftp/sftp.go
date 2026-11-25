// Package sftp is the driver of files on SSH servers over SFTP
// (.specs/veho-sources.md §3.2 and §3.3): a source that reads ranges, and a
// sink with staged, resumable uploads renamed into place. scp:// endpoints
// are SFTP endpoints, as every OpenSSH server that offers scp offers SFTP.
// It runs as the plugin veho-plugin-sftp.
package sftp

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
	"net"
	"net/url"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	pkgsftp "github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/dihedron/veho/internal/netguard"
	"github.com/dihedron/veho/pkg/plugin"
)

const (
	// staging is the directory under the root that holds the uploads.
	staging = ".veho-staging"
	// dialTimeout bounds the connection and the SSH handshake.
	dialTimeout = 15 * time.Second
	maxParts    = 10000
)

// Driver reads and writes files over SFTP, through the dial guard.
type Driver struct {
	guard *netguard.Guard
}

type settings struct {
	AllowCIDRs []string `json:"allow_cidrs"`
}

// Factory builds the driver from its settings: allow_cidrs, the ranges the
// dial guard allows besides the public addresses.
func Factory(raw json.RawMessage) (plugin.Driver, error) {
	var s settings
	if err := plugin.DecodeSettings(raw, &s); err != nil {
		return nil, fmt.Errorf("sftp: %w", err)
	}
	g, err := netguard.New(s.AllowCIDRs)
	if err != nil {
		return nil, fmt.Errorf("sftp: %w", err)
	}
	return Driver{guard: g}, nil
}

func (Driver) Name() string      { return "sftp" }
func (Driver) Schemes() []string { return []string{"sftp", "scp"} }

func (Driver) Capabilities() plugin.Capabilities {
	return plugin.Capabilities{Source: true, Sink: true, RangeRead: true, ResumableUpload: true, MaxParts: maxParts, MaxPartSize: 5 << 30}
}

// schema is the endpoint configuration's JSON Schema (PLG-10).
//
//go:embed schema.json
var schema []byte

func (Driver) ConfigSchema() []byte { return schema }

// ParseRef parses sftp:// and scp:// URLs; the key is the path without the
// leading slash, relative to the endpoint's root.
func (Driver) ParseRef(raw string) (plugin.Ref, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "sftp" && u.Scheme != "scp") || u.Host == "" {
		return plugin.Ref{}, plugin.Wrapf(plugin.Permanent, "sftp: %q is not an sftp:// or scp:// URL", raw)
	}
	key := strings.TrimPrefix(u.Path, "/")
	if err := checkKey(key); err != nil {
		return plugin.Ref{}, err
	}
	return plugin.Ref{Key: key}, nil
}

type config struct {
	Host    string `json:"host"`
	Port    int    `json:"port"`
	Root    string `json:"root"`
	HostKey string `json:"host_key"`
}

// Open checks the configuration and the credentials (username, and
// private_key, optionally with passphrase, or password); the connection is
// made at the first call.
func (d Driver) Open(ctx context.Context, cfg plugin.Config, creds plugin.Credentials) (plugin.Client, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var c config
	dec := json.NewDecoder(strings.NewReader(string(cfg)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, plugin.Wrapf(plugin.Permanent, "sftp: configuration: %v", err)
	}
	if c.Port == 0 {
		c.Port = 22
	}
	switch {
	case c.Host == "" || strings.ContainsAny(c.Host, "/@ "):
		return nil, plugin.Wrapf(plugin.Permanent, "sftp: configuration: a host is required")
	case c.Port < 1 || c.Port > 65535:
		return nil, plugin.Wrapf(plugin.Permanent, "sftp: configuration: port %d", c.Port)
	case !path.IsAbs(c.Root) || path.Clean(c.Root) != c.Root:
		return nil, plugin.Wrapf(plugin.Permanent, "sftp: configuration: root must be a clean absolute path")
	}
	hostKey, _, _, _, err := ssh.ParseAuthorizedKey([]byte(c.HostKey))
	if err != nil {
		return nil, plugin.Wrapf(plugin.Permanent, "sftp: configuration: host_key: %v", err)
	}
	auth, err := authMethods(creds)
	if err != nil {
		return nil, err
	}
	guard := d.guard
	if guard == nil {
		if guard, err = netguard.New(nil); err != nil {
			return nil, err
		}
	}
	return &client{
		addr: net.JoinHostPort(c.Host, strconv.Itoa(c.Port)), root: c.Root, guard: guard,
		ssh: &ssh.ClientConfig{User: creds["username"], Auth: auth, HostKeyCallback: ssh.FixedHostKey(hostKey), Timeout: dialTimeout},
	}, nil
}

func authMethods(creds plugin.Credentials) ([]ssh.AuthMethod, error) {
	for k := range creds {
		if k != "username" && k != "password" && k != "private_key" && k != "passphrase" {
			return nil, plugin.Wrapf(plugin.Permanent, "sftp: unknown credential %q", k)
		}
	}
	switch {
	case creds["username"] == "":
		return nil, plugin.Wrapf(plugin.Permanent, "sftp: credentials: a username is required")
	case creds["private_key"] != "" && creds["password"] == "":
		var (
			signer ssh.Signer
			err    error
		)
		if creds["passphrase"] != "" {
			signer, err = ssh.ParsePrivateKeyWithPassphrase([]byte(creds["private_key"]), []byte(creds["passphrase"]))
		} else {
			signer, err = ssh.ParsePrivateKey([]byte(creds["private_key"]))
		}
		if err != nil {
			return nil, plugin.Wrapf(plugin.Permanent, "sftp: credentials: private_key: %v", err)
		}
		return []ssh.AuthMethod{ssh.PublicKeys(signer)}, nil
	case creds["password"] != "" && creds["private_key"] == "":
		return []ssh.AuthMethod{ssh.Password(creds["password"])}, nil
	}
	return nil, plugin.Wrapf(plugin.Permanent, "sftp: credentials: private_key or password")
}

// client is the driver bound to one server and root.
type client struct {
	addr  string
	root  string
	guard *netguard.Guard
	ssh   *ssh.ClientConfig

	mu   sync.Mutex
	conn *ssh.Client
	sc   *pkgsftp.Client
}

// connect makes the connection, once; ctx bounds it.
func (c *client) connect(ctx context.Context) (*pkgsftp.Client, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sc != nil {
		return c.sc, nil
	}
	nc, err := c.guard.Dialer(dialTimeout).DialContext(ctx, "tcp", c.addr)
	if err != nil {
		if errors.Is(err, netguard.ErrForbidden) {
			return nil, plugin.Wrap(fmt.Errorf("sftp: %w", netguard.ErrForbidden), plugin.Permanent)
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, plugin.Wrap(fmt.Errorf("sftp: connect: %w", err), plugin.Transient)
	}
	_ = nc.SetDeadline(time.Now().Add(dialTimeout))
	sshConn, chans, reqs, err := ssh.NewClientConn(nc, c.addr, c.ssh)
	if err != nil {
		_ = nc.Close()
		if strings.Contains(err.Error(), "host key") {
			return nil, plugin.Wrap(fmt.Errorf("sftp: the server's host key is not the configured host_key: %w", err), plugin.Permanent)
		}
		if strings.Contains(err.Error(), "unable to authenticate") {
			return nil, plugin.Wrap(fmt.Errorf("sftp: %w", err), plugin.Permanent)
		}
		return nil, plugin.Wrap(fmt.Errorf("sftp: handshake: %w", err), plugin.Transient)
	}
	_ = nc.SetDeadline(time.Time{})
	conn := ssh.NewClient(sshConn, chans, reqs)
	sc, err := pkgsftp.NewClient(conn, pkgsftp.UseConcurrentWrites(true))
	if err != nil {
		_ = conn.Close()
		return nil, plugin.Wrap(fmt.Errorf("sftp: the SFTP subsystem: %w", err), plugin.Permanent)
	}
	c.conn, c.sc = conn, sc
	return sc, nil
}

func (c *client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sc == nil {
		return nil
	}
	_ = c.sc.Close()
	err := c.conn.Close()
	c.sc, c.conn = nil, nil
	return err
}

// call runs f on the connection, cut when ctx ends: the calls of pkg/sftp
// take no context, so the connection is closed, and the next call makes a
// new one.
func (c *client) call(ctx context.Context, f func(sc *pkgsftp.Client) error) error {
	sc, err := c.connect(ctx)
	if err != nil {
		return err
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = c.Close()
		case <-done:
		}
	}()
	err = f(sc)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// checkKey refuses a key that is not a clean relative path, or that reaches
// the staging directory (SRC-W4).
func checkKey(key string) error {
	if key == "" || key == "." || path.IsAbs(key) || path.Clean(key) != key || key == ".." || strings.HasPrefix(key, "../") ||
		key == staging || strings.HasPrefix(key, staging+"/") || strings.ContainsRune(key, 0) {
		return plugin.Wrapf(plugin.Permanent, "sftp: invalid key %q", key)
	}
	return nil
}

// join is the path of key under root.
func join(root, key string) (string, error) {
	if err := checkKey(key); err != nil {
		return "", err
	}
	return path.Join(root, key), nil
}

// noLinks refuses a path under the root with a symbolic link in it, which
// could lead out of the root (SRC-W4); the root itself is the operator's.
func (c *client) noLinks(sc *pkgsftp.Client, p string) error {
	rel := strings.TrimPrefix(p, c.root+"/")
	cur := c.root
	for _, seg := range strings.Split(rel, "/") {
		cur = path.Join(cur, seg)
		fi, err := sc.Lstat(cur)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return plugin.Wrapf(plugin.Permanent, "sftp: %s: a symbolic link, not followed", rel)
		}
	}
	return nil
}

// classify maps an SFTP error.
func classify(err error, key string) error {
	switch {
	case err == nil:
		return nil
	case plugin.ClassOf(err) != plugin.Unknown, errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("sftp: %s: %w", key, plugin.ErrNotFound)
	case errors.Is(err, fs.ErrPermission):
		return plugin.Wrap(fmt.Errorf("sftp: %s: %w", key, err), plugin.Permanent)
	}
	return plugin.Wrap(fmt.Errorf("sftp: %s: %w", key, err), plugin.Transient)
}

func version(fi fs.FileInfo) string {
	return strconv.FormatInt(fi.Size(), 10) + "-" + strconv.FormatInt(fi.ModTime().Unix(), 10)
}

// stat is the regular file at p, no link on the way.
func (c *client) stat(sc *pkgsftp.Client, p string) (fs.FileInfo, error) {
	if err := c.noLinks(sc, p); err != nil {
		return nil, err
	}
	fi, err := sc.Lstat(p)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, plugin.Wrapf(plugin.Permanent, "sftp: %s is not a regular file", strings.TrimPrefix(p, c.root+"/"))
	}
	return fi, nil
}

// Stat gives the size and the version, <size>-<mtime in seconds> (SRC-1,
// R-15).
func (c *client) Stat(ctx context.Context, ref plugin.Ref) (plugin.ObjectInfo, error) {
	p, err := c.path(ref)
	if err != nil {
		return plugin.ObjectInfo{}, err
	}
	var info plugin.ObjectInfo
	err = c.call(ctx, func(sc *pkgsftp.Client) error {
		fi, err := c.stat(sc, p)
		if err != nil {
			return err
		}
		info = plugin.ObjectInfo{Size: fi.Size(), Version: version(fi)}
		return nil
	})
	return info, classify(err, ref.Key)
}

func (c *client) path(ref plugin.Ref) (string, error) {
	if ref.Container != "" {
		return "", plugin.Wrapf(plugin.Permanent, "sftp: no containers (got %q)", ref.Container)
	}
	return join(c.root, ref.Key)
}

// ReadRange checks the version, reads at the offset, and checks the version
// again when the range is read (SRC-2).
func (c *client) ReadRange(ctx context.Context, ref plugin.Ref, off, length int64, ifVersion string) (io.ReadCloser, error) {
	if off < 0 || length < 0 {
		return nil, plugin.Wrapf(plugin.Permanent, "sftp: range [%d, +%d)", off, length)
	}
	p, err := c.path(ref)
	if err != nil {
		return nil, err
	}
	sc, err := c.connect(ctx)
	if err != nil {
		return nil, err
	}
	fi, err := c.stat(sc, p)
	if err != nil {
		return nil, classify(err, ref.Key)
	}
	if ifVersion != "" && version(fi) != ifVersion {
		return nil, plugin.Wrapf(plugin.SourceChanged, "sftp: %s changed", ref.Key)
	}
	if off+length > fi.Size() {
		return nil, plugin.Wrapf(plugin.Permanent, "sftp: %s: range [%d, +%d) beyond the size %d", ref.Key, off, length, fi.Size())
	}
	f, err := sc.Open(p)
	if err != nil {
		return nil, classify(err, ref.Key)
	}
	return &reader{c: c, sc: sc, f: f, r: io.NewSectionReader(f, off, length), left: length, path: p, key: ref.Key, version: ifVersion, ctx: ctx}, nil
}

// reader reads exactly the range, then checks the version again.
type reader struct {
	c       *client
	sc      *pkgsftp.Client
	f       *pkgsftp.File
	r       io.Reader
	left    int64
	path    string
	key     string
	version string
	ctx     context.Context
	done    bool
}

func (r *reader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if r.left == 0 {
		return 0, r.end()
	}
	n, err := r.r.Read(p)
	r.left -= int64(n)
	switch {
	case errors.Is(err, io.EOF) && r.left > 0:
		return n, plugin.Wrapf(plugin.Transient, "sftp: %s: the file ended %d bytes short", r.key, r.left)
	case err != nil && !errors.Is(err, io.EOF):
		return n, classify(err, r.key)
	case r.left == 0:
		return n, r.end()
	}
	return n, nil
}

// end is io.EOF if the file is still at its version.
func (r *reader) end() error {
	if r.done || r.version == "" {
		return io.EOF
	}
	r.done = true
	fi, err := r.sc.Lstat(r.path)
	if err != nil {
		return classify(err, r.key)
	}
	if version(fi) != r.version {
		return plugin.Wrapf(plugin.SourceChanged, "sftp: %s changed during the read", r.key)
	}
	return io.EOF
}

func (r *reader) Close() error { return r.f.Close() }

// --- the sink: <root>/.veho-staging/<id>/data, written part after part,
// with a marker per part, renamed into place at commit -----------------------

type token struct {
	ID string `json:"id"`
}

func (c *client) stagingDir(id string) string { return path.Join(c.root, staging, id) }

// Begin creates the upload's staging directory.
func (c *client) Begin(ctx context.Context, ref plugin.Ref, _ plugin.ObjectInfo, _ plugin.PutOptions) (plugin.Upload, error) {
	p, err := c.path(ref)
	if err != nil {
		return nil, err
	}
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	id := hex.EncodeToString(b)
	err = c.call(ctx, func(sc *pkgsftp.Client) error {
		if err := sc.MkdirAll(path.Join(c.stagingDir(id), "parts")); err != nil {
			return err
		}
		f, err := sc.OpenFile(path.Join(c.stagingDir(id), "data"), os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
		if err != nil {
			return err
		}
		return f.Close()
	})
	if err != nil {
		return nil, classify(err, ref.Key)
	}
	return &upload{c: c, id: id, dest: p, key: ref.Key}, nil
}

// Resume re-attaches to the upload of the token; ResumeInvalid when its
// staging directory is gone.
func (c *client) Resume(ctx context.Context, ref plugin.Ref, tok []byte) (plugin.Upload, error) {
	p, err := c.path(ref)
	if err != nil {
		return nil, err
	}
	var t token
	if err := json.Unmarshal(tok, &t); err != nil || len(t.ID) != 32 || strings.Trim(t.ID, "0123456789abcdef") != "" {
		return nil, plugin.Wrapf(plugin.ResumeInvalid, "sftp: a malformed token")
	}
	err = c.call(ctx, func(sc *pkgsftp.Client) error {
		_, err := sc.Stat(path.Join(c.stagingDir(t.ID), "data"))
		return err
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil, plugin.Wrapf(plugin.ResumeInvalid, "sftp: the upload is gone")
	}
	if err != nil {
		return nil, classify(err, ref.Key)
	}
	return &upload{c: c, id: t.ID, dest: p, key: ref.Key}, nil
}

// Delete removes the file; an absent one is no error.
func (c *client) Delete(ctx context.Context, ref plugin.Ref) error {
	p, err := c.path(ref)
	if err != nil {
		return err
	}
	err = c.call(ctx, func(sc *pkgsftp.Client) error {
		if err := c.noLinks(sc, p); err != nil {
			return err
		}
		return sc.Remove(p)
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return classify(err, ref.Key)
}

type upload struct {
	c    *client
	id   string
	dest string
	key  string
}

func (u *upload) Token() []byte {
	b, _ := json.Marshal(token{ID: u.id})
	return b
}

// parts lists the markers, parts/<n>-<size>, by part number.
func (u *upload) parts(sc *pkgsftp.Client) ([]plugin.PartResult, error) {
	entries, err := sc.ReadDir(path.Join(u.c.stagingDir(u.id), "parts"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, plugin.Wrapf(plugin.ResumeInvalid, "sftp: the upload is gone")
	}
	if err != nil {
		return nil, err
	}
	var out []plugin.PartResult
	for _, e := range entries {
		no, size, ok := strings.Cut(e.Name(), "-")
		n, err1 := strconv.Atoi(no)
		s, err2 := strconv.ParseInt(size, 10, 64)
		if !ok || err1 != nil || err2 != nil {
			continue
		}
		out = append(out, plugin.PartResult{PartNo: n, Size: s})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PartNo < out[j].PartNo })
	return out, nil
}

// offset is where part n starts: after parts 0 to n-1, which must be there
// (parts are written in order).
func offset(parts []plugin.PartResult, n int) (int64, error) {
	var off int64
	for i := range n {
		if i >= len(parts) || parts[i].PartNo != i {
			return 0, plugin.Wrapf(plugin.Permanent, "sftp: part %d before part %d", n, i)
		}
		off += parts[i].Size
	}
	return off, nil
}

// WritePart writes part n at its offset in the staging file, then its
// marker; writing it again with the same bytes is harmless.
func (u *upload) WritePart(ctx context.Context, n int, r io.Reader, length int64) (plugin.PartResult, error) {
	if n < 0 || n >= maxParts || length < 0 {
		return plugin.PartResult{}, plugin.Wrapf(plugin.Permanent, "sftp: part %d of %d bytes", n, length)
	}
	var res plugin.PartResult
	err := u.c.call(ctx, func(sc *pkgsftp.Client) error {
		parts, err := u.parts(sc)
		if err != nil {
			return err
		}
		off, err := offset(parts, n)
		if err != nil {
			return err
		}
		f, err := sc.OpenFile(path.Join(u.c.stagingDir(u.id), "data"), os.O_WRONLY)
		if err != nil {
			return err
		}
		h := sha256.New()
		w := io.NewOffsetWriter(f, off)
		written, err := io.Copy(io.MultiWriter(w, h), io.LimitReader(r, length))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
		if written != length {
			return plugin.Wrapf(plugin.Transient, "sftp: part %d: %d bytes of %d", n, written, length)
		}
		for _, p := range parts {
			if p.PartNo == n && p.Size != length {
				return plugin.Wrapf(plugin.Permanent, "sftp: part %d again with %d bytes, was %d", n, length, p.Size)
			}
		}
		marker, err := sc.OpenFile(path.Join(u.c.stagingDir(u.id), "parts", fmt.Sprintf("%d-%d", n, length)), os.O_WRONLY|os.O_CREATE)
		if err != nil {
			return err
		}
		res = plugin.PartResult{PartNo: n, Size: length, SHA256: hex.EncodeToString(h.Sum(nil))}
		return marker.Close()
	})
	return res, classify(err, u.key)
}

func (u *upload) ListParts(ctx context.Context) ([]plugin.PartResult, error) {
	var out []plugin.PartResult
	err := u.c.call(ctx, func(sc *pkgsftp.Client) error {
		var err error
		out, err = u.parts(sc)
		return err
	})
	return out, classify(err, u.key)
}

// Commit truncates the staging file to the parts' total and renames it
// into place, atomically with posix-rename where the server has it; a
// commit again, after the rename, returns the object (PLG-7).
func (u *upload) Commit(ctx context.Context, want []plugin.PartResult) (plugin.ObjectInfo, error) {
	var info plugin.ObjectInfo
	err := u.c.call(ctx, func(sc *pkgsftp.Client) error {
		var total int64
		for i, p := range want {
			if p.PartNo != i {
				return plugin.Wrapf(plugin.Permanent, "sftp: commit: part %d is missing", i)
			}
			total += p.Size
		}
		dir := u.c.stagingDir(u.id)
		data := path.Join(dir, "data")
		if _, err := sc.Stat(data); errors.Is(err, fs.ErrNotExist) {
			// committed already?
			fi, err := u.c.stat(sc, u.dest)
			if err == nil && fi.Size() == total {
				info = plugin.ObjectInfo{Size: fi.Size(), Version: version(fi)}
				return nil
			}
			return plugin.Wrapf(plugin.ResumeInvalid, "sftp: the upload is gone")
		}
		have, err := u.parts(sc)
		if err != nil {
			return err
		}
		if len(have) < len(want) {
			return plugin.Wrapf(plugin.Permanent, "sftp: commit: %d parts written, %d committed", len(have), len(want))
		}
		for i, p := range want {
			if have[i].Size != p.Size {
				return plugin.Wrapf(plugin.Permanent, "sftp: commit: part %d has %d bytes, %d committed", i, have[i].Size, p.Size)
			}
		}
		if err := sc.Truncate(data, total); err != nil {
			return err
		}
		if err := u.c.noLinks(sc, u.dest); err != nil {
			return err
		}
		if err := sc.MkdirAll(path.Dir(u.dest)); err != nil {
			return err
		}
		if _, ok := sc.HasExtension("posix-rename@openssh.com"); ok {
			err = sc.PosixRename(data, u.dest)
		} else {
			_ = sc.Remove(u.dest) // shortcut: not atomic without posix-rename; such servers are rare
			err = sc.Rename(data, u.dest)
		}
		if err != nil {
			return err
		}
		_ = sc.RemoveAll(dir)
		fi, err := sc.Lstat(u.dest)
		if err != nil {
			return err
		}
		info = plugin.ObjectInfo{Size: fi.Size(), Version: version(fi)}
		return nil
	})
	return info, classify(err, u.key)
}

// Abort removes the staging directory; one that is gone is no error.
func (u *upload) Abort(ctx context.Context) error {
	err := u.c.call(ctx, func(sc *pkgsftp.Client) error { return sc.RemoveAll(u.c.stagingDir(u.id)) })
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return classify(err, u.key)
}
