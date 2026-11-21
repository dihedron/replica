//go:build integration

package main

import (
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"

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
	k1, k2 := keyOptions{key()}, keyOptions{key()}
	steps := []struct {
		name string
		cmd  interface{ Execute([]string) error }
		ok   bool
	}{
		{"init", &KEKInitCommand{db, k1}, true},
		{"init again", &KEKInitCommand{db, k1}, false},
		{"rotate", &KEKRotateCommand{db, k1}, true},
		{"rewrap", &KEKRewrapCommand{db, k1, 500}, true},
		{"rekey", &KEKRekeyCommand{db, k1, k2.UnwrapKey}, true},
		{"rotate with the old key", &KEKRotateCommand{db, k1}, false},
		{"rotate with the new key", &KEKRotateCommand{db, k2}, true},
		{"show", &KEKShowCommand{db}, true},
		{"a bad unwrap key", &KEKRotateCommand{db, keyOptions{"vault:x"}}, false},
	}
	for _, s := range steps {
		if err := s.cmd.Execute(nil); (err == nil) != s.ok {
			t.Errorf("%s: %v", s.name, err)
		}
	}
}
