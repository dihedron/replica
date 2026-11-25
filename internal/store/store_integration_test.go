//go:build integration

package store

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/dihedron/veho/internal/store/migrate"
	"github.com/dihedron/veho/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// dsn returns the DSN of an environment variable (test/lab/lab.sh env), or
// skips the test.
func dsn(t *testing.T, env string) string {
	t.Helper()
	v := os.Getenv(env)
	if v == "" {
		t.Skipf("%s is not set: run with test/lab/lab.sh test", env)
	}
	return v
}

// migrated applies the migrations as veho_migrator.
func migrated(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	conn, err := Connect(ctx, dsn(t, "VEHO_TEST_DATABASE_URL"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	if _, err := migrate.Apply(ctx, conn, migrations.FS); err != nil {
		t.Fatal(err)
	}
}

// open opens a pool on the DSN of env.
func open(t *testing.T, env string) *pgxpool.Pool {
	t.Helper()
	p, err := Open(context.Background(), dsn(t, env), Options{Name: "test/" + strings.ToLower(env)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

// fixture: two tenants with an endpoint each, made through the API role.
type fixture struct {
	api, worker *pgxpool.Pool
	t1, t2      uuid.UUID
	ep1, ep2    uuid.UUID
}

func setup(t *testing.T) fixture {
	t.Helper()
	migrated(t)
	ctx := context.Background()
	f := fixture{api: open(t, "VEHO_TEST_API_DATABASE_URL"), worker: open(t, "VEHO_TEST_WORKER_DATABASE_URL")}
	if _, err := f.api.Exec(ctx, "select veho.put_location('lab-a', 'region', null)"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []struct {
		tenant, endpoint *uuid.UUID
	}{{&f.t1, &f.ep1}, {&f.t2, &f.ep2}} {
		if err := f.api.QueryRow(ctx, "select veho.create_tenant($1)", "test-"+uuid.New().String()).Scan(p.tenant); err != nil {
			t.Fatal(err)
		}
		*p.endpoint = uuid.NewV7()
		if err := InTenant(ctx, f.api, *p.tenant, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, "insert into veho.endpoints (id, tenant_id, name, location_id, driver) values ($1, $2, 'src', 'lab-a', 's3')", *p.endpoint, *p.tenant)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

// insertJob inserts a job with one transfer under tenant, from and to ep.
func insertJob(ctx context.Context, tx pgx.Tx, tenant, ep uuid.UUID) (job, transfer uuid.UUID, err error) {
	job, transfer = uuid.NewV7(), uuid.NewV7()
	if _, err = tx.Exec(ctx, `insert into veho.jobs (id, tenant_id, source_endpoint_id, source_path, policy)
		values ($1, $2, $3, 'a/b', '{"min_copies": 1}')`, job, tenant, ep); err != nil {
		return
	}
	_, err = tx.Exec(ctx, `insert into veho.transfers (id, tenant_id, job_id, destination_endpoint_id, position, dest_path)
		values ($1, $2, $3, $4, '{"region": "lab-a"}', 'a/b')`, transfer, tenant, job, ep)
	return
}

// sqlState is the SQLSTATE of err, or "".
func sqlState(err error) string {
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok {
		return pgErr.Code
	}
	return ""
}

// TestNoTenant: STO-3.
func TestNoTenant(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	for name, p := range map[string]*pgxpool.Pool{"api": f.api, "worker": f.worker} {
		for _, table := range []string{"tenants", "endpoints", "policies", "jobs", "transfers", "parts"} {
			var n int
			if err := p.QueryRow(ctx, "select count(*) from veho."+table).Scan(&n); err != nil || n != 0 {
				t.Errorf("%s: %s without a tenant: %d rows (%v)", name, table, n, err)
			}
		}
	}
	_, err := f.api.Exec(ctx, "insert into veho.endpoints (tenant_id, name, location_id, driver) values ($1, 'x', 'lab-a', 's3')", f.t1)
	if sqlState(err) != "42501" {
		t.Errorf("insert without a tenant: %v, want 42501", err)
	}
}

// TestIsolation: STO-4.
func TestIsolation(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	err := InTenant(ctx, f.api, f.t2, func(tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, "select count(*) from veho.endpoints where id = $1", f.ep1).Scan(&n); err != nil || n != 0 {
			t.Errorf("T2 sees T1's endpoint: %d (%v)", n, err)
		}
		for _, stmt := range []string{"update veho.endpoints set name = 'stolen' where id = $1", "delete from veho.endpoints where id = $1"} {
			tag, err := tx.Exec(ctx, stmt, f.ep1)
			if err != nil || tag.RowsAffected() != 0 {
				t.Errorf("%q on T1's endpoint: %v, %v", stmt, tag, err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	err = InTenant(ctx, f.api, f.t2, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "insert into veho.endpoints (tenant_id, name, location_id, driver) values ($1, 'x', 'lab-a', 's3')", f.t1)
		return err
	})
	if sqlState(err) != "42501" {
		t.Errorf("T2 inserts for T1: %v, want 42501", err)
	}
	err = InTenant(ctx, f.api, f.t1, func(tx pgx.Tx) error {
		_, _, err := insertJob(ctx, tx, f.t1, f.ep2)
		return err
	})
	if sqlState(err) != "23503" {
		t.Errorf("T1's job from T2's endpoint: %v, want 23503", err)
	}
}

// TestPrivileges: STO-5.
func TestPrivileges(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	for _, c := range []struct {
		name string
		pool *pgxpool.Pool
		stmt string
		args []any
	}{
		{"worker inserts an endpoint", f.worker, "insert into veho.endpoints (tenant_id, name, location_id, driver) values ($1, 'x', 'lab-a', 's3')", []any{f.t1}},
		{"worker updates a policy", f.worker, "update veho.policies set version = 2", nil},
		{"worker updates a tenant", f.worker, "update veho.tenants set state = 'suspended'", nil},
		{"worker creates a tenant", f.worker, "select veho.create_tenant('x')", nil},
		{"api writes executors", f.api, "insert into veho.executors (id, region, app_version) values ('x', 'a', 'v')", nil},
		{"api scans across tenants", f.api, "select * from veho.expired_transfers(1)", nil},
		{"api creates a table", f.api, "create table veho.x (i int)", nil},
		{"worker creates a table", f.worker, "create table veho.x (i int)", nil},
	} {
		err := InTenant(ctx, c.pool, f.t1, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, c.stmt, c.args...)
			return err
		})
		if sqlState(err) != "42501" {
			t.Errorf("%s: %v, want 42501", c.name, err)
		}
	}
}

// TestImmutable: STO-6.
func TestImmutable(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	var job, transfer uuid.UUID
	if err := InTenant(ctx, f.api, f.t1, func(tx pgx.Tx) (err error) {
		job, transfer, err = insertJob(ctx, tx, f.t1, f.ep1)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string]*pgxpool.Pool{"api": f.api, "worker": f.worker} {
		for _, stmt := range []string{
			`update veho.jobs set policy = '{"min_copies": 2}' where id = $1`,
			`update veho.jobs set source_path = 'c/d' where id = $1`,
			`update veho.transfers set position = '{"region": "x"}' where id = $1`,
			`update veho.transfers set dest_path = 'x' where id = $1`,
		} {
			id := job
			if strings.Contains(stmt, "transfers") {
				id = transfer
			}
			err := InTenant(ctx, p, f.t1, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, stmt, id)
				return err
			})
			if sqlState(err) != "23000" {
				t.Errorf("%s: %q: %v, want 23000", name, stmt, err)
			}
		}
	}
	if err := InTenant(ctx, f.worker, f.t1, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "update veho.jobs set state = 'RUNNING', updated_at = now() where id = $1", job)
		return err
	}); err != nil {
		t.Errorf("a mutable column: %v", err)
	}
}

// TestCrossTenant: STO-7.
func TestCrossTenant(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	var job, transfer uuid.UUID
	if err := InTenant(ctx, f.api, f.t1, func(tx pgx.Tx) (err error) {
		job, transfer, err = insertJob(ctx, tx, f.t1, f.ep1)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := InTenant(ctx, f.worker, f.t1, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "update veho.transfers set state = 'COPYING', lease_owner = 'gone', lease_expires_at = now() - interval '1 minute' where id = $1", transfer); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, "update veho.jobs set settled_at = now() - interval '1 hour' where id = $1", job)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	found := func(stmt string, args []any, want uuid.UUID) bool {
		rows, err := f.worker.Query(ctx, stmt, args...)
		if err != nil {
			t.Fatal(err)
		}
		pairs, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct{ Tenant, ID uuid.UUID }])
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range pairs {
			if p.ID == want {
				return p.Tenant == f.t1
			}
		}
		return false
	}
	if !found("select * from veho.expired_transfers(1000)", nil, transfer) {
		t.Error("expired_transfers misses the expired transfer")
	}
	if !found("select * from veho.settled_jobs_before($1, 1000)", []any{time.Now()}, job) {
		t.Error("settled_jobs_before misses the settled job")
	}
}

// TestChecks: STO-9, on the lab the checks pass; on a standby or with
// synchronous_commit = local they fail.
func TestChecks(t *testing.T) {
	migrated(t)
	ctx := context.Background()
	worker := dsn(t, "VEHO_TEST_WORKER_DATABASE_URL")
	errs := Checks(ctx, open(t, "VEHO_TEST_WORKER_DATABASE_URL"), migrations.FS, time.Second)
	standalone := os.Getenv("VEHO_TEST_STANDALONE") != "" // one PostgreSQL, no standby (test/ci)
	if standalone && (len(errs) != 1 || !containsErr(errs, ErrNoQuorum)) || !standalone && len(errs) != 0 {
		t.Errorf("Checks: %v", errs)
	}
	local, err := Open(ctx, worker+"&synchronous_commit=local", Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	if errs := Checks(ctx, local, migrations.FS, time.Second); !containsErr(errs, ErrSyncCommit) {
		t.Errorf("Checks with synchronous_commit = local: %v", errs)
	}
	if standalone {
		return // no standby to check against
	}
	standby := standbyDSN(t, worker)
	sp, err := Open(ctx, standby, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	if errs := Checks(ctx, sp, migrations.FS, time.Second); !containsErr(errs, ErrNotPrimary) {
		t.Errorf("Checks on a standby: %v", errs)
	}
	// STO-C5: no clock agrees with the database's to the nanosecond
	if errs := Checks(ctx, open(t, "VEHO_TEST_WORKER_DATABASE_URL"), migrations.FS, time.Nanosecond); !containsErr(errs, ErrClock) {
		t.Errorf("Checks with a 1ns bound: %v", errs)
	}
}

func containsErr(errs []error, target error) bool {
	for _, err := range errs {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

// standbyDSN returns dsn reduced to one host that is a standby.
func standbyDSN(t *testing.T, dsn string) string {
	t.Helper()
	at, slash := strings.Index(dsn, "@"), strings.Index(dsn, "/veho?")
	hosts := strings.Split(dsn[at+1:slash], ",")
	query := strings.Replace(dsn[slash:], "&target_session_attrs=read-write", "", 1)
	for _, h := range hosts {
		one := dsn[:at+1] + h + query
		conn, err := pgx.Connect(context.Background(), one)
		if err != nil {
			continue
		}
		var recovery bool
		err = conn.QueryRow(context.Background(), "select pg_is_in_recovery()").Scan(&recovery)
		_ = conn.Close(context.Background())
		if err == nil && recovery {
			return one
		}
	}
	t.Skip("no standby reachable")
	return ""
}

// TestEnqueueInTransaction: STO-10, a job and its workflow, enqueued by
// veho_api in one transaction, commit or roll back together.
func TestEnqueueInTransaction(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	dbosDSN := dsn(t, "VEHO_TEST_DBOS_DATABASE_URL")
	quiet := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	dctx, err := dbos.NewContext(ctx, dbos.Config{AppName: "veho-store-test", DatabaseURL: dbosDSN, ExecutorID: "store-test", Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	dbos.RegisterWorkflow(dctx, noop, dbos.WithWorkflowName("noop"))
	if _, err := dbos.RegisterQueue(dctx, "store-test"); err != nil {
		t.Fatal(err)
	}
	if err := dbos.Launch(dctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dbos.Shutdown(dctx, 5*time.Second) }()
	client, err := dbos.NewClient(ctx, dbos.ClientConfig{DatabaseURL: dbosDSN, AppName: "veho-store-test", Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dbos.Shutdown(client, 5*time.Second) }()

	for _, commit := range []bool{true, false} {
		var job uuid.UUID
		run := func(tx pgx.Tx) error {
			var err error
			if job, _, err = insertJob(ctx, tx, f.t1, f.ep1); err != nil {
				return err
			}
			if _, err := dbos.Enqueue[string](client, "store-test", "noop", job.String(),
				dbos.WithEnqueueWorkflowID(job.String()), dbos.WithEnqueueTransaction(tx)); err != nil {
				return err
			}
			if !commit {
				return errRollback
			}
			return nil
		}
		if err := InTenant(ctx, f.api, f.t1, run); err != nil && !errors.Is(err, errRollback) {
			t.Fatalf("commit=%v: %v", commit, err)
		}
		var jobs, workflows int
		_ = InTenant(ctx, f.api, f.t1, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, "select count(*) from veho.jobs where id = $1", job).Scan(&jobs)
		})
		if err := f.api.QueryRow(ctx, "select count(*) from dbos.workflow_status where workflow_uuid = $1", job.String()).Scan(&workflows); err != nil {
			t.Fatal(err)
		}
		want := 0
		if commit {
			want = 1
		}
		if jobs != want || workflows != want {
			t.Errorf("commit=%v: %d job(s), %d workflow(s), want %d and %d", commit, jobs, workflows, want, want)
		}
	}
}

var errRollback = errors.New("roll back on purpose")

func noop(_ dbos.Context, in string) (string, error) { return in, nil }

// TryLock: one holder at a time, and a holder that goes silent (a node cut
// off by a partition, whose session the server would keep until TCP
// keepalives give up) loses the lock after its idle timeout (found by AC-6).
func TestTryLockIdle(t *testing.T) {
	pool := open(t, "VEHO_TEST_WORKER_DATABASE_URL")
	ctx := context.Background()
	name := "veho-test-lock-" + uuid.New().String()
	release, locked, err := TryLock(ctx, pool, name, time.Second)
	if err != nil || !locked {
		t.Fatalf("first: %v, %v", locked, err)
	}
	defer release()
	if _, locked, err := TryLock(ctx, pool, name, time.Second); err != nil || locked {
		t.Fatalf("second while held: %v, %v", locked, err)
	}
	time.Sleep(2500 * time.Millisecond) // the holder says nothing
	r2, locked, err := TryLock(ctx, pool, name, time.Second)
	if err != nil || !locked {
		t.Fatalf("after the holder's idle timeout: %v, %v", locked, err)
	}
	r2()
}

// A host that does not answer, first in the DSN, costs a connection only the
// connect timeout before the next host is tried (found by AC-6: a node in a
// healthy region, its DSN listing the partitioned region's member first,
// waited minutes for every new connection).
func TestConnectSkipsDeadHost(t *testing.T) {
	dsn := dsn(t, "VEHO_TEST_DATABASE_URL")
	at := strings.Index(dsn, "@")
	dead := dsn[:at+1] + "192.0.2.1:5432," + dsn[at+1:] // TEST-NET-1: never answers
	start := time.Now()
	conn, err := Connect(context.Background(), dead, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	if d := time.Since(start); d > 15*time.Second {
		t.Errorf("connected after %v", d)
	}
}
