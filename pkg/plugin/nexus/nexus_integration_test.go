//go:build integration

package nexus

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dihedron/veho/pkg/plugin"
	"github.com/dihedron/veho/pkg/plugin/conformance"
)

// lab is the lab's Nexus (lab.sh nexus up), or a skip.
type lab struct {
	d     plugin.Driver
	url   string
	ca    []byte
	creds plugin.Credentials
}

func labNexus(t *testing.T) lab {
	t.Helper()
	url := os.Getenv("VEHO_TEST_NEXUS_URL")
	if url == "" {
		t.Skip("no Nexus node: test/lab/lab.sh nexus up")
	}
	ca, err := os.ReadFile(os.Getenv("VEHO_TEST_NEXUS_CA"))
	if err != nil {
		t.Fatal(err)
	}
	settings, _ := json.Marshal(settings{AllowCIDRs: []string{os.Getenv("VEHO_TEST_NEXUS_ALLOW")}})
	d, err := Factory(settings)
	if err != nil {
		t.Fatal(err)
	}
	return lab{d: d, url: url, ca: ca, creds: plugin.Credentials{"username": os.Getenv("VEHO_TEST_NEXUS_USER"), "password": os.Getenv("VEHO_TEST_NEXUS_PASSWORD")}}
}

func (l lab) cfg(repo, format string) plugin.Config {
	b, _ := json.Marshal(config{URL: l.url, Repository: repo, Format: format, CACert: string(l.ca)})
	return b
}

func (l lab) open(t *testing.T, repo, format string) plugin.Client {
	t.Helper()
	c, err := l.d.Open(context.Background(), l.cfg(repo, format), l.creds)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// NX-T4: the conformance suite against the lab's Nexus, and its refused
// redeploy and anonymous reading.
func TestLabNexus(t *testing.T) {
	l := labNexus(t)
	prefix := fmt.Sprintf("conformance-%d/", time.Now().UnixNano())
	conformance.Run(t, conformance.Target{
		Driver: l.d, Config: l.cfg("veho-raw", "raw"), Credentials: l.creds, Prefix: prefix, PartSize: 1 << 20, Confined: true,
		BadCredentials: plugin.Credentials{"username": "veho", "password": "wrong"},
	})

	ctx := context.Background()
	once := l.open(t, "veho-raw-once", "")
	key := prefix + "once"
	for i, want := range []plugin.Class{plugin.Unknown, plugin.Permanent} {
		if _, err := put(t, once, key, []byte(fmt.Sprintf("version %d", i))); plugin.ClassOf(err) != want && !(want == plugin.Unknown && err == nil) {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	anon, err := l.d.Open(ctx, l.cfg("veho-raw-once", ""), nil)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := anon.Stat(ctx, plugin.Ref{Key: key}); err != nil || info.Size != 9 {
		t.Fatalf("anonymous read: %+v, %v", info, err)
	}
	_ = once.Delete(ctx, plugin.Ref{Key: key})
}

// NX-T6 in the lab: the suite through coordinates on veho-maven (a release
// repository that allows redeploying), and a snapshot refused by Nexus's
// own maven-releases.
func TestLabMaven(t *testing.T) {
	l := labNexus(t)
	run := time.Now().UnixNano()
	conformance.Run(t, conformance.Target{
		Driver: l.d, Config: l.cfg("veho-maven", "maven2"), Credentials: l.creds, PartSize: 1 << 20,
		BadCredentials: plugin.Credentials{"username": "veho", "password": "wrong"},
		Key:            func(k string) string { return fmt.Sprintf("org.veho.lab:conformance:%d-%s@bin", run, k) },
	})
	ctx := context.Background()
	c := l.open(t, "veho-maven", "maven2")
	key := fmt.Sprintf("org.veho.lab:app:1.%d@jar", run)
	if _, err := put(t, c, key, []byte("a jar")); err != nil {
		t.Fatal(err)
	}
	if info, err := c.Stat(ctx, plugin.Ref{Key: fmt.Sprintf("org/veho/lab/app/1.%d/app-1.%d.jar", run, run)}); err != nil || info.Size != 5 {
		t.Fatalf("by its path: %+v, %v", info, err)
	}
	if _, err := put(t, c, key+".sha1", []byte("x")); plugin.ClassOf(err) != plugin.Permanent {
		t.Fatalf("a checksum key: %v", err)
	}
	_ = c.Delete(ctx, plugin.Ref{Key: key})
	releases := l.open(t, "maven-releases", "maven2")
	if _, err := put(t, releases, fmt.Sprintf("org.veho.lab:app:1.%d-SNAPSHOT@jar", run), []byte("x")); plugin.ClassOf(err) != plugin.Permanent {
		t.Fatalf("a snapshot into maven-releases: %v", err)
	}
}

// NX-T7 in the lab: a generated package published, read back by its
// packument, refused again (veho-npm allows one deploy), deleted.
func TestLabNPM(t *testing.T) {
	l := labNexus(t)
	ctx := context.Background()
	c := l.open(t, "veho-npm", "npm")
	for _, name := range []string{"veho-lab", "@veho/lab"} {
		version := fmt.Sprintf("1.0.%d", time.Now().UnixNano())
		key := name + "@" + version
		data := pkg(t, name, version)
		info, err := put(t, c, key, data)
		if err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		got, err := c.Stat(ctx, plugin.Ref{Key: key})
		if err != nil || got.Size != int64(len(data)) || got.Version != info.Version {
			t.Fatalf("%s: stat %+v, %v", key, got, err)
		}
		readBack(t, c, key, data, got.Version)
		if _, err := put(t, c, key, data); plugin.ClassOf(err) != plugin.Permanent {
			t.Fatalf("%s again: %v", key, err)
		}
		if err := c.Delete(ctx, plugin.Ref{Key: key}); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Stat(ctx, plugin.Ref{Key: key}); !errors.Is(err, plugin.ErrNotFound) {
			t.Fatalf("%s after delete: %v", key, err)
		}
	}
}

// NX-T8 in the lab: a generated wheel and sdist uploaded, found in the
// simple index, read back, deleted.
func TestLabPyPI(t *testing.T) {
	l := labNexus(t)
	ctx := context.Background()
	c := l.open(t, "veho-pypi", "pypi")
	version := fmt.Sprintf("1.0.%d", time.Now().UnixNano())
	for key, data := range map[string][]byte{
		"veho_lab-" + version + "-py3-none-any.whl": wheel(t, "veho_lab", version),
		"veho_lab-" + version + ".tar.gz":           sdist(t, "veho_lab", version),
	} {
		info, err := put(t, c, key, data)
		if err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		got, err := c.Stat(ctx, plugin.Ref{Key: key})
		if err != nil || got.Size != int64(len(data)) || got.Version != info.Version {
			t.Fatalf("%s: stat %+v, %v", key, got, err)
		}
		readBack(t, c, key, data, got.Version)
		if err := c.Delete(ctx, plugin.Ref{Key: key}); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Stat(ctx, plugin.Ref{Key: key}); !errors.Is(err, plugin.ErrNotFound) {
			t.Fatalf("%s after delete: %v", key, err)
		}
	}
}

func readBack(t *testing.T, c plugin.Client, key string, data []byte, version string) {
	t.Helper()
	r, err := c.ReadRange(context.Background(), plugin.Ref{Key: key}, 0, int64(len(data)), version)
	if err != nil {
		t.Fatalf("%s: %v", key, err)
	}
	defer func() { _ = r.Close() }()
	if b, _ := io.ReadAll(r); !bytes.Equal(b, data) {
		t.Fatalf("%s: read back %d bytes, differing", key, len(b))
	}
}

func metadata(name, version string) string {
	return "Metadata-Version: 2.1\nName: " + name + "\nVersion: " + version + "\nSummary: veho lab\n"
}

// wheel is a minimal wheel: its METADATA, WHEEL and RECORD.
func wheel(t *testing.T, name, version string) []byte {
	t.Helper()
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	info := name + "-" + version + ".dist-info/"
	for file, body := range map[string]string{
		name + "/__init__.py": "",
		info + "METADATA":     metadata(name, version),
		info + "WHEEL":        "Wheel-Version: 1.0\nGenerator: veho\nRoot-Is-Purelib: true\nTag: py3-none-any\n",
		info + "RECORD":       "",
	} {
		w, err := z.Create(file)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(w, strings.NewReader(body))
	}
	_ = z.Close()
	return b.Bytes()
}

// sdist is a minimal sdist: its PKG-INFO.
func sdist(t *testing.T, name, version string) []byte {
	t.Helper()
	var b bytes.Buffer
	z := gzip.NewWriter(&b)
	w := tar.NewWriter(z)
	body := metadata(name, version)
	if err := w.WriteHeader(&tar.Header{Name: name + "-" + version + "/PKG-INFO", Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte(body))
	_ = w.Close()
	_ = z.Close()
	return b.Bytes()
}
