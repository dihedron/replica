//go:build integration

package main

import (
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dihedron/veho/internal/audit"
	"github.com/dihedron/veho/internal/store"
)

// TestKEK: veho kek init, show, rotate, rewrap and rekey, in order, on the
// lab; the details are tested in internal/secrets.
func TestKEK(t *testing.T) {
	if os.Getenv("VEHO_TEST_DATABASE_URL") == "" {
		t.Skip("VEHO_TEST_DATABASE_URL is not set: run with test/lab/lab.sh test")
	}
	if err := (&MigrateCommand{dbOptions{DSNEnv: "VEHO_TEST_DATABASE_URL"}}).Execute(nil); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	conn, err := store.Connect(ctx, os.Getenv("VEHO_TEST_DATABASE_URL"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	// the lock of internal/secrets' tests, which share veho.kek
	for _, q := range []string{
		"select pg_advisory_lock(hashtext('veho-test-kek'))",
		"update veho.endpoints set credential_ref = null where credential_ref like 'db://%'",
		"delete from veho.secrets",
		"delete from veho.kek",
	} {
		if _, err := conn.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	key := func() string {
		p := filepath.Join(t.TempDir(), "unwrap.key")
		if err := os.WriteFile(p, []byte((rand.Text() + rand.Text())[:32]), 0o600); err != nil {
			t.Fatal(err)
		}
		return "file:" + p
	}
	db := dbOptions{DSNEnv: "VEHO_TEST_DATABASE_URL"}
	rec := &audit.Recorder{}
	aud := auditOptions{auditor: rec}
	k1, k2 := keyOptions{key()}, keyOptions{key()}
	steps := []struct {
		name string
		cmd  interface{ Execute([]string) error }
		ok   bool
	}{
		{"init", &KEKInitCommand{db, k1, aud}, true},
		{"init again", &KEKInitCommand{db, k1, aud}, false},
		{"rotate", &KEKRotateCommand{db, k1, aud}, true},
		{"rewrap", &KEKRewrapCommand{db, k1, aud, 500}, true},
		{"rekey", &KEKRekeyCommand{db, k1, aud, k2.UnwrapKey}, true},
		{"rotate with the old key", &KEKRotateCommand{db, k1, aud}, false},
		{"rotate with the new key", &KEKRotateCommand{db, k2, aud}, true},
		{"show", &KEKShowCommand{db}, true},
		{"a bad unwrap key", &KEKRotateCommand{db, keyOptions{"vault:x"}, aud}, false},
	}
	for _, s := range steps {
		if err := s.cmd.Execute(nil); (err == nil) != s.ok {
			t.Errorf("%s: %v", s.name, err)
		}
	}
	// AUDL-4: one event per command that reached the database
	var got []string
	for _, e := range rec.Events() {
		got = append(got, e.Action+":"+e.Outcome)
		if e.Principal == "" {
			t.Errorf("%s: no principal", e.Action)
		}
	}
	want := "KEK_INIT:success KEK_INIT:failed KEK_ROTATE:success KEK_REWRAP:success KEK_REKEY:success KEK_ROTATE:failed KEK_ROTATE:success"
	if strings.Join(got, " ") != want {
		t.Errorf("events:\n%s\nwant:\n%s", strings.Join(got, " "), want)
	}
}
