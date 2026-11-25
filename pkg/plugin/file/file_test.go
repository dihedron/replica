package file

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/dihedron/veho/pkg/plugin"
	"github.com/dihedron/veho/pkg/plugin/conformance"
)

func TestConformance(t *testing.T) {
	cfg, _ := json.Marshal(config{Root: t.TempDir()})
	conformance.Run(t, conformance.Target{Driver: New(), Config: cfg, PartSize: 64 << 10, Confined: true})
}

// TestKeys: no key escapes the root or reaches the staging directory.
func TestKeys(t *testing.T) {
	for _, key := range []string{"", "/etc/passwd", "../x", "a/../../x", "a//b", "./a", "..", ".veho-staging", ".veho-staging/x"} {
		if err := checkKey(key); plugin.ClassOf(err) != plugin.Permanent {
			t.Errorf("key %q: %v", key, err)
		}
	}
	for _, key := range []string{"a", "a/b/c.img", ".hidden", "x..y"} {
		if err := checkKey(key); err != nil {
			t.Errorf("key %q: %v", key, err)
		}
	}
	if _, err := (Driver{}).ParseRef("s3://b/k"); err == nil {
		t.Error("ParseRef of an s3 URL")
	}
	if ref, err := (Driver{}).ParseRef("file:///a/b"); err != nil || ref.Key != "a/b" {
		t.Errorf("ParseRef = %+v, %v", ref, err)
	}
}

// TestSymlinkEscape: a symbolic link out of the root is not followed.
func TestSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	if err := os.Symlink(t.TempDir(), root+"/out"); err != nil {
		t.Skip(err)
	}
	cfg, _ := json.Marshal(config{Root: root})
	c, err := New().Open(t.Context(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	up, err := c.Begin(t.Context(), plugin.Ref{Key: "out/x"}, plugin.ObjectInfo{}, plugin.PutOptions{})
	if err == nil {
		_, err = up.Commit(t.Context(), nil)
	}
	if err == nil || errors.Is(err, plugin.ErrNotFound) {
		t.Errorf("a write through a link out of the root: %v", err)
	}
}

// TestFactory: the driver has no settings yet, so any key is refused
// (veho-plugin-distribution.md §4).
func TestFactory(t *testing.T) {
	d, err := Factory(json.RawMessage("{}"))
	if err != nil || d.Name() != "file" {
		t.Fatalf("factory: %v %v", d, err)
	}
	if _, err := Factory(json.RawMessage(`{"nope": 1}`)); err == nil {
		t.Error("an unknown setting was accepted")
	}
}

// TestRoots: SRC-T6 and SRC-W7, with the driver's roots setting an
// endpoint's root must be one of them or under one; empty, no endpoint;
// New, for tests and libraries, has no restriction.
func TestRoots(t *testing.T) {
	base := t.TempDir()
	images := filepath.Join(base, "images")
	if err := os.MkdirAll(filepath.Join(images, "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc", filepath.Join(images, "etc")); err != nil {
		t.Fatal(err)
	}
	cfg := func(root string) plugin.Config { b, _ := json.Marshal(config{Root: root}); return b }
	d, err := Factory(json.RawMessage(`{"roots": ["` + images + `"]}`))
	if err != nil {
		t.Fatal(err)
	}
	cc := d.(plugin.ConfigChecker)
	for _, ok := range []string{images, filepath.Join(images, "a")} {
		if err := cc.CheckConfig(context.Background(), cfg(ok)); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
		c, err := d.Open(context.Background(), cfg(ok), nil)
		if err != nil {
			t.Errorf("Open %s: %v", ok, err)
		} else {
			_ = c.Close()
		}
	}
	for _, bad := range []string{base, "/etc", images + "-other", filepath.Join(images, "..", "x"), "relative"} {
		if err := cc.CheckConfig(context.Background(), cfg(bad)); err == nil || plugin.ClassOf(err) != plugin.Permanent {
			t.Errorf("CheckConfig %s: %v, want refused", bad, err)
		}
	}
	// a link under a root that leads out of it: refused when opened
	if c, err := d.Open(context.Background(), cfg(filepath.Join(images, "etc")), nil); err == nil {
		_ = c.Close()
		t.Error("a root that is a link out of the roots was opened")
	}
	none, err := Factory(json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := none.(plugin.ConfigChecker).CheckConfig(context.Background(), cfg(images)); err == nil {
		t.Error("no roots: an endpoint was accepted")
	}
	if err := New().CheckConfig(context.Background(), cfg(images)); err != nil {
		t.Errorf("New: %v", err)
	}
	for _, bad := range []string{`{"roots": ["relative"]}`, `{"roots": ["/a/../b"]}`, `{"nope": 1}`} {
		if _, err := Factory(json.RawMessage(bad)); err == nil {
			t.Errorf("%s: accepted", bad)
		}
	}
}
