package process

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// installed lays out dir as a plugin package does: the file plugin's
// binary as veho-plugin-<name>, its manifest and signature, signed by key.
func installed(t *testing.T, key ed25519.PrivateKey, m Manifest) string {
	t.Helper()
	dir := t.TempDir()
	data, err := os.ReadFile(binaries["file"].path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "veho-plugin-"+m.Name), data, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteManifest(dir, key, m); err != nil {
		t.Fatal(err)
	}
	return dir
}

func newKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

// TestManifest: PD-T1 and PD-T5, veho-plugin-distribution.md §3.2.
func TestManifest(t *testing.T) {
	pub, priv := newKey(t)
	other, otherPriv := newKey(t)
	sum := hex.EncodeToString(binaries["file"].sum)
	good := Manifest{Name: "file", Version: "1.2.3", SHA256: sum, Protocol: Handshake.ProtocolVersion}

	dir := installed(t, priv, good)
	opts, err := Installed(dir, "file", "1.2.3", []ed25519.PublicKey{other, pub}, false)
	if err != nil {
		t.Fatalf("a valid manifest: %v", err)
	}
	p, err := Load(opts)
	if err != nil {
		t.Fatalf("Load from the manifest: %v", err)
	}
	p.Kill()

	for _, c := range []struct {
		name, want string
		dir        func() string
		version    string
		keys       []ed25519.PublicKey
	}{
		{"unknown key", "signature", func() string { return installed(t, otherPriv, good) }, "1.2.3", []ed25519.PublicKey{pub}},
		{"no key", "signature", func() string { return dir }, "1.2.3", nil},
		{"another version", "version", func() string { return dir }, "1.2.4", []ed25519.PublicKey{pub}},
		{"another name", "name", func() string {
			d := installed(t, priv, Manifest{Name: "s3", Version: "1.2.3", SHA256: sum, Protocol: Handshake.ProtocolVersion})
			_ = os.Rename(filepath.Join(d, "veho-plugin-s3.manifest"), filepath.Join(d, "veho-plugin-file.manifest"))
			_ = os.Rename(filepath.Join(d, "veho-plugin-s3.manifest.sig"), filepath.Join(d, "veho-plugin-file.manifest.sig"))
			return d
		}, "1.2.3", []ed25519.PublicKey{pub}},
		{"another protocol", "protocol", func() string {
			return installed(t, priv, Manifest{Name: "file", Version: "1.2.3", SHA256: sum, Protocol: Handshake.ProtocolVersion + 1})
		}, "1.2.3", []ed25519.PublicKey{pub}},
		{"tampered manifest", "signature", func() string {
			d := installed(t, priv, good)
			path := filepath.Join(d, "veho-plugin-file.manifest")
			data, _ := os.ReadFile(path)
			_ = os.WriteFile(path, []byte(strings.Replace(string(data), "1.2.3", "1.2.4", 1)), 0o644)
			return d
		}, "1.2.4", []ed25519.PublicKey{pub}},
		{"no manifest", "manifest", func() string {
			d := installed(t, priv, good)
			_ = os.Remove(filepath.Join(d, "veho-plugin-file.manifest"))
			return d
		}, "1.2.3", []ed25519.PublicKey{pub}},
	} {
		if _, err := Installed(c.dir(), "file", c.version, c.keys, false); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want an error about the %s", c.name, err, c.want)
		}
	}

	// another binary than the manifest's: refused by the checksum at start
	d := installed(t, priv, Manifest{Name: "file", Version: "1.2.3", SHA256: strings.Repeat("0", 64), Protocol: Handshake.ProtocolVersion})
	opts, err = Installed(d, "file", "1.2.3", []ed25519.PublicKey{pub}, false)
	if err != nil {
		t.Fatal(err)
	}
	if p, err := Load(opts); err == nil {
		p.Kill()
		t.Error("a binary other than the manifest's started")
	}

	// root ownership is checked on the directory and the files
	if os.Getuid() != 0 {
		if _, err := Installed(dir, "file", "1.2.3", []ed25519.PublicKey{pub}, true); err == nil || !strings.Contains(err.Error(), "root") {
			t.Errorf("a user's directory: %v, want refused", err)
		}
	}
}

// TestParseKeys: one base64 Ed25519 key a line, comments and blank lines
// skipped, anything else refused.
func TestParseKeys(t *testing.T) {
	pub, _ := newKey(t)
	keys, err := ParseKeys([]byte("# veho plugin signing keys\n\n" + base64.StdEncoding.EncodeToString(pub) + "\n"))
	if err != nil || len(keys) != 1 || !keys[0].Equal(pub) {
		t.Fatalf("%v %v", keys, err)
	}
	for _, bad := range []string{"not base64!\n", base64.StdEncoding.EncodeToString([]byte("short")) + "\n"} {
		if _, err := ParseKeys([]byte(bad)); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// TestSettings: PD-T3, a plugin builds its driver from the settings it is
// started with; one that refuses them fails Load with its message.
func TestSettings(t *testing.T) {
	p, err := Load(Options{Path: binaries["file"].path, SHA256: binaries["file"].sum, Settings: []byte(`{}`)})
	if err != nil {
		t.Fatalf("empty settings: %v", err)
	}
	p.Kill()
	_, err = Load(Options{Path: binaries["file"].path, SHA256: binaries["file"].sum, Settings: []byte(`{"bogus": 1}`)})
	if err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Errorf("settings refused by the plugin: %v, want its message", err)
	}
}
