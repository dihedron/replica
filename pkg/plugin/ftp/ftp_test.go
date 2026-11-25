package ftp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dihedron/veho/internal/netguard"
	"github.com/dihedron/veho/pkg/plugin"
	"github.com/dihedron/veho/pkg/plugin/conformance"
)

// server is a small FTP server on the loopback, plain, serving a directory:
// USER, PASS, TYPE, FEAT, OPTS, SIZE, MDTM, EPSV, PASV, REST, RETR, QUIT.
type server struct {
	addr string
	dir  string
	// pasvHost, when set, is the address PASV announces (a bounce)
	pasvHost string
	noEPSV   bool
	mu       sync.Mutex
	dialled  []string // the data connections' peers
}

func newServer(t *testing.T) *server {
	t.Helper()
	s := &server{dir: t.TempDir()}
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
			go s.serve(c)
		}
	}()
	return s
}

func (s *server) serve(c net.Conn) {
	defer func() { _ = c.Close() }()
	r := bufio.NewReader(c)
	reply := func(format string, args ...any) { _, _ = fmt.Fprintf(c, format+"\r\n", args...) }
	reply("220 ready")
	var (
		data   net.Listener
		offset int64
		user   string
	)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd, arg, _ := strings.Cut(strings.TrimRight(line, "\r\n"), " ")
		file := filepath.Join(s.dir, filepath.FromSlash(strings.TrimPrefix(arg, "/")))
		switch strings.ToUpper(cmd) {
		case "USER":
			user = arg
			reply("331 password")
		case "PASS":
			if user == "veho" && arg == "secret" || user == "anonymous" {
				reply("230 in")
			} else {
				reply("530 no")
			}
		case "TYPE", "OPTS":
			reply("200 ok")
		case "FEAT":
			reply("211-features\r\n SIZE\r\n MDTM\r\n REST STREAM\r\n211 end")
		case "SIZE":
			if fi, err := os.Stat(file); err == nil {
				reply("213 %d", fi.Size())
			} else {
				reply("550 no such file")
			}
		case "MDTM":
			if fi, err := os.Stat(file); err == nil {
				reply("213 %s", fi.ModTime().UTC().Format("20060102150405"))
			} else {
				reply("550 no such file")
			}
		case "EPSV", "PASV":
			if strings.ToUpper(cmd) == "EPSV" && s.noEPSV {
				reply("500 no")
				continue
			}
			if data, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
				reply("425 no")
				continue
			}
			port := data.Addr().(*net.TCPAddr).Port
			if strings.ToUpper(cmd) == "EPSV" {
				reply("229 Entering Extended Passive Mode (|||%d|)", port)
			} else {
				h := "127,0,0,1"
				if s.pasvHost != "" {
					h = strings.ReplaceAll(s.pasvHost, ".", ",")
				}
				reply("227 Entering Passive Mode (%s,%d,%d)", h, port/256, port%256)
			}
		case "REST":
			offset, _ = strconv.ParseInt(arg, 10, 64)
			reply("350 ok")
		case "RETR":
			f, err := os.Open(file)
			if err != nil || data == nil {
				reply("550 no")
				continue
			}
			reply("150 sending")
			dc, err := data.Accept()
			_ = data.Close()
			data = nil
			if err != nil {
				_ = f.Close()
				continue
			}
			s.mu.Lock()
			s.dialled = append(s.dialled, dc.RemoteAddr().String())
			s.mu.Unlock()
			_, _ = f.Seek(offset, io.SeekStart)
			offset = 0
			_, err = io.Copy(dc, f)
			_ = f.Close()
			_ = dc.Close()
			if err != nil {
				reply("426 aborted")
			} else {
				reply("226 done")
			}
		case "ABOR":
			reply("226 aborted")
		case "QUIT":
			reply("221 bye")
			return
		default:
			reply("502 %s", cmd)
		}
	}
}

func (s *server) put(t *testing.T, ref plugin.Ref, data []byte) {
	p := filepath.Join(s.dir, "pub", filepath.FromSlash(ref.Key))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	// MDTM has a precision of one second (R-15): a new version moves the
	// time by one
	var mod time.Time
	if fi, err := os.Stat(p); err == nil {
		mod = fi.ModTime().Add(time.Second)
	} else {
		mod = time.Now().Truncate(time.Second)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mod, mod); err != nil {
		t.Fatal(err)
	}
}

func (s *server) config(extra string) plugin.Config {
	host, port, _ := net.SplitHostPort(s.addr)
	return plugin.Config(`{"host": "` + host + `", "port": ` + port + `, "root": "/pub", "tls": "none"` + extra + `}`)
}

var good = plugin.Credentials{"username": "veho", "password": "secret"}

// loopback is the driver with the loopback allowed and plain FTP too.
func loopback(t *testing.T) plugin.Driver {
	t.Helper()
	d, err := Factory(json.RawMessage(`{"allow_cidrs": ["127.0.0.0/8"], "plaintext": true}`))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// TestConformance: SRC-T7, the suite as a source, confined.
func TestConformance(t *testing.T) {
	s := newServer(t)
	conformance.Run(t, conformance.Target{Driver: loopback(t), Config: s.config(""), Credentials: good,
		BadCredentials: plugin.Credentials{"username": "veho", "password": "wrong"}, Put: s.put, Confined: true, PartSize: 64 << 10})
}

// TestPlaintext: SRC-T5, tls: none only when the settings allow it.
func TestPlaintext(t *testing.T) {
	s := newServer(t)
	d, err := Factory(json.RawMessage(`{"allow_cidrs": ["127.0.0.0/8"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if c, err := d.Open(context.Background(), s.config(""), good); err == nil {
		_ = c.Close()
		t.Error("tls: none without plaintext: accepted")
	}
}

// TestBounce: SRC-W1, a PASV answer naming another address is not
// followed: the data connection goes to the control connection's address.
func TestBounce(t *testing.T) {
	s := newServer(t)
	s.noEPSV = true
	s.pasvHost = "10.9.9.9"
	s.put(t, plugin.Ref{Key: "a"}, []byte("0123456789"))
	c, err := loopback(t).Open(context.Background(), s.config(""), good)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	r, err := c.ReadRange(context.Background(), plugin.Ref{Key: "a"}, 2, 4, "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil || string(b) != "2345" {
		t.Errorf("read %q %v", b, err)
	}
}

// TestPinned: SRC-W1, a data connection to any address but the control
// connection's is refused by the dialer.
func TestPinned(t *testing.T) {
	g, _ := netguard.New([]string{"0.0.0.0/0"})
	d := pinned(g, time.Second)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(l.Addr().String())
	c, err := d.DialContext(context.Background(), "tcp", "127.0.0.1:"+port)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	if _, err := d.DialContext(context.Background(), "tcp", "127.0.0.2:"+port); !errors.Is(err, netguard.ErrForbidden) {
		t.Errorf("another address: %v, want ErrForbidden", err)
	}
}

// TestGuard: SRC-3, the loopback without allow_cidrs is refused.
func TestGuard(t *testing.T) {
	s := newServer(t)
	d, err := Factory(json.RawMessage(`{"plaintext": true}`))
	if err != nil {
		t.Fatal(err)
	}
	c, err := d.Open(context.Background(), s.config(""), good)
	if err == nil {
		defer func() { _ = c.Close() }()
		_, err = c.Stat(context.Background(), plugin.Ref{Key: "x"})
	}
	if !errors.Is(err, netguard.ErrForbidden) {
		t.Errorf("the loopback: %v, want ErrForbidden", err)
	}
}

// TestAnonymous: no credentials is anonymous FTP.
func TestAnonymous(t *testing.T) {
	s := newServer(t)
	s.put(t, plugin.Ref{Key: "a"}, []byte("x"))
	c, err := loopback(t).Open(context.Background(), s.config(""), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if info, err := c.Stat(context.Background(), plugin.Ref{Key: "a"}); err != nil || info.Size != 1 {
		t.Errorf("%+v %v", info, err)
	}
}

func TestOpen(t *testing.T) {
	s := newServer(t)
	d := loopback(t)
	for _, cfg := range []string{`{}`, `{"host": "x", "root": "rel", "tls": "explicit"}`, `{"host": "x", "root": "/a", "tls": "maybe"}`,
		`{"host": "x", "root": "/a", "port": 0, "tls": "explicit", "bogus": 1}`, `{"host": "", "root": "/a"}`} {
		if c, err := d.Open(context.Background(), plugin.Config(cfg), good); err == nil {
			_ = c.Close()
			t.Errorf("%s: accepted", cfg)
		}
	}
	for _, creds := range []plugin.Credentials{{"username": "u"}, {"password": "p"}, {"username": "u", "password": "p", "x": "y"}} {
		if c, err := d.Open(context.Background(), s.config(""), creds); err == nil {
			_ = c.Close()
			t.Errorf("credentials %v: accepted", creds)
		}
	}
}

func TestFactory(t *testing.T) {
	for _, bad := range []string{`{"allow_cidrs": ["x"]}`, `{"roots": []}`} {
		if _, err := Factory(json.RawMessage(bad)); err == nil {
			t.Errorf("%s: accepted", bad)
		}
	}
}

func TestParseRef(t *testing.T) {
	for _, u := range []string{"ftp://files.example/a/b.img", "ftps://files.example:990/a/b.img"} {
		if ref, err := (Driver{}).ParseRef(u); err != nil || ref.Key != "a/b.img" {
			t.Errorf("%s: %+v %v", u, ref, err)
		}
	}
	for _, bad := range []string{"http://x/a", "ftp://x/", "ftp://x/a/../b"} {
		if _, err := (Driver{}).ParseRef(bad); err == nil {
			t.Errorf("%s: accepted", bad)
		}
	}
}

// FuzzKey: SRC-T9, a key accepted joins to a path under the root, with no
// character that would end the FTP command.
func FuzzKey(f *testing.F) {
	for _, k := range []string{"a", "a/b", "../x", "/etc", "a\r\nDELE x", ".", "a//b"} {
		f.Add(k)
	}
	f.Fuzz(func(t *testing.T, key string) {
		p, err := join("/pub", key)
		if err != nil {
			return
		}
		if !strings.HasPrefix(p, "/pub/") || strings.Contains(p, "/../") || strings.ContainsAny(p, "\r\n\x00") {
			t.Fatalf("%q: %q", key, p)
		}
	})
}
