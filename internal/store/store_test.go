package store

import (
	"context"
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
	base := "postgres://veho_api@db-a:5432,db-b:5432/veho?sslrootcert=" + caFile(t) + "&sslmode="
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

// dial gives up on a host that does not answer within its timeout, with an
// error that is not a context deadline: DBOS retries a failed connection only
// when it is not one, so a failover would otherwise end its workflows (AC-5).
func TestDialTimeout(t *testing.T) {
	defer func(d time.Duration) { dialTimeout = d }(dialTimeout)
	dialTimeout = 200 * time.Millisecond
	start := time.Now()
	conn, err := dial(context.Background(), "tcp", "192.0.2.1:5432") // TEST-NET-1: never answers
	if err == nil {
		_ = conn.Close()
		t.Fatal("connected to TEST-NET-1")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("gave up after %v", d)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("%v: reads as a context deadline", err)
	}
}

// STO-C5: the offset is the database's time against the midpoint of the
// query's round trip, so the trip's length cancels out.
func TestClockOffset(t *testing.T) {
	base := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		sent, received, db time.Time
		want               time.Duration
	}{
		{base, base.Add(200 * time.Millisecond), base.Add(100 * time.Millisecond), 0},
		{base, base.Add(200 * time.Millisecond), base.Add(2100 * time.Millisecond), 2 * time.Second},
		{base, base.Add(40 * time.Millisecond), base.Add(-480 * time.Millisecond), -500 * time.Millisecond},
	} {
		if got := clockOffset(tc.sent, tc.received, tc.db); got != tc.want {
			t.Errorf("sent %v, received %v, db %v: %v, want %v", tc.sent, tc.received, tc.db, got, tc.want)
		}
	}
}
