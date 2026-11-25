// Package ftp is the source driver of files on FTP servers
// (.specs/veho-sources.md §3.4): FTPS by default, plain FTP only when the
// platform allows it, passive mode only, every data connection to the
// control connection's address. Read-only, by ranges (REST and RETR). It
// runs as the plugin veho-plugin-ftp.
package ftp

import (
	"context"
	"crypto/tls"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jlaffaye/ftp"

	"github.com/dihedron/veho/internal/netguard"
	"github.com/dihedron/veho/pkg/plugin"
)

const dialTimeout = 15 * time.Second

// Driver reads files from FTP servers, through the dial guard.
type Driver struct {
	guard     *netguard.Guard
	plaintext bool
	// tlsConfig is for tests, which trust their own server
	tlsConfig *tls.Config
}

type settings struct {
	AllowCIDRs []string `json:"allow_cidrs"`
	// Plaintext allows endpoints with tls: none, whose password crosses the
	// network in clear.
	Plaintext bool `json:"plaintext"`
}

// Factory builds the driver from its settings: allow_cidrs and plaintext.
func Factory(raw json.RawMessage) (plugin.Driver, error) {
	var s settings
	if err := plugin.DecodeSettings(raw, &s); err != nil {
		return nil, fmt.Errorf("ftp: %w", err)
	}
	g, err := netguard.New(s.AllowCIDRs)
	if err != nil {
		return nil, fmt.Errorf("ftp: %w", err)
	}
	return Driver{guard: g, plaintext: s.Plaintext}, nil
}

func (Driver) Name() string      { return "ftp" }
func (Driver) Schemes() []string { return []string{"ftp", "ftps"} }

func (Driver) Capabilities() plugin.Capabilities {
	return plugin.Capabilities{Source: true, RangeRead: true}
}

// schema is the endpoint configuration's JSON Schema (PLG-10).
//
//go:embed schema.json
var schema []byte

func (Driver) ConfigSchema() []byte { return schema }

// ParseRef parses ftp:// and ftps:// URLs; the key is the path without the
// leading slash, relative to the endpoint's root.
func (Driver) ParseRef(raw string) (plugin.Ref, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "ftp" && u.Scheme != "ftps") || u.Host == "" {
		return plugin.Ref{}, plugin.Wrapf(plugin.Permanent, "ftp: %q is not an ftp:// or ftps:// URL", raw)
	}
	key := strings.TrimPrefix(u.Path, "/")
	if _, err := join("/", key); err != nil {
		return plugin.Ref{}, err
	}
	return plugin.Ref{Key: key}, nil
}

type config struct {
	Host string `json:"host"`
	Port int    `json:"port"`
	Root string `json:"root"`
	TLS  string `json:"tls"`
}

// Open checks the configuration and the credentials (username and
// password; neither is anonymous FTP).
// parse checks a configuration without credentials or network, its
// defaults applied.
func (d Driver) parse(cfg plugin.Config) (config, error) {
	var c config
	dec := json.NewDecoder(strings.NewReader(string(cfg)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return config{}, plugin.Wrapf(plugin.Permanent, "ftp: configuration: %v", err)
	}
	if c.TLS == "" {
		c.TLS = "explicit"
	}
	if c.Port == 0 {
		c.Port = 21
		if c.TLS == "implicit" {
			c.Port = 990
		}
	}
	switch {
	case c.Host == "" || strings.ContainsAny(c.Host, "/@ "):
		return config{}, plugin.Wrapf(plugin.Permanent, "ftp: configuration: a host is required")
	case c.Port < 1 || c.Port > 65535:
		return config{}, plugin.Wrapf(plugin.Permanent, "ftp: configuration: port %d", c.Port)
	case !path.IsAbs(c.Root) || path.Clean(c.Root) != c.Root || strings.ContainsAny(c.Root, "\r\n\x00"):
		return config{}, plugin.Wrapf(plugin.Permanent, "ftp: configuration: root must be a clean absolute path")
	case c.TLS != "explicit" && c.TLS != "implicit" && c.TLS != "none":
		return config{}, plugin.Wrapf(plugin.Permanent, "ftp: configuration: tls must be explicit, implicit or none")
	case c.TLS == "none" && !d.plaintext:
		return config{}, plugin.Wrapf(plugin.Permanent, "ftp: tls: none is not allowed on this platform (the driver's plaintext setting)")
	}
	return c, nil
}

// CheckConfig is what Open checks before the credentials (PLG-10).
func (d Driver) CheckConfig(_ context.Context, cfg plugin.Config) error {
	_, err := d.parse(cfg)
	return err
}

func (d Driver) Open(ctx context.Context, cfg plugin.Config, creds plugin.Credentials) (plugin.Client, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c, err := d.parse(cfg)
	if err != nil {
		return nil, err
	}
	for k := range creds {
		if k != "username" && k != "password" {
			return nil, plugin.Wrapf(plugin.Permanent, "ftp: unknown credential %q", k)
		}
	}
	user, pass := creds["username"], creds["password"]
	if (user == "") != (pass == "") {
		return nil, plugin.Wrapf(plugin.Permanent, "ftp: credentials: username and password, or neither (anonymous)")
	}
	if user == "" {
		user, pass = "anonymous", "anonymous"
	}
	guard := d.guard
	if guard == nil {
		var err error
		if guard, err = netguard.New(nil); err != nil {
			return nil, err
		}
	}
	tlsConfig := d.tlsConfig
	if tlsConfig == nil {
		tlsConfig = &tls.Config{MinVersion: tls.VersionTLS12, ServerName: c.Host,
			// servers that require the data connection to resume the
			// control connection's session (vsftpd's require_ssl_reuse)
			ClientSessionCache: tls.NewLRUClientSessionCache(4)}
	}
	return &client{addr: net.JoinHostPort(c.Host, strconv.Itoa(c.Port)), root: c.Root, mode: c.TLS,
		user: user, pass: pass, guard: guard, tls: tlsConfig}, nil
}

// pinned is a dialer through the guard whose connections after the first
// must go to the first one's address: the data connections go where the
// control connection went (SRC-W1, no FXP).
func pinned(g *netguard.Guard, timeout time.Duration) net.Dialer {
	var (
		mu   sync.Mutex
		host string
	)
	return net.Dialer{Timeout: timeout, Control: func(network, address string, c syscall.RawConn) error {
		if err := g.Control(network, address, c); err != nil {
			return err
		}
		h, _, err := net.SplitHostPort(address)
		if err != nil {
			return netguard.ErrForbidden
		}
		mu.Lock()
		defer mu.Unlock()
		if host == "" {
			host = h
		} else if h != host {
			return netguard.ErrForbidden
		}
		return nil
	}}
}

// client is the driver bound to one server and root.
type client struct {
	addr, root, mode string
	user, pass       string
	guard            *netguard.Guard
	tls              *tls.Config

	mu   sync.Mutex
	conn *ftp.ServerConn // for Stat
}

// Flows is the control connection (TLS from its start with implicit FTPS;
// explicit FTPS upgrades after AUTH TLS, which is not probed: TCP only)
// and the data connections, whose passive ports the server chooses.
func (c *client) Flows(context.Context) ([]plugin.Flow, error) {
	host, port, _ := net.SplitHostPort(c.addr) // made by JoinHostPort
	p, _ := strconv.Atoi(port)
	control := plugin.Flow{Purpose: "ftp control", Host: host, Port: p}
	if c.mode == "implicit" {
		control.TLS, control.ServerName = true, host
	}
	return []plugin.Flow{control, {Purpose: "ftp data", Host: host, Unprobed: "passive data ports, chosen by the server for each transfer"}}, nil
}

// dial makes a control connection and logs in.
func (c *client) dial(ctx context.Context) (*ftp.ServerConn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	opts := []ftp.DialOption{ftp.DialWithContext(ctx), ftp.DialWithDialer(pinned(c.guard, dialTimeout)), ftp.DialWithTimeout(dialTimeout)}
	switch c.mode {
	case "explicit":
		opts = append(opts, ftp.DialWithExplicitTLS(c.tls))
	case "implicit":
		opts = append(opts, ftp.DialWithTLS(c.tls))
	}
	conn, err := ftp.Dial(c.addr, opts...)
	if err != nil {
		return nil, c.classify(ctx, "connect", err, "")
	}
	if err := conn.Login(c.user, c.pass); err != nil {
		_ = conn.Quit()
		if isCode(err, 530) {
			return nil, plugin.Wrap(fmt.Errorf("ftp: login: %w", err), plugin.Permanent)
		}
		return nil, c.classify(ctx, "login", err, "")
	}
	return conn, nil
}

func isCode(err error, codes ...int) bool {
	var te *textproto.Error
	if !errors.As(err, &te) {
		return false
	}
	for _, c := range codes {
		if te.Code == c {
			return true
		}
	}
	return false
}

func (c *client) classify(ctx context.Context, op string, err error, key string) error {
	switch {
	case err == nil:
		return nil
	case ctx.Err() != nil:
		return ctx.Err()
	case plugin.ClassOf(err) != plugin.Unknown:
		return err
	case errors.Is(err, netguard.ErrForbidden):
		return plugin.Wrap(fmt.Errorf("ftp: %w", netguard.ErrForbidden), plugin.Permanent)
	case isCode(err, 550):
		return fmt.Errorf("ftp: %s: %w", key, plugin.ErrNotFound)
	case isCode(err, 530, 532, 534, 504, 502):
		return plugin.Wrap(fmt.Errorf("ftp: %s: %w", op, err), plugin.Permanent)
	}
	var cert *tls.CertificateVerificationError
	if errors.As(err, &cert) {
		return plugin.Wrap(fmt.Errorf("ftp: the server's certificate: %w", cert.Err), plugin.Permanent)
	}
	return plugin.Wrap(fmt.Errorf("ftp: %s %s: %w", op, key, err), plugin.Transient)
}

func (c *client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return nil
	}
	err := c.conn.Quit()
	c.conn = nil
	return err
}

// join is the path of key under root: a clean relative path, without the
// characters that would end an FTP command (SRC-W4).
func join(root, key string) (string, error) {
	if key == "" || key == "." || path.IsAbs(key) || path.Clean(key) != key || key == ".." || strings.HasPrefix(key, "../") ||
		strings.ContainsAny(key, "\r\n\x00") {
		return "", plugin.Wrapf(plugin.Permanent, "ftp: invalid key %q", key)
	}
	return path.Join(root, key), nil
}

func (c *client) path(ref plugin.Ref) (string, error) {
	if ref.Container != "" {
		return "", plugin.Wrapf(plugin.Permanent, "ftp: no containers (got %q)", ref.Container)
	}
	return join(c.root, ref.Key)
}

// stat is SIZE and MDTM on conn: the version is <size>-<mtime in seconds>
// (R-15).
func (c *client) stat(ctx context.Context, conn *ftp.ServerConn, p, key string) (plugin.ObjectInfo, error) {
	size, err := conn.FileSize(p)
	if err != nil {
		return plugin.ObjectInfo{}, c.classify(ctx, "SIZE", err, key)
	}
	mod, err := conn.GetTime(p)
	if err != nil {
		return plugin.ObjectInfo{}, c.classify(ctx, "MDTM", err, key)
	}
	return plugin.ObjectInfo{Size: size, Version: strconv.FormatInt(size, 10) + "-" + strconv.FormatInt(mod.Unix(), 10)}, nil
}

// Stat runs on one control connection, kept for the client's later Stats.
func (c *client) Stat(ctx context.Context, ref plugin.Ref) (plugin.ObjectInfo, error) {
	p, err := c.path(ref)
	if err != nil {
		return plugin.ObjectInfo{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		if c.conn, err = c.dial(ctx); err != nil {
			return plugin.ObjectInfo{}, err
		}
	}
	info, err := c.stat(ctx, c.conn, p, ref.Key)
	if err != nil && plugin.ClassOf(err) == plugin.Transient {
		_ = c.conn.Quit() // a broken connection: the next Stat makes another
		c.conn = nil
	}
	return info, err
}

// ReadRange checks the version, reads with REST and RETR on a connection
// of its own, and checks the version again when the range is read (SRC-2).
func (c *client) ReadRange(ctx context.Context, ref plugin.Ref, off, length int64, ifVersion string) (io.ReadCloser, error) {
	if off < 0 || length < 0 {
		return nil, plugin.Wrapf(plugin.Permanent, "ftp: range [%d, +%d)", off, length)
	}
	p, err := c.path(ref)
	if err != nil {
		return nil, err
	}
	conn, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	info, err := c.stat(ctx, conn, p, ref.Key)
	if err == nil && ifVersion != "" && info.Version != ifVersion {
		err = plugin.Wrapf(plugin.SourceChanged, "ftp: %s changed", ref.Key)
	}
	if err == nil && off+length > info.Size {
		err = plugin.Wrapf(plugin.Permanent, "ftp: %s: range [%d, +%d) beyond the size %d", ref.Key, off, length, info.Size)
	}
	if err != nil {
		_ = conn.Quit()
		return nil, err
	}
	r := &reader{c: c, conn: conn, ctx: ctx, path: p, key: ref.Key, version: ifVersion, left: length}
	if length == 0 {
		return r, nil
	}
	resp, err := conn.RetrFrom(p, uint64(off))
	if err != nil {
		_ = conn.Quit()
		return nil, c.classify(ctx, "RETR", err, ref.Key)
	}
	r.resp = resp
	return r, nil
}

// reader reads exactly the range from the data connection, then closes it
// and checks the version again on the control connection.
type reader struct {
	c       *client
	conn    *ftp.ServerConn
	resp    *ftp.Response
	ctx     context.Context
	path    string
	key     string
	version string
	left    int64
	done    bool
	closed  bool
}

func (r *reader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if r.left == 0 {
		return 0, r.end()
	}
	if int64(len(p)) > r.left {
		p = p[:r.left]
	}
	n, err := r.resp.Read(p)
	r.left -= int64(n)
	switch {
	case errors.Is(err, io.EOF) && r.left > 0:
		return n, plugin.Wrapf(plugin.Transient, "ftp: %s: the file ended %d bytes short", r.key, r.left)
	case err != nil && !errors.Is(err, io.EOF):
		return n, r.c.classify(r.ctx, "RETR", err, r.key)
	case r.left == 0:
		return n, r.end()
	}
	return n, nil
}

// end closes the data connection and checks the version: io.EOF if the
// file is still at its version.
func (r *reader) end() error {
	if r.done {
		return io.EOF
	}
	r.done = true
	if r.resp != nil {
		// a transfer cut before the end is answered 426 or 450: the bytes
		// asked for were all read, so that is no error
		_ = r.resp.Close()
		r.resp = nil
	}
	if r.version == "" {
		return io.EOF
	}
	info, err := r.c.stat(r.ctx, r.conn, r.path, r.key)
	if err != nil {
		return err
	}
	if info.Version != r.version {
		return plugin.Wrapf(plugin.SourceChanged, "ftp: %s changed during the read", r.key)
	}
	return io.EOF
}

func (r *reader) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	if r.resp != nil {
		_ = r.resp.Close()
	}
	return r.conn.Quit()
}

// The driver is a source only.

func (c *client) Begin(context.Context, plugin.Ref, plugin.ObjectInfo, plugin.PutOptions) (plugin.Upload, error) {
	return nil, plugin.Wrapf(plugin.Permanent, "ftp: a source only")
}

func (c *client) Resume(context.Context, plugin.Ref, []byte) (plugin.Upload, error) {
	return nil, plugin.Wrapf(plugin.Permanent, "ftp: a source only")
}

func (c *client) Delete(context.Context, plugin.Ref) error {
	return plugin.Wrapf(plugin.Permanent, "ftp: a source only")
}
