//go:build integration

package migrate

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/dihedron/veho/migrations"
	"github.com/jackc/pgx/v5"
)

// connect opens a connection as veho_migrator (test/lab/lab.sh env); the
// test is skipped when VEHO_TEST_DATABASE_URL is unset.
func connect(t *testing.T) *pgx.Conn {
	t.Helper()
	dsn := os.Getenv("VEHO_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("VEHO_TEST_DATABASE_URL is not set: run with test/lab/lab.sh test")
	}
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

// reset empties the schema veho, as roles.sql leaves it.
func reset(t *testing.T, conn *pgx.Conn) {
	t.Helper()
	if _, err := conn.Exec(context.Background(), "drop schema veho cascade; create schema veho"); err != nil {
		t.Fatal(err)
	}
}

// TestApply: STO-1, a second run applies nothing, and Check agrees.
func TestApply(t *testing.T) {
	ctx := context.Background()
	conn := connect(t)
	reset(t, conn)
	if err := Check(ctx, conn, migrations.FS); err == nil {
		t.Error("Check on an empty schema: no error")
	}
	done, err := Apply(ctx, conn, migrations.FS)
	if err != nil || len(done) == 0 {
		t.Fatalf("Apply = (%v, %v)", done, err)
	}
	again, err := Apply(ctx, conn, migrations.FS)
	if err != nil || len(again) != 0 {
		t.Errorf("second Apply = (%v, %v), want nothing applied", again, err)
	}
	if err := Check(ctx, conn, migrations.FS); err != nil {
		t.Errorf("Check after Apply: %v", err)
	}
}

// TestChanged: STO-1, an applied file whose content changed stops Apply and
// fails Check; a database with an unknown version is ahead.
func TestChanged(t *testing.T) {
	ctx := context.Background()
	conn := connect(t)
	if _, err := Apply(ctx, conn, migrations.FS); err != nil {
		t.Fatal(err)
	}
	changed := fstest.MapFS{"001_core.sql": {Data: []byte("select 'edited';")}}
	if _, err := Apply(ctx, conn, changed); !errors.Is(err, ErrChanged) {
		t.Errorf("Apply with an edited 001: %v, want ErrChanged", err)
	}
	if err := Check(ctx, conn, changed); !errors.Is(err, ErrChanged) {
		t.Errorf("Check with an edited 001: %v, want ErrChanged", err)
	}
	older := fstest.MapFS{"000_before.sql": {Data: []byte("select 0;")}}
	if err := Check(ctx, conn, older); !errors.Is(err, ErrAhead) {
		t.Errorf("Check by a binary that does not know 001: %v, want ErrAhead", err)
	}
}

// TestConcurrent: STO-2, two migrations at once apply each file once.
func TestConcurrent(t *testing.T) {
	ctx := context.Background()
	reset(t, connect(t))
	conns := []*pgx.Conn{connect(t), connect(t)}
	var wg sync.WaitGroup
	results := make([][]string, len(conns))
	errs := make([]error, len(conns))
	for i, c := range conns {
		wg.Go(func() { results[i], errs[i] = Apply(ctx, c, migrations.FS) })
	}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil {
		t.Fatalf("errors: %v, %v", errs[0], errs[1])
	}
	if len(results[0]) > 0 && len(results[1]) > 0 {
		t.Errorf("both applied migrations: %v and %v", results[0], results[1])
	}
	var n int
	if err := conns[0].QueryRow(ctx, "select count(*) from veho.schema_migrations where version = '001'").Scan(&n); err != nil || n != 1 {
		t.Errorf("001 recorded %d times (%v)", n, err)
	}
}
