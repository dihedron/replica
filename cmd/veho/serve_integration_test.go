//go:build integration

package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dihedron/veho/internal/engine"
	"github.com/dihedron/veho/internal/secrets"
	"github.com/dihedron/veho/internal/store"
)

// TestServe: veho serve starts (checks, keyring, topology, DBOS), beats,
// and stops on its context.
func TestServe(t *testing.T) {
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
	dir := t.TempDir()
	key := filepath.Join(dir, "unwrap.key")
	if err := os.WriteFile(key, make([]byte, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	// the KEKs under this test's key, with the lock of internal/secrets' tests
	for _, q := range []string{
		"select pg_advisory_lock(hashtext('veho-test-kek'))",
		"update veho.endpoints set credential_ref = null where credential_ref like 'db://%'",
		"delete from veho.secrets", "delete from veho.kek",
		"select veho.put_location('lab-a', 'region', null)",
	} {
		if _, err := conn.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if err := secrets.InitKEK(ctx, conn, secrets.FileSource{Path: key}); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "veho.yaml")
	if err := os.WriteFile(cfg, []byte(`
node: {id: test-serve, region: lab-a}
database:
  api: {dsn_env: VEHO_TEST_API_DATABASE_URL}
  worker: {dsn_env: VEHO_TEST_WORKER_DATABASE_URL}
  dbos: {dsn_env: VEHO_TEST_DBOS_DATABASE_URL}
secrets: {unwrap_key: "file:`+key+`"}
audit: {sink: stderr}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	ready := make(chan *engine.Engine, 1)
	rctx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- (&ServeCommand{Config: cfg, ready: ready}).run(rctx) }()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("serve stopped: %v", err)
	case <-time.After(60 * time.Second):
		t.Fatal("serve did not start")
	}
	var age float64
	if err := conn.QueryRow(ctx, "select extract(epoch from now() - last_seen) from veho.executors where id = 'test-serve'").Scan(&age); err != nil || age > 10 {
		t.Errorf("heartbeat %vs ago, %v", age, err)
	}
	stop()
	if err := <-done; err != nil {
		t.Errorf("stop: %v", err)
	}
}
