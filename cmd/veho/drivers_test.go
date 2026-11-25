package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dihedron/veho/internal/config"
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
	pluginsRootOwned = false
	t.Cleanup(func() { pluginsRootOwned = true })

	cases := []struct {
		name    string
		drivers []config.Driver
		want    []string // driver names loaded
		err     string   // part of the error, when it fails
	}{
		{"built in", []config.Driver{{Name: "file"}}, []string{"file"}, ""},
		{"built in, unknown setting", []config.Driver{{Name: "file", Settings: map[string]any{"nope": 1}}}, nil, "unknown field"},
		{"pinned", []config.Driver{{Name: "file", Path: bin, SHA256: pinned}}, []string{"file"}, ""},
		{"pinned, unknown setting", []config.Driver{{Name: "file", Path: bin, SHA256: pinned, Settings: map[string]any{"nope": 1}}}, nil, "settings refused"},
		{"pinned, wrong checksum", []config.Driver{{Name: "file", Path: bin, SHA256: strings.Repeat("0", 64)}}, nil, "driver file"},
		{"pinned, another name", []config.Driver{{Name: "acme", Path: bin, SHA256: pinned}}, nil, `the plugin serves "file"`},
		{"installed, no manifest", []config.Driver{{Name: "nothere"}}, nil, "manifest"},
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
