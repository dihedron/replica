//go:build integration

package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/dihedron/veho/internal/audit"
	"github.com/dihedron/veho/internal/config"
	"github.com/dihedron/veho/internal/policy"
	"github.com/dihedron/veho/internal/secrets"
	"github.com/dihedron/veho/internal/store"
	"github.com/dihedron/veho/internal/store/migrate"
	"github.com/dihedron/veho/migrations"
	"github.com/dihedron/veho/pkg/plugin"
	"github.com/dihedron/veho/pkg/plugin/file"
	"github.com/dihedron/veho/pkg/plugin/s3"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func dsn(t *testing.T, env string) string {
	t.Helper()
	v := os.Getenv(env)
	if v == "" {
		t.Skipf("%s is not set: run with test/lab/lab.sh test", env)
	}
	return v
}

type fixture struct {
	e           *Engine
	api, worker *pgxpool.Pool
	migrator    *pgx.Conn
	tenant      uuid.UUID
	audit       *audit.Recorder
	secrets     *secrets.Store
	flaky       *flaky
}

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	c, err := config.Parse([]byte(`
node: {id: test-` + uuid.New().String()[:8] + `, region: lab-a}
secrets: {unwrap_key: "file:/nonexistent"}
engine: {heartbeat: 1s, lease_ttl: 4s, part_size_mib: 5, parts_in_flight: 2}
notifications: {enabled: true, allow_cidrs: [127.0.0.0/8]}
api: {tls: {cert_file: /unused, key_file: /unused}}
`))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// setup: the migrations, the pools, an engine (not launched), a tenant with
// the endpoints src, d1, d2 in the locations lab-a and lab-b.
func setup(t *testing.T) fixture {
	t.Helper()
	ctx := context.Background()
	conn, err := store.Connect(ctx, dsn(t, "VEHO_TEST_DATABASE_URL"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(ctx) })
	if _, err := migrate.Apply(ctx, conn, migrations.FS); err != nil {
		t.Fatal(err)
	}
	f := fixture{migrator: conn, audit: &audit.Recorder{}, flaky: &flaky{Driver: file.New()}}
	for env, p := range map[string]**pgxpool.Pool{"VEHO_TEST_API_DATABASE_URL": &f.api, "VEHO_TEST_WORKER_DATABASE_URL": &f.worker} {
		if *p, err = store.Open(ctx, dsn(t, env), store.Options{Name: "test/engine"}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup((*p).Close)
	}
	// the KEKs under this test's key, with the lock of the other packages'
	// tests that share veho.kek
	key := filepath.Join(t.TempDir(), "unwrap.key")
	if err := os.WriteFile(key, []byte(strings.Repeat("k", 32)), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"select pg_advisory_lock(hashtext('veho-test-kek'))",
		"update veho.endpoints set credential_ref = null where credential_ref like 'db://%'",
		"delete from veho.secrets", "delete from veho.kek",
	} {
		if _, err := conn.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if err := secrets.InitKEK(ctx, conn, secrets.FileSource{Path: key}); err != nil {
		t.Fatal(err)
	}
	ring, err := secrets.LoadKeyring(ctx, f.worker, secrets.FileSource{Path: key})
	if err != nil {
		t.Fatal(err)
	}
	f.secrets = secrets.NewStore(ring, f.api, f.worker, f.audit)
	drivers := map[string]plugin.Driver{"file": file.New(), "s3": s3.New(), "flaky": f.flaky}
	if f.e, err = New(ctx, Deps{Config: testConfig(t), API: f.api, Worker: f.worker, DBOSURL: dsn(t, "VEHO_TEST_DBOS_DATABASE_URL"),
		Version: "test", Audit: f.audit, Secrets: f.secrets, Drivers: drivers}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.e.Shutdown(5 * time.Second) })
	for _, q := range []string{"select veho.put_location('lab-a', 'region', null)", "select veho.put_location('lab-b', 'region', null)"} {
		if _, err := f.api.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.api.QueryRow(ctx, "select veho.create_tenant($1)", "test-"+uuid.New().String()).Scan(&f.tenant); err != nil {
		t.Fatal(err)
	}
	// the tests retry every 100ms: below the default floor of FR-12's bounds
	if _, err := f.migrator.Exec(ctx, `update veho.tenants set quotas = '{"min_retry_initial": "50ms"}' where id = $1`, f.tenant); err != nil {
		t.Fatal(err)
	}
	if err := store.InTenant(ctx, f.api, f.tenant, func(tx pgx.Tx) error {
		for name, loc := range map[string]string{"src": "lab-a", "d1": "lab-a", "d2": "lab-b"} {
			if _, err := tx.Exec(ctx, "insert into veho.endpoints (tenant_id, name, location_id, driver) values ($1, $2, $3, 'file')", f.tenant, name, loc); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return f
}

const twoCopies = `{"destinations": [{"endpoint": "d1", "path": "out/a"}, {"endpoint": "d2", "path": "out/a"}]}`

func (f fixture) workflows(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.api.QueryRow(context.Background(), "select count(*) from dbos.workflow_status where name = $1", WorkflowJob).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// cancel keeps the test's workflows from running in a later test's engine.
func (f fixture) cancel(t *testing.T, job uuid.UUID) {
	t.Cleanup(func() { _ = dbos.CancelWorkflow(f.e.dbos, JobWorkflowID(job)) })
}

// ENG-1
func TestSubmit(t *testing.T) {
	f := setup(t)
	ctx := audit.WithRequest(context.Background(), audit.Request{Principal: "alice"})
	job, created, err := f.e.Submit(ctx, f.tenant, Request{IdempotencyKey: "k1", SourceEndpoint: "src", SourcePath: "in/a", Policy: []byte(twoCopies)})
	if err != nil || !created {
		t.Fatalf("submit: %v, %v", created, err)
	}
	f.cancel(t, job)
	var state string
	var transfers int
	if err := store.InTenant(ctx, f.api, f.tenant, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, "select state from veho.jobs where id = $1", job).Scan(&state); err != nil {
			return err
		}
		return tx.QueryRow(ctx, "select count(*) from veho.transfers where job_id = $1", job).Scan(&transfers)
	}); err != nil || state != "ACCEPTED" || transfers != 2 {
		t.Errorf("job %s, %d transfers, %v", state, transfers, err)
	}
	var name, queue, status string
	if err := f.api.QueryRow(ctx, "select name, queue_name, status from dbos.workflow_status where workflow_uuid = $1", JobWorkflowID(job)).Scan(&name, &queue, &status); err != nil ||
		name != WorkflowJob || queue != QueueJobs || status != "ENQUEUED" {
		t.Errorf("workflow: %s %s %s, %v", name, queue, status, err)
	}

	// FR-2: the same key gives the same job, and no new workflow
	before := f.workflows(t)
	again, created, err := f.e.Submit(ctx, f.tenant, Request{IdempotencyKey: "k1", SourceEndpoint: "src", SourcePath: "in/a", Policy: []byte(twoCopies)})
	if err != nil || created || again != job || f.workflows(t) != before {
		t.Errorf("resubmit: %s %v %v", again, created, err)
	}
	if ev := f.audit.Events(); len(ev) != 1 || ev[0].Action != "JOB_SUBMIT" || ev[0].Principal != "alice" || ev[0].JobID != job.String() {
		t.Errorf("audit: %+v", ev)
	}

	// INV-1: a failure after the inserts leaves neither rows nor workflow
	f.e.beforeCommit = func() error { return errors.New("injected") }
	if _, _, err := f.e.Submit(ctx, f.tenant, Request{IdempotencyKey: "k2", SourceEndpoint: "src", SourcePath: "in/b", Policy: []byte(twoCopies)}); err == nil {
		t.Fatal("injected failure: no error")
	}
	f.e.beforeCommit = nil
	var n int
	if err := store.InTenant(ctx, f.api, f.tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "select count(*) from veho.jobs where idempotency_key = 'k2'").Scan(&n)
	}); err != nil || n != 0 || f.workflows(t) != before {
		t.Errorf("after a rollback: %d jobs, %d workflows (was %d), %v", n, f.workflows(t), before, err)
	}
}

func TestSubmitRefused(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	if _, _, err := f.e.Submit(ctx, f.tenant, Request{SourceEndpoint: "nope", SourcePath: "a", Policy: []byte(twoCopies)}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown source: %v", err)
	}
	if _, _, err := f.e.Submit(ctx, uuid.New(), Request{SourceEndpoint: "src", SourcePath: "a", Policy: []byte(twoCopies)}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown tenant: %v", err)
	}
	for _, p := range []string{`{"destinations": [{"endpoint": "d1", "path": "x"}, {"endpoint": "other", "path": "x"}]}`, `{"nope": 1}`} {
		if _, _, err := f.e.Submit(ctx, f.tenant, Request{SourceEndpoint: "src", SourcePath: "a", Policy: []byte(p)}); err == nil {
			t.Errorf("%s: no error", p)
		} else if _, ok := errors.AsType[*policy.Error](err); !ok {
			t.Errorf("%s: %v, want a policy error", p, err)
		}
	}
	// FR-12: with the default bounds (1s to 1h), a backoff outside them is refused
	if _, err := f.migrator.Exec(ctx, `update veho.tenants set quotas = '{}' where id = $1`, f.tenant); err != nil {
		t.Fatal(err)
	}
	for _, retry := range []string{`{"initial": "100ms"}`, `{"max_interval": "2h"}`} {
		p := `{"destinations": [{"endpoint": "d1", "path": "x"}], "retry": ` + retry + `}`
		if _, _, err := f.e.Submit(ctx, f.tenant, Request{SourceEndpoint: "src", SourcePath: "a", Policy: []byte(p)}); err == nil {
			t.Errorf("retry %s: accepted", retry)
		} else if pe, ok := errors.AsType[*policy.Error](err); !ok || pe.Code != policy.CodeInvalidPolicy {
			t.Errorf("retry %s: %v, want invalid_policy", retry, err)
		}
	}
	if _, err := f.migrator.Exec(ctx, `update veho.tenants set quotas = '{"max_active_jobs": 1}' where id = $1`, f.tenant); err != nil {
		t.Fatal(err)
	}
	job, _, err := f.e.Submit(ctx, f.tenant, Request{SourceEndpoint: "src", SourcePath: "a", Policy: []byte(twoCopies)})
	if err != nil {
		t.Fatal(err)
	}
	f.cancel(t, job)
	if _, _, err := f.e.Submit(ctx, f.tenant, Request{SourceEndpoint: "src", SourcePath: "b", Policy: []byte(twoCopies)}); !errors.Is(err, ErrQuota) {
		t.Errorf("over the quota: %v", err)
	}
}

// §7: the heartbeat records the executor; without it the node fences
// itself after half the lease, and recovers with the next good beat.
func TestHeartbeat(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	if err := f.e.Heartbeat(ctx); err != nil {
		t.Fatal(err)
	}
	var age float64
	if err := f.worker.QueryRow(ctx, "select extract(epoch from now() - last_seen) from veho.executors where id = $1", f.e.executor).Scan(&age); err != nil || age > 5 {
		t.Errorf("last_seen %vs ago, %v", age, err)
	}

	dead, err := store.Open(ctx, dsn(t, "VEHO_TEST_WORKER_DATABASE_URL"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	dead.Close()
	worker := f.e.worker
	f.e.worker = dead
	now := time.Now()
	f.e.beat(ctx, now.Add(time.Second)) // within half the lease: not fenced
	if f.e.Fenced() {
		t.Fatal("fenced after one failure")
	}
	f.e.beat(ctx, now.Add(3*time.Second))
	if !f.e.Fenced() {
		t.Fatal("not fenced after half the lease")
	}
	f.e.worker = worker
	f.e.beat(ctx, now.Add(4*time.Second))
	if f.e.Fenced() {
		t.Error("still fenced after a good beat")
	}
}

// FR-12: a tenant's bounds must keep the policy defaults (2s, 15m) valid.
func TestRetryBounds(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	for _, tc := range []struct {
		q  Quotas
		ok bool
	}{
		{Quotas{MinRetryInitial: policy.Duration(500 * time.Millisecond), MaxRetryInterval: policy.Duration(2 * time.Hour)}, true},
		{Quotas{MinRetryInitial: policy.Duration(2 * time.Second), MaxRetryInterval: policy.Duration(15 * time.Minute)}, true},
		{Quotas{MinRetryInitial: policy.Duration(5 * time.Second)}, false},
		{Quotas{MaxRetryInterval: policy.Duration(10 * time.Minute)}, false},
		{Quotas{MinRetryInitial: policy.Duration(-time.Second)}, false},
	} {
		got, err := f.e.UpdateTenant(ctx, f.tenant, nil, &tc.q)
		switch {
		case tc.ok && (err != nil || got.Quotas != tc.q):
			t.Errorf("%+v: %+v, %v", tc.q, got.Quotas, err)
		case !tc.ok && !errors.Is(err, ErrInvalid):
			t.Errorf("%+v: %v, want ErrInvalid", tc.q, err)
		}
	}
}

// EXT-5, AC-20: DBOS recovers and dequeues only the workflows of its own
// application version, so a node runs WorkflowVersion, not its build's
// version: a new build of compatible workflows must pick up the old build's
// work.
func TestWorkflowVersion(t *testing.T) {
	f := setup(t)
	e, err := New(context.Background(), Deps{Config: testConfig(t), API: f.api, Worker: f.worker, DBOSURL: dsn(t, "VEHO_TEST_DBOS_DATABASE_URL"),
		Audit: &audit.Recorder{}, Secrets: f.secrets})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = e.Shutdown(time.Second) }()
	if v := e.dbos.GetApplicationVersion(); v != WorkflowVersion {
		t.Errorf("application version %q, want WorkflowVersion %q", v, WorkflowVersion)
	}
}
