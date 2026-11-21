//go:build integration

package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dihedron/veho/internal/audit"
	"github.com/dihedron/veho/internal/store"
)

// selfSigned writes a certificate and key for localhost into dir.
func selfSigned(t *testing.T, dir string) (string, string) {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"}, DNSNames: []string{"localhost"},
		IPAddresses: nil, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	kd, _ := x509.MarshalECPrivateKey(k)
	crt, key := filepath.Join(dir, "api.crt"), filepath.Join(dir, "api.key")
	if err := os.WriteFile(crt, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kd}), 0o600); err != nil {
		t.Fatal(err)
	}
	return crt, key
}

// stdout runs fn and returns what it printed.
func stdout(t *testing.T, fn func() error) string {
	t.Helper()
	r, w, _ := os.Pipe()
	saved := os.Stdout
	os.Stdout = w
	err := fn()
	os.Stdout = saved
	_ = w.Close()
	out, _ := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

// API-T8: veho tenant create and veho apikey create bootstrap a key whose
// hash is stored, and that no audit event or log line carries.
func TestBootstrap(t *testing.T) {
	if os.Getenv("VEHO_TEST_DATABASE_URL") == "" {
		t.Skip("VEHO_TEST_DATABASE_URL is not set: run with test/lab/lab.sh test")
	}
	if err := (&MigrateCommand{dbOptions{DSNEnv: "VEHO_TEST_DATABASE_URL"}}).Execute(nil); err != nil {
		t.Fatal(err)
	}
	db := dbOptions{DSNEnv: "VEHO_TEST_DATABASE_URL"}
	rec := &audit.Recorder{}
	name := "boot-" + strings.ReplaceAll(time.Now().Format("150405.000"), ".", "")
	tc := &TenantCreateCommand{dbOptions: db, auditOptions: auditOptions{auditor: rec}}
	tc.Args.Name = name
	if id := stdout(t, func() error { return tc.Execute(nil) }); len(id) != 36 {
		t.Fatalf("tenant id %q", id)
	}
	kc := &APIKeyCreateCommand{dbOptions: db, auditOptions: auditOptions{auditor: rec}, Tenant: name, Role: "admin", Name: "first-admin"}
	key := stdout(t, func() error { return kc.Execute(nil) })
	if !strings.HasPrefix(key, "veho_") || len(key) < 50 {
		t.Fatalf("key %q", key)
	}
	pc := &APIKeyCreateCommand{dbOptions: db, auditOptions: auditOptions{auditor: rec}, PlatformAdmin: true, Name: "root-" + name}
	_ = stdout(t, func() error { return pc.Execute(nil) })
	if err := (&APIKeyCreateCommand{dbOptions: db, auditOptions: auditOptions{auditor: rec}, Tenant: name, Name: "x"}).Execute(nil); err == nil {
		t.Error("a tenant without a role: no error")
	}

	ctx := context.Background()
	conn, err := store.Connect(ctx, os.Getenv("VEHO_TEST_DATABASE_URL"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	id, secret := key[5:17], key[18:]
	raw, _ := base64.RawURLEncoding.DecodeString(secret)
	want := sha256.Sum256(raw)
	var stored []byte
	if err := conn.QueryRow(ctx, "select sha256 from veho.api_keys where id = $1", id).Scan(&stored); err != nil || !bytes.Equal(stored, want[:]) {
		t.Errorf("stored hash: %x, %v", stored, err)
	}
	events, _ := json.Marshal(rec.Events())
	if strings.Contains(string(events), secret) || len(rec.Events()) != 3 {
		t.Errorf("events %s", events)
	}
}
