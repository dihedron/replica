package secrets

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"uuid"

	"github.com/dihedron/veho/internal/audit"
)

// memKeyring is a Keyring that never reloads.
func memKeyring(kek map[int][]byte) *Keyring { return &Keyring{kek: kek} }

func TestAADUnambiguous(t *testing.T) {
	if bytes.Equal(aad("x", []byte("ab"), []byte("c")), aad("x", []byte("a"), []byte("bc"))) {
		t.Error("two field lists encode alike")
	}
	if bytes.Equal(kekAAD(1), aad(labelDEK, u32(1))) {
		t.Error("the labels do not separate the layers")
	}
}

// SECR-1: a value opens with its own tenant, name, versions only, and a
// flipped bit in any layer fails.
func TestSealOpen(t *testing.T) {
	ctx := context.Background()
	unwrap := newKey()
	kek1, kek2 := newKey(), newKey()
	ring := memKeyring(map[int][]byte{1: kek1, 2: kek2})
	tenant, other := uuid.New(), uuid.New()
	value := []byte("canary-value")
	dek, ct, err := sealValue(ctx, ring, tenant, "s3", 3, 1, value)
	if err != nil {
		t.Fatal(err)
	}
	got, err := openValue(ctx, ring, tenant, "s3", 3, 1, dek, ct)
	if err != nil || !bytes.Equal(got, value) {
		t.Fatalf("round trip: %q, %v", got, err)
	}
	for _, c := range []struct {
		name              string
		tenant            uuid.UUID
		secret            string
		version, kekVersn int
	}{
		{"tenant", other, "s3", 3, 1},
		{"name", tenant, "s4", 3, 1},
		{"version", tenant, "s3", 4, 1},
		{"KEK version", tenant, "s3", 3, 2},
	} {
		if _, err := openValue(ctx, ring, c.tenant, c.secret, c.version, c.kekVersn, dek, ct); err == nil {
			t.Errorf("another %s: decrypts", c.name)
		}
	}
	flip := func(b []byte, i int) []byte { b = bytes.Clone(b); b[i%len(b)] ^= 1; return b }
	for i := range len(ct) {
		if _, err := openValue(ctx, ring, tenant, "s3", 3, 1, dek, flip(ct, i)); err == nil {
			t.Fatalf("ciphertext bit %d flipped: decrypts", i)
		}
	}
	for i := range len(dek) {
		if _, err := openValue(ctx, ring, tenant, "s3", 3, 1, flip(dek, i), ct); err == nil {
			t.Fatalf("DEK bit %d flipped: decrypts", i)
		}
	}
	wrapped, _ := seal(unwrap, kek1, kekAAD(1))
	if _, err := unwrapKEKs(unwrap, []kekRow{{1, flip(wrapped, 5), keyID(unwrap), "active"}}); err == nil {
		t.Error("KEK bit flipped: unwraps")
	}
	if _, err := unwrapKEKs(unwrap, []kekRow{{2, wrapped, keyID(unwrap), "active"}}); err == nil {
		t.Error("KEK relabelled as another version: unwraps")
	}
}

// SECR-7: the keyring needs one active KEK and the right unwrap key.
func TestUnwrapKEKs(t *testing.T) {
	key, wrong := newKey(), newKey()
	kek := newKey()
	wrapped, _ := seal(key, kek, kekAAD(1))
	got, err := unwrapKEKs(key, []kekRow{{1, wrapped, keyID(key), "active"}})
	if err != nil || !bytes.Equal(got[1], kek) {
		t.Fatalf("unwrap: %v", err)
	}
	_, err = unwrapKEKs(wrong, []kekRow{{1, wrapped, keyID(key), "active"}})
	if err == nil || !strings.Contains(err.Error(), keyID(key)) || !strings.Contains(err.Error(), keyID(wrong)) {
		t.Errorf("wrong key: %v, want both IDs", err)
	}
	if _, err := unwrapKEKs(key, []kekRow{{1, wrapped, keyID(key), "retiring"}}); err == nil {
		t.Error("no active KEK: no error")
	}
	if _, err := unwrapKEKs(key, nil); err == nil {
		t.Error("no KEK: no error")
	}
}

func writeKey(t *testing.T, mode os.FileMode, size int) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "unwrap.key")
	if err := os.WriteFile(p, bytes.Repeat([]byte{7}, size), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

// SECR-7: the sources refuse files others can read, and keys of the wrong
// size. (A file of another owner needs root to make; the check is the same
// branch as the mode's.)
func TestSources(t *testing.T) {
	ctx := context.Background()
	for _, mode := range []os.FileMode{0o400, 0o600} {
		if k, err := (FileSource{writeKey(t, mode, keySize)}).Load(ctx); err != nil || len(k) != keySize {
			t.Errorf("mode %v: %v", mode, err)
		}
	}
	for _, c := range []struct {
		mode os.FileMode
		size int
	}{{0o640, keySize}, {0o604, keySize}, {0o600, keySize - 1}, {0o600, keySize + 1}, {0o600, 0}} {
		if _, err := (FileSource{writeKey(t, c.mode, c.size)}).Load(ctx); err == nil {
			t.Errorf("mode %v, %d bytes: no error", c.mode, c.size)
		}
	}
	if _, err := (FileSource{t.TempDir()}).Load(ctx); err == nil {
		t.Error("a directory: no error")
	}

	t.Setenv("CREDENTIALS_DIRECTORY", "")
	if _, err := (CredentialSource{"veho-unwrap"}).Load(ctx); err == nil || !strings.Contains(err.Error(), "CREDENTIALS_DIRECTORY") {
		t.Errorf("no $CREDENTIALS_DIRECTORY: %v", err)
	}
	p := writeKey(t, 0o400, keySize)
	t.Setenv("CREDENTIALS_DIRECTORY", filepath.Dir(p))
	if _, err := (CredentialSource{filepath.Base(p)}).Load(ctx); err != nil {
		t.Errorf("credential: %v", err)
	}
	if _, err := (CredentialSource{"../" + filepath.Base(p)}).Load(ctx); err == nil {
		t.Error("a credential outside the directory: no error")
	}
}

func TestParseSource(t *testing.T) {
	for in, want := range map[string]UnwrapKeySource{
		"file:/etc/veho/unwrap.key":      FileSource{"/etc/veho/unwrap.key"},
		"systemd-credential:veho-unwrap": CredentialSource{"veho-unwrap"},
	} {
		if got, err := ParseSource(in); err != nil || got != want || got.Name() != in {
			t.Errorf("%s: %v, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "file:", "vault:x", "systemd-credential:a/b", "/etc/veho/unwrap.key"} {
		if _, err := ParseSource(in); err == nil {
			t.Errorf("%q: no error", in)
		}
	}
}

// SECR-9: a Secret never prints its value.
func TestSecretRedacted(t *testing.T) {
	s := &Secret{b: []byte("canary-value")}
	holder := struct{ S *Secret }{s}
	var out bytes.Buffer
	for _, f := range []string{"%v", "%s", "%+v", "%#v", "%q", "%x"} {
		fmt.Fprintf(&out, f+"\n", s)
		fmt.Fprintf(&out, f+"\n", holder)
	}
	slog.New(slog.NewTextHandler(&out, nil)).Info("x", "secret", s, "holder", holder)
	slog.New(slog.NewJSONHandler(&out, nil)).Info("x", "secret", s, "holder", holder)
	j, _ := json.Marshal(holder)
	out.Write(j)
	if strings.Contains(out.String(), "canary") || strings.Contains(out.String(), "63616e617279") {
		t.Errorf("the value shows:\n%s", out.String())
	}
	s.Zero()
	if !bytes.Equal(s.Bytes(), make([]byte, len("canary-value"))) {
		t.Error("Zero left bytes")
	}
}

// SECR-8, unit part: a reference without db:// is unavailable and invalid.
func TestFetchBadRef(t *testing.T) {
	_, err := NewStore(memKeyring(nil), nil, nil, &audit.Recorder{}).Fetch(context.Background(), uuid.New(), "vault://x")
	if !errors.Is(err, ErrUnavailable) || !errors.Is(err, ErrInvalid) {
		t.Errorf("got %v", err)
	}
}

func FuzzOpen(f *testing.F) {
	ctx := context.Background()
	ring := memKeyring(map[int][]byte{1: newKey()})
	tenant := uuid.New()
	dek, ct, err := sealValue(ctx, ring, tenant, "s3", 1, 1, []byte("value"))
	if err != nil {
		f.Fatal(err)
	}
	f.Add(dek, ct)
	f.Add([]byte{}, []byte{})
	f.Fuzz(func(t *testing.T, d, c []byte) {
		if bytes.Equal(d, dek) && bytes.Equal(c, ct) {
			return
		}
		if _, err := openValue(ctx, ring, tenant, "s3", 1, 1, d, c); err == nil {
			t.Fatal("forged blob decrypts")
		}
	})
}
