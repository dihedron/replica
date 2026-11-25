package file

import (
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/dihedron/veho/pkg/plugin"
	"github.com/dihedron/veho/pkg/plugin/conformance"
)

func TestConformance(t *testing.T) {
	cfg, _ := json.Marshal(config{Root: t.TempDir()})
	conformance.Run(t, conformance.Target{Driver: New(), Config: cfg, PartSize: 64 << 10})
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
