package sftp

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	pkgsftp "github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/dihedron/veho/internal/netguard"
	"github.com/dihedron/veho/pkg/plugin"
	"github.com/dihedron/veho/pkg/plugin/conformance"
)

// server is an SSH server on the loopback with the SFTP subsystem of
// pkg/sftp, serving the real file system; root is the endpoint's root.
type server struct {
	addr     string
	hostKey  string // authorized_keys form
	root     string
	user     string
	password string
	userKey  []byte // PEM of the user's private key
}

func newServer(t *testing.T) *server {
	t.Helper()
	_, hostPriv, _ := ed25519.GenerateKey(rand.Reader)
	hostSigner, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatal(err)
	}
	userPub, userPriv, _ := ed25519.GenerateKey(rand.Reader)
	block, err := ssh.MarshalPrivateKey(userPriv, "")
	if err != nil {
		t.Fatal(err)
	}
	sshUserPub, _ := ssh.NewPublicKey(userPub)
	s := &server{root: t.TempDir(), user: "veho", password: "secret", userKey: pem.EncodeToMemory(block),
		hostKey: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(hostSigner.PublicKey())))}
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
			if c.User() == s.user && string(pw) == s.password {
				return nil, nil
			}
			return nil, errors.New("no")
		},
		PublicKeyCallback: func(c ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
			if c.User() == s.user && bytes.Equal(k.Marshal(), sshUserPub.Marshal()) {
				return nil, nil
			}
			return nil, errors.New("no")
		},
	}
	cfg.AddHostKey(hostSigner)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	s.addr = l.Addr().String()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go serve(c, cfg)
		}
	}()
	return s
}

func serve(c net.Conn, cfg *ssh.ServerConfig) {
	defer func() { _ = c.Close() }()
	_, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		if nc.ChannelType() != "session" {
			_ = nc.Reject(ssh.UnknownChannelType, "")
			continue
		}
		ch, in, err := nc.Accept()
		if err != nil {
			return
		}
		go func() {
			for req := range in {
				ok := req.Type == "subsystem" && len(req.Payload) > 4 && string(req.Payload[4:]) == "sftp"
				_ = req.Reply(ok, nil)
				if ok {
					srv, err := pkgsftp.NewServer(ch)
					if err == nil {
						_ = srv.Serve()
					}
					_ = ch.Close()
				}
			}
		}()
	}
}

func (s *server) config(t *testing.T, extra string) plugin.Config {
	host, port, _ := net.SplitHostPort(s.addr)
	return plugin.Config(`{"host": "` + host + `", "port": ` + port + `, "root": "` + s.root + `", "host_key": "` + s.hostKey + `"` + extra + `}`)
}

func (s *server) creds() plugin.Credentials {
	return plugin.Credentials{"username": s.user, "password": s.password}
}

func loopback(t *testing.T) Driver {
	t.Helper()
	g, err := netguard.New([]string{"127.0.0.0/8"})
	if err != nil {
		t.Fatal(err)
	}
	return Driver{guard: g}
}

// TestConformance: SRC-T7, the suite as a source and a sink, confined.
func TestConformance(t *testing.T) {
	s := newServer(t)
	conformance.Run(t, conformance.Target{Driver: loopback(t), Config: s.config(t, ""), Credentials: s.creds(),
		BadCredentials: plugin.Credentials{"username": s.user, "password": "wrong"}, Confined: true, PartSize: 64 << 10})
}

// TestHostKey: SRC-T4 and SRC-W2, another host key is refused; there is no
// trust on first use.
func TestHostKey(t *testing.T) {
	s := newServer(t)
	other := newServer(t)
	d := loopback(t)
	cfg := strings.Replace(string(s.config(t, "")), s.hostKey, other.hostKey, 1)
	c, err := d.Open(context.Background(), plugin.Config(cfg), s.creds())
	if err == nil {
		defer func() { _ = c.Close() }()
		_, err = c.Stat(context.Background(), plugin.Ref{Key: "x"})
	}
	if err == nil || plugin.ClassOf(err) != plugin.Permanent || !strings.Contains(err.Error(), "host key") {
		t.Errorf("another host key: %v, want a Permanent host key error", err)
	}
	noKey := strings.Replace(string(s.config(t, "")), `"host_key": "`+s.hostKey+`"`, `"host_key": ""`, 1)
	if _, err := d.Open(context.Background(), plugin.Config(noKey), s.creds()); err == nil {
		t.Error("no host key: accepted")
	}
}

// TestPrivateKey: a private key logs in as well as a password.
func TestPrivateKey(t *testing.T) {
	s := newServer(t)
	if err := os.WriteFile(filepath.Join(s.root, "a"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := loopback(t).Open(context.Background(), s.config(t, ""), plugin.Credentials{"username": s.user, "private_key": string(s.userKey)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if info, err := c.Stat(context.Background(), plugin.Ref{Key: "a"}); err != nil || info.Size != 5 {
		t.Errorf("Stat: %+v %v", info, err)
	}
	for _, creds := range []plugin.Credentials{{"username": s.user}, {"password": "x"}, {"username": s.user, "private_key": "not a key"}, {"username": s.user, "password": "p", "other": "x"}} {
		if c, err := loopback(t).Open(context.Background(), s.config(t, ""), creds); err == nil {
			_ = c.Close()
			t.Errorf("credentials %v: accepted", creds)
		}
	}
}

// TestSymlinkEscape: SRC-W4, a link inside the root to a file outside it
// is not followed.
func TestSymlinkEscape(t *testing.T) {
	s := newServer(t)
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(s.root, "link")); err != nil {
		t.Fatal(err)
	}
	c, err := loopback(t).Open(context.Background(), s.config(t, ""), s.creds())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if _, err := c.Stat(context.Background(), plugin.Ref{Key: "link"}); err == nil || plugin.ClassOf(err) != plugin.Permanent {
		t.Errorf("a link out of the root: %v, want refused", err)
	}
	if r, err := c.ReadRange(context.Background(), plugin.Ref{Key: "link"}, 0, 6, ""); err == nil {
		_ = r.Close()
		t.Error("a link out of the root was read")
	}
}

// TestChangedDuringRead: SRC-T4 and SRC-W3, a change between the Stat and
// the end of a read is SourceChanged.
func TestChangedDuringRead(t *testing.T) {
	s := newServer(t)
	path := filepath.Join(s.root, "a")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), 1<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := loopback(t).Open(context.Background(), s.config(t, ""), s.creds())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	ref := plugin.Ref{Key: "a"}
	info, err := c.Stat(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	r, err := c.ReadRange(context.Background(), ref, 0, 1<<20, info.Version)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	if _, err := io.ReadFull(r, make([]byte, 10)); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
	_, err = io.Copy(io.Discard, r)
	if plugin.ClassOf(err) != plugin.SourceChanged {
		t.Errorf("a change during the read: %v, want SourceChanged", err)
	}
}

// TestGuard: SRC-3, the loopback without allow_cidrs is refused.
func TestGuard(t *testing.T) {
	s := newServer(t)
	d, err := Factory(json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	c, err := d.Open(context.Background(), s.config(t, ""), s.creds())
	if err == nil {
		defer func() { _ = c.Close() }()
		_, err = c.Stat(context.Background(), plugin.Ref{Key: "x"})
	}
	if !errors.Is(err, netguard.ErrForbidden) {
		t.Errorf("the loopback: %v, want ErrForbidden", err)
	}
}

func TestFactory(t *testing.T) {
	if d, err := Factory(json.RawMessage(`{"allow_cidrs": ["10.0.0.0/8"]}`)); err != nil || d.Name() != "sftp" {
		t.Errorf("%v %v", d, err)
	}
	for _, bad := range []string{`{"allow_cidrs": ["x"]}`, `{"roots": []}`} {
		if _, err := Factory(json.RawMessage(bad)); err == nil {
			t.Errorf("%s: accepted", bad)
		}
	}
}

// TestOpen: the configurations refused.
func TestOpen(t *testing.T) {
	s := newServer(t)
	d := loopback(t)
	host, port, _ := net.SplitHostPort(s.addr)
	for _, cfg := range []string{
		`{}`, `{"host": "", "root": "/x", "host_key": "` + s.hostKey + `"}`,
		`{"host": "` + host + `", "root": "relative", "host_key": "` + s.hostKey + `"}`,
		`{"host": "` + host + `", "root": "/x", "host_key": "not a key"}`,
		`{"host": "` + host + `", "port": 70000, "root": "/x", "host_key": "` + s.hostKey + `"}`,
		`{"host": "` + host + `", "port": ` + port + `, "root": "/x", "host_key": "` + s.hostKey + `", "bogus": 1}`,
	} {
		if c, err := d.Open(context.Background(), plugin.Config(cfg), s.creds()); err == nil {
			_ = c.Close()
			t.Errorf("%s: accepted", cfg)
		}
	}
}

func TestParseRef(t *testing.T) {
	for _, u := range []string{"sftp://files.example/a/b.img", "scp://files.example/a/b.img", "sftp://user@files.example:2222/a/b.img"} {
		if ref, err := (Driver{}).ParseRef(u); err != nil || ref.Key != "a/b.img" {
			t.Errorf("%s: %+v %v", u, ref, err)
		}
	}
	for _, bad := range []string{"http://x/a", "sftp://x/", "sftp://x/a/../b"} {
		if _, err := (Driver{}).ParseRef(bad); err == nil {
			t.Errorf("%s: accepted", bad)
		}
	}
}

// FuzzKey: SRC-T9, a key accepted joins to a path under the root.
func FuzzKey(f *testing.F) {
	for _, k := range []string{"a", "a/b", "../x", "a/../../x", "/etc", ".veho-staging/x", "a//b", "."} {
		f.Add(k)
	}
	f.Fuzz(func(t *testing.T, key string) {
		p, err := join("/srv/images", key)
		if err != nil {
			return
		}
		if !strings.HasPrefix(p, "/srv/images/") || strings.Contains(p, "/../") || p == "/srv/images/"+staging || strings.HasPrefix(p, "/srv/images/"+staging+"/") {
			t.Fatalf("%q: %q", key, p)
		}
	})
}

var _ = strconv.Itoa
