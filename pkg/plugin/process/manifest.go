package process

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/dihedron/veho/pkg/semver"
)

// Dir is where the plugin packages install their binaries and manifests
// (veho-plugin-distribution.md §5).
const Dir = "/usr/libexec/veho/plugins"

// KeyDir holds the publishers' public keys, <name>.pub, the release's as
// veho.pub (§3.2, §11).
const KeyDir = "/etc/veho/plugin-keys"

// ReleaseKey is the key of the plugins of veho's release, the default of a
// driver that names none.
const ReleaseKey = "veho"

// Manifest vouches for a plugin binary of a release (§3.1).
type Manifest struct {
	Name     string `json:"name"`
	Version  string `json:"version"` // the plugin's own
	Veho     string `json:"veho"`    // the versions of veho it supports, a semver range
	SHA256   string `json:"sha256"`
	Protocol uint   `json:"protocol"`
}

// Binary is the file name of a plugin's binary; its manifest and signature
// are next to it, with .manifest and .manifest.sig.
func Binary(name string) string { return "veho-plugin-" + name }

// WriteManifest writes m and its Ed25519 signature into dir.
func WriteManifest(dir string, key ed25519.PrivateKey, m Manifest) error {
	data, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("plugin %s: manifest: %w", m.Name, err)
	}
	data = append(data, '\n')
	base := filepath.Join(dir, Binary(m.Name)+".manifest")
	if err := os.WriteFile(base, data, 0o644); err != nil { //nolint:gosec // public, as the binary
		return fmt.Errorf("plugin %s: manifest: %w", m.Name, err)
	}
	sig := base64.StdEncoding.EncodeToString(ed25519.Sign(key, data)) + "\n"
	if err := os.WriteFile(base+".sig", []byte(sig), 0o644); err != nil { //nolint:gosec // public
		return fmt.Errorf("plugin %s: manifest: %w", m.Name, err)
	}
	return nil
}

// ParseKeys reads public keys, one base64 Ed25519 key a line; blank lines
// and lines starting with # are skipped.
func ParseKeys(data []byte) ([]ed25519.PublicKey, error) {
	var keys []ed25519.PublicKey
	sc := bufio.NewScanner(bytes.NewReader(data))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, err := base64.StdEncoding.DecodeString(line)
		if err != nil || len(k) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("plugin keys: line %d is not a base64 Ed25519 public key", n)
		}
		keys = append(keys, ed25519.PublicKey(k))
	}
	return keys, sc.Err()
}

// LoadKeys reads the public keys of the key file name.pub in dir, which
// must hold one at least; with requireRootOwned, the directory and the file
// must be root's and not writable by others (§3.2, step 2).
func LoadKeys(dir, name string, requireRootOwned bool) ([]ed25519.PublicKey, error) {
	path := filepath.Join(dir, name+".pub")
	if requireRootOwned {
		for _, p := range []string{dir, path} {
			if err := rootOwned(p); err != nil {
				return nil, fmt.Errorf("plugin key %s: %w", name, err)
			}
		}
	}
	data, err := os.ReadFile(path) //nolint:gosec // the key directory
	if err != nil {
		return nil, fmt.Errorf("plugin key %s: %w", name, err)
	}
	keys, err := ParseKeys(data)
	if err != nil {
		return nil, fmt.Errorf("plugin key %s: %w", name, err)
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("plugin key %s: %s holds no key", name, path)
	}
	return keys, nil
}

// Installed checks the manifest of the plugin name installed in dir against
// keys and veho's version (§3.2, steps 1 to 3) and returns the Options to
// start it, whose SHA-256 go-plugin checks before the start (step 4).
func Installed(dir, name, version string, keys []ed25519.PublicKey, requireRootOwned bool) (Options, error) {
	fail := func(step string, err error) (Options, error) {
		return Options{}, fmt.Errorf("plugin %s: %s: %w", name, step, err)
	}
	bin := filepath.Join(dir, Binary(name))
	if requireRootOwned {
		for _, p := range []string{dir, bin + ".manifest", bin + ".manifest.sig"} {
			if err := rootOwned(p); err != nil {
				return fail("manifest", err)
			}
		}
	}
	data, err := os.ReadFile(bin + ".manifest") //nolint:gosec // the plugin directory
	if err != nil {
		return fail("manifest", err)
	}
	sig, err := os.ReadFile(bin + ".manifest.sig") //nolint:gosec // the plugin directory
	if err != nil {
		return fail("manifest", err)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sig)))
	if err != nil || !slices.ContainsFunc(keys, func(k ed25519.PublicKey) bool { return ed25519.Verify(k, data, raw) }) {
		return fail("signature", fmt.Errorf("the manifest is not signed by the driver's key"))
	}
	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return fail("manifest", err)
	}
	if m.Name != name {
		return fail("name", fmt.Errorf("the manifest is of %q", m.Name))
	}
	supported, err := semver.ParseRange(m.Veho)
	if err != nil {
		return fail("version", err)
	}
	v, err := semver.Parse(version)
	if err != nil {
		return fail("version", fmt.Errorf("veho's own: %w", err))
	}
	if !supported.Contains(v) {
		return fail("version", fmt.Errorf("the plugin %s supports veho %s, not %s: install a plugin for this version", m.Version, m.Veho, version))
	}
	sum, err := hex.DecodeString(m.SHA256)
	switch {
	case m.Protocol != Handshake.ProtocolVersion:
		return fail("protocol", fmt.Errorf("the plugin speaks protocol %d, veho %d", m.Protocol, Handshake.ProtocolVersion))
	case err != nil || len(sum) != 32:
		return fail("manifest", fmt.Errorf("sha256 %q: want 64 hexadecimal digits", m.SHA256))
	}
	return Options{Path: bin, SHA256: sum, RequireRootOwned: requireRootOwned}, nil
}
