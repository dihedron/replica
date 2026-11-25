package process

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/dihedron/veho/pkg/plugin"
	"github.com/dihedron/veho/pkg/plugin/conformance"
	"github.com/dihedron/veho/pkg/plugin/file"
)

// binaries holds the plugin binaries built for the tests, and their SHA-256.
var binaries = map[string]struct {
	path string
	sum  []byte
}{}

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "veho-plugins-")
	if err != nil {
		panic(err)
	}
	for _, name := range []string{"file", "s3"} {
		path := filepath.Join(dir, "veho-plugin-"+name)
		cmd := exec.Command("go", "build", "-o", path, "../../../cmd/veho-plugin-"+name)
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			panic(err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			panic(err)
		}
		sum := sha256.Sum256(data)
		binaries[name] = struct {
			path string
			sum  []byte
		}{path, sum[:]}
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func load(t testing.TB, name string) *Process {
	t.Helper()
	// the file plugin's endpoints are the tests' temporary directories
	settings, _ := json.Marshal(map[string]any{"roots": []string{filepath.Clean(os.TempDir())}})
	if name != "file" {
		settings = nil
	}
	p, err := Load(Options{Path: binaries[name].path, SHA256: binaries[name].sum, Settings: settings})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Kill)
	return p
}

func fileConfig(t testing.TB) plugin.Config {
	cfg, _ := json.Marshal(map[string]string{"root": t.TempDir()})
	return cfg
}

// TestConformance: the file driver, out of process, passes the suite.
func TestConformance(t *testing.T) {
	conformance.Run(t, conformance.Target{Driver: load(t, "file").Driver(), Config: fileConfig(t), PartSize: 64 << 10})
}

// TestChecksum: PLG-14, a binary of another checksum is not started.
func TestChecksum(t *testing.T) {
	if _, err := Load(Options{Path: binaries["file"].path, SHA256: make([]byte, sha256.Size)}); err == nil {
		t.Error("a binary with the wrong checksum started")
	}
	if _, err := Load(Options{Path: binaries["file"].path}); err == nil {
		t.Error("a binary without a checksum started")
	}
}

// TestRootOwned: PLG-14, a binary that is not root's is refused.
func TestRootOwned(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root")
	}
	if _, err := Load(Options{Path: binaries["file"].path, SHA256: binaries["file"].sum, RequireRootOwned: true}); err == nil {
		t.Error("a binary owned by a user started")
	}
}

// TestRestart: PLG-18, a crashed plugin fails the call in flight as
// Transient, and the next Open starts it again.
func TestRestart(t *testing.T) {
	p := load(t, "file")
	ctx := context.Background()
	cfg := fileConfig(t)
	c, err := p.Driver().Open(ctx, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	pid := p.client.ReattachConfig().Pid
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); !p.client.Exited() && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := c.Stat(ctx, plugin.Ref{Key: "x"}); plugin.ClassOf(err) != plugin.Transient {
		t.Errorf("a call to a dead plugin: %v (%v), want Transient", err, plugin.ClassOf(err))
	}
	c, err = p.Driver().Open(ctx, cfg, nil)
	if err != nil {
		t.Fatalf("Open after the crash: %v", err)
	}
	defer func() { _ = c.Close() }()
	if _, err := c.Stat(ctx, plugin.Ref{Key: "x"}); plugin.ClassOf(err) != plugin.Permanent {
		t.Errorf("Stat after the restart: %v, want not found", err)
	}
}

// BenchmarkCopy is PLG-17's measurement: an object read from a source and
// written to a sink through the core, compiled in and out of process.
func BenchmarkCopy(b *testing.B) {
	const size, part = 64 << 20, 8 << 20
	for name, d := range map[string]func(testing.TB) plugin.Driver{
		"in-process": func(testing.TB) plugin.Driver { return file.New() },
		"process":    func(t testing.TB) plugin.Driver { return load(t, "file").Driver() },
	} {
		b.Run(name, func(b *testing.B) {
			ctx := context.Background()
			c, err := d(b).Open(ctx, fileConfig(b), nil)
			if err != nil {
				b.Fatal(err)
			}
			defer func() { _ = c.Close() }()
			src := plugin.Ref{Key: "source"}
			up, _ := c.Begin(ctx, src, plugin.ObjectInfo{}, plugin.PutOptions{})
			var parts []plugin.PartResult
			for i := range size / part {
				r, err := up.WritePart(ctx, i, bytes.NewReader(make([]byte, part)), part)
				if err != nil {
					b.Fatal(err)
				}
				parts = append(parts, r)
			}
			if _, err := up.Commit(ctx, parts); err != nil {
				b.Fatal(err)
			}
			b.SetBytes(size)
			for b.Loop() {
				up, err := c.Begin(ctx, plugin.Ref{Key: "copy"}, plugin.ObjectInfo{}, plugin.PutOptions{})
				if err != nil {
					b.Fatal(err)
				}
				parts = parts[:0]
				for i := range size / part {
					r, err := c.ReadRange(ctx, src, int64(i*part), part, "")
					if err != nil {
						b.Fatal(err)
					}
					h := sha256.New() // the core's digest (FR-8)
					res, err := up.WritePart(ctx, i, io.TeeReader(r, h), part)
					_ = r.Close()
					if err != nil {
						b.Fatal(err)
					}
					parts = append(parts, res)
				}
				if _, err := up.Commit(ctx, parts); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
