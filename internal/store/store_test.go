package store

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// caFile writes a throwaway CA certificate, for sslrootcert.
func caFile(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"}, IsCA: true,
		BasicConstraintsValid: true, NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestCheckDSN: STO-8, only verify-full without a plaintext fallback.
func TestCheckDSN(t *testing.T) {
	base := "postgres://veho_api:x@db-a:5432,db-b:5432/veho?sslrootcert=" + caFile(t) + "&sslmode="
	for mode, ok := range map[string]bool{"disable": false, "allow": false, "prefer": false, "require": false, "verify-ca": false, "verify-full": true} {
		err := CheckDSN(base+mode, false)
		if ok != (err == nil) {
			t.Errorf("sslmode=%s: %v", mode, err)
		}
		if !ok && !errors.Is(err, ErrInsecureConnection) {
			t.Errorf("sslmode=%s: %v, want ErrInsecureConnection", mode, err)
		}
	}
	if err := CheckDSN(base+"disable", true); err != nil {
		t.Errorf("allowInsecure: %v", err)
	}
}

// TestQuorum: STO-C3's reading of synchronous_standby_names.
func TestQuorum(t *testing.T) {
	for names, want := range map[string]bool{
		`ANY 1 ("db-a","db-c")`: true,
		`any 2 (a, b, c)`:       true,
		`ANY 1 (*)`:             true,
		``:                      false,
		`db-a`:                  false,
		`FIRST 1 (a, b)`:        false,
		`1 (a, b)`:              false,
		`ANY 0 (a)`:             false,
		`ANY x (a)`:             false,
	} {
		if got := quorum(names); got != want {
			t.Errorf("quorum(%q) = %v, want %v", names, got, want)
		}
	}
}
