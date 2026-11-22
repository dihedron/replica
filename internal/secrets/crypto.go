// Package secrets keeps the tenants' credentials in the state store,
// envelope-encrypted (.specs/veho-secrets.md): a value under its own data key
// (DEK), the DEK wrapped by a versioned KEK, the KEK stored wrapped by an
// unwrap key that the platform gives the node and that is never in the
// database. Adapted from ligo 9f11da9 (internal/sealing: sealing.go,
// localkek.go): ligo seals under one KEK; veho adds the versioned, wrapped
// KEKs, the tenant-bound associated data and rotation.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"uuid"
)

// keySize is the size of every key: unwrap key, KEK and DEK (AES-256).
const keySize = 32

// The labels of the associated data of the three layers (§1).
const (
	labelKEK    = "veho-kek/v1"
	labelDEK    = "veho-dek/v1"
	labelSecret = "veho-secret/v1" //nolint:gosec // a label of the associated data, not a credential
)

// aad encodes label and fields, each prefixed by its 2-byte big-endian
// length, so that no two lists of fields encode alike.
func aad(label string, fields ...[]byte) []byte {
	out := binary.BigEndian.AppendUint16(nil, uint16(len(label))) //nolint:gosec // labels are short constants
	out = append(out, label...)
	for _, f := range fields {
		out = binary.BigEndian.AppendUint16(out, uint16(len(f))) //nolint:gosec // fields are at most 64 bytes
		out = append(out, f...)
	}
	return out
}

func u32(n int) []byte { return binary.BigEndian.AppendUint32(nil, uint32(n)) } //nolint:gosec // versions are positive int4

func kekAAD(version int) []byte { return aad(labelKEK, u32(version)) }

func dekAAD(tenant uuid.UUID, name string, version, kekVersion int) []byte {
	return aad(labelDEK, tenant[:], []byte(name), u32(version), u32(kekVersion))
}

func valueAAD(tenant uuid.UUID, name string, version int) []byte {
	return aad(labelSecret, tenant[:], []byte(name), u32(version))
}

// newKey returns a random key.
func newKey() []byte {
	k := make([]byte, keySize)
	_, _ = rand.Read(k) // never fails
	return k
}

func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != keySize {
		return nil, fmt.Errorf("secrets: a key of %d bytes, want %d", len(key), keySize)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secrets: %w", err)
	}
	return cipher.NewGCM(block)
}

// seal encrypts plaintext under key with a random nonce: nonce || sealed.
func seal(key, plaintext, ad []byte) ([]byte, error) {
	aead, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize(), aead.NonceSize()+len(plaintext)+aead.Overhead())
	_, _ = rand.Read(nonce) // never fails
	return aead.Seal(nonce, nonce, plaintext, ad), nil
}

// errOpen is the failure to decrypt: a wrong key, a tampered blob or other
// associated data, which GCM does not tell apart.
var errOpen = errors.New("does not decrypt")

// open decrypts the output of seal.
func open(key, sealed, ad []byte) ([]byte, error) {
	aead, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	if len(sealed) < aead.NonceSize()+aead.Overhead() {
		return nil, errOpen
	}
	n := aead.NonceSize()
	out, err := aead.Open(nil, sealed[:n], sealed[n:], ad)
	if err != nil {
		return nil, errOpen
	}
	return out, nil
}

// keyID names an unwrap key without revealing it (§1).
func keyID(unwrapKey []byte) string {
	m := hmac.New(sha256.New, unwrapKey)
	m.Write([]byte("veho-unwrap-key-id"))
	return hex.EncodeToString(m.Sum(nil)[:8])
}
