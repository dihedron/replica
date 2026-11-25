package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dihedron/veho/internal/config"
	"github.com/dihedron/veho/pkg/metadata"
	"github.com/dihedron/veho/pkg/plugin/process"
)

// TestLoadDrivers: the drivers the configuration lists, built in, pinned
// by path and SHA-256, or installed with a manifest, and each failure
// (veho-plugin-distribution.md §2, §3).
func TestLoadDrivers(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "veho-plugin-file")
	if out, err := exec.Command("go", "build", "-o", bin, "../veho-plugin-file").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	data, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	pinned := hex.EncodeToString(sum[:])
	// the file plugin installed as acme, signed with acme's key; another
	// publisher's key, other, is installed too, and no veho.pub
	plugins, keyDir := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(plugins, process.Binary("acme")), data, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"acme", "other"} {
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(keyDir, k+".pub"), []byte(base64.StdEncoding.EncodeToString(pub)+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if k == "acme" {
			if err := process.WriteManifest(plugins, priv, process.Manifest{Name: "acme", Version: "2.0.0", Veho: "[" + metadata.Version + ",)", SHA256: pinned, Protocol: process.Handshake.ProtocolVersion}); err != nil {
				t.Fatal(err)
			}
		}
	}
	pluginDir, pluginKeyDir, pluginsRootOwned = plugins, keyDir, false
	t.Cleanup(func() { pluginDir, pluginKeyDir, pluginsRootOwned = process.Dir, process.KeyDir, true })

	cases := []struct {
		name    string
		drivers []config.Driver
		want    []string // driver names loaded
		err     string   // part of the error, when it fails
	}{
		{"built in", []config.Driver{{Name: "file"}}, []string{"file"}, ""},
		{"built in, http", []config.Driver{{Name: "http", Settings: map[string]any{"allow_cidrs": []any{"10.0.0.0/8"}}}}, []string{"http"}, ""},
		{"built in, unknown setting", []config.Driver{{Name: "file", Settings: map[string]any{"nope": 1}}}, nil, "unknown field"},
		{"pinned", []config.Driver{{Name: "file", Path: bin, SHA256: pinned}}, []string{"file"}, ""},
		{"pinned, unknown setting", []config.Driver{{Name: "file", Path: bin, SHA256: pinned, Settings: map[string]any{"nope": 1}}}, nil, "settings refused"},
		{"pinned, wrong checksum", []config.Driver{{Name: "file", Path: bin, SHA256: strings.Repeat("0", 64)}}, nil, "driver file"},
		{"pinned, another name", []config.Driver{{Name: "acme", Path: bin, SHA256: pinned}}, nil, `the plugin serves "file"`},
		{"built in, with a key", []config.Driver{{Name: "file", Key: "acme"}}, nil, "takes no key"},
		{"installed, no manifest", []config.Driver{{Name: "nothere", Key: "acme"}}, nil, "manifest"},
		// acme's key verifies acme's manifest, so the plugin starts; it
		// serves the file driver, which the name check then refuses
		{"installed, its publisher's key", []config.Driver{{Name: "acme", Key: "acme"}}, nil, `the plugin serves "file"`},
		{"installed, another publisher's key", []config.Driver{{Name: "acme", Key: "other"}}, nil, "signature"},
		{"installed, the release key missing", []config.Driver{{Name: "acme"}}, nil, "plugin key veho"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			drivers, stop, err := loadDrivers(&config.Config{Drivers: c.drivers})
			defer stop()
			if c.err != "" {
				if err == nil || !strings.Contains(err.Error(), c.err) {
					t.Fatalf("error %v, want %q", err, c.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(drivers) != len(c.want) {
				t.Fatalf("drivers %v, want %v", drivers, c.want)
			}
			for _, n := range c.want {
				if d, ok := drivers[n]; !ok || d.Name() != n {
					t.Errorf("driver %s: %v", n, d)
				}
			}
		})
	}
}
