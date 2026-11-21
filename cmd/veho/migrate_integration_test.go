//go:build integration

package main

import (
	"errors"
	"os"
	"testing"

	"github.com/dihedron/veho/internal/store"
)

// TestMigrate: veho migrate applies what is missing (nothing the second
// time), reads the DSN only from the named variable, and refuses a DSN
// without verify-full.
func TestMigrate(t *testing.T) {
	if os.Getenv("VEHO_TEST_DATABASE_URL") == "" {
		t.Skip("VEHO_TEST_DATABASE_URL is not set: run with test/lab/lab.sh test")
	}
	for range 2 {
		if err := (&MigrateCommand{DSNEnv: "VEHO_TEST_DATABASE_URL"}).Execute(nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := (&MigrateCommand{DSNEnv: "VEHO_TEST_NOT_SET"}).Execute(nil); err == nil {
		t.Error("an unset variable: no error")
	}
	t.Setenv("VEHO_TEST_PLAIN_DSN", "postgres://veho_migrator:x@127.0.0.1:1/veho?sslmode=disable")
	if err := (&MigrateCommand{DSNEnv: "VEHO_TEST_PLAIN_DSN"}).Execute(nil); !errors.Is(err, store.ErrInsecureConnection) {
		t.Errorf("a DSN without TLS: %v", err)
	}
}
