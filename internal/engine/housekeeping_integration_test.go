//go:build integration

package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/dihedron/veho/internal/audit"
	"github.com/dihedron/veho/internal/store"
	"github.com/dihedron/veho/pkg/plugin"
	"github.com/dihedron/veho/pkg/plugin/file"
	"github.com/dihedron/veho/pkg/plugin/s3"
	"github.com/dihedron/veho/test/labtest"
	"github.com/jackc/pgx/v5"
)

func (f fixture) housekeep(t *testing.T) {
	t.Helper()
	locked, err := f.e.Housekeep(context.Background())
	if err != nil || !locked {
		t.Fatalf("housekeeping: %v, %v", locked, err)
	}
}

func (f fixture) sql(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := f.migrator.Exec(context.Background(), q, args...); err != nil {
		t.Fatal(err)
	}
}

func (f fixture) events(action string) int {
	n := 0
	for _, e := range f.audit.Events() {
		if e.Action == action {
			n++
		}
	}
	return n
}

// jobEvents counts the events of action about job: the lab's database
// holds other tests' jobs, which housekeeping repairs too.
func (f fixture) jobEvents(action string, job uuid.UUID) int {
	n := 0
	for _, e := range f.audit.Events() {
		if d, ok := e.Details.(map[string]string); ok && e.Action == action && d["job"] == job.String() {
			n++
		}
	}
	return n
}

// ENG-6, ENG-7 in one process: a second executor blocks mid-batch and stops
// beating; housekeeping on the first adopts its workflows, which finish
// with the right bytes; the blocked step, released, writes nothing (INV-3).
func TestAdoption(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	srcDir := f.fileEndpoint(t, "source", "file", "lab-a")
	a := f.fileEndpoint(t, "a", "flaky", "lab-a")
	f.fileEndpoint(t, "b", "file", "lab-b")

	other, err := New(ctx, Deps{Config: testConfig(t), API: f.api, Worker: f.worker, DBOSURL: dsn(t, "VEHO_TEST_DBOS_DATABASE_URL"),
		Version: "test", Audit: &audit.Recorder{}, Secrets: f.secrets,
		Drivers: map[string]plugin.Driver{"file": file.New(), "s3": s3.New(), "flaky": f.flaky}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Shutdown(5 * time.Second) })
	if err := other.Heartbeat(ctx); err != nil {
		t.Fatal(err)
	}
	blocked, release := make(chan struct{}), make(chan struct{})
	var once bool
	f.flaky.set(func(int) error {
		if !once {
			once = true
			close(blocked)
			<-release
		}
		return nil
	})
	if err := other.Launch(); err != nil {
		t.Fatal(err)
	}
	job, name := f.submitRun(t, srcDir, 12*mib, destPolicy(2, "", "a", "b"))
	<-blocked // the other executor is in a's first batch
	f.sql(t, "update veho.executors set last_seen = now() - interval '1 hour' where id = $1", other.executor)
	f.launch(t)
	f.housekeep(t)
	if f.events("SYSTEM_ZOMBIE_ADOPTED") == 0 { // other runs' dead executors may be adopted too
		t.Errorf("%d adoption events", f.events("SYSTEM_ZOMBIE_ADOPTED"))
	}
	close(release) // the zombie's step goes on, and must record nothing
	s := f.wait(t, job, func(s JobStatus) bool { return s.State == "SUCCEEDED" && s.SettledAt != nil })
	if s.Copies != 2 {
		t.Errorf("job: %+v", s)
	}
	same(t, filepath.Join(srcDir, name), filepath.Join(a, "copy", name))
	var adoptions int
	if err := store.InTenant(ctx, f.api, f.tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "select max(adoptions) from veho.transfers where job_id = $1", job).Scan(&adoptions)
	}); err != nil || adoptions != 1 {
		t.Errorf("adoptions %d, %v", adoptions, err)
	}
}

// A node stopped by its signal (the context given to New is cancelled, then
// Shutdown) leaves its running workflows PENDING for adoption: a cancelled
// context must not cancel them durably, which would fail their transfers
// (found by AC-4 in the lab).
func TestStopLeavesWorkPending(t *testing.T) {
	f := setup(t)
	srcDir := f.fileEndpoint(t, "source", "file", "lab-a")
	f.fileEndpoint(t, "a", "flaky", "lab-a")
	f.fileEndpoint(t, "b", "file", "lab-b")
	sig, stop := context.WithCancel(context.Background())
	defer stop()
	other, err := New(sig, Deps{Config: testConfig(t), API: f.api, Worker: f.worker, DBOSURL: dsn(t, "VEHO_TEST_DBOS_DATABASE_URL"),
		Version: "test", Audit: &audit.Recorder{}, Secrets: f.secrets,
		Drivers: map[string]plugin.Driver{"file": file.New(), "s3": s3.New(), "flaky": f.flaky}})
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Heartbeat(sig); err != nil {
		t.Fatal(err)
	}
	blocked := make(chan struct{})
	var once sync.Once
	f.flaky.set(func(int) error {
		once.Do(func() { close(blocked) })
		time.Sleep(200 * time.Millisecond)
		return nil
	})
	if err := other.Launch(); err != nil {
		t.Fatal(err)
	}
	job, _ := f.submitRun(t, srcDir, 12*mib, destPolicy(2, "", "a", "b"))
	<-blocked // a transfer runs on the other node
	stop()    // SIGTERM
	_ = other.Shutdown(time.Second)
	var cancelled int
	if err := f.api.QueryRow(context.Background(), "select count(*) from dbos.workflow_status where workflow_uuid like 'transfer/%' and status = 'CANCELLED' and executor_id = $1",
		other.executor).Scan(&cancelled); err != nil {
		t.Fatal(err)
	}
	if cancelled != 0 {
		t.Errorf("%d transfer workflows cancelled by the stop, want 0", cancelled)
	}
	_ = job
}

// ENG-12: a job past its deadline is FAILED and its transfers cancelled.
func TestDeadline(t *testing.T) {
	f := setup(t)
	srcDir := f.fileEndpoint(t, "source", "file", "lab-a")
	f.fileEndpoint(t, "a", "flaky", "lab-a")
	f.fileEndpoint(t, "b", "file", "lab-b")
	f.flaky.set(func(int) error { return plugin.Wrapf(plugin.Transient, "throttled") })
	f.launch(t)
	job, _ := f.submitRun(t, srcDir, 1*mib, destPolicy(2, "", "a", "b"))
	f.wait(t, job, func(s JobStatus) bool {
		for _, tr := range s.Transfers {
			if tr.Endpoint == "a" && tr.State == "RETRY_WAIT" {
				return true
			}
		}
		return false
	})
	f.sql(t, "update veho.jobs set deadline_at = now() - interval '1 second' where id = $1", job)
	f.housekeep(t)
	s := f.wait(t, job, isSettled)
	if s.State != "FAILED" || f.jobEvents("SYSTEM_JOB_EXPIRED", job) != 1 {
		t.Errorf("job: %+v", s)
	}
	for _, tr := range s.Transfers {
		if tr.Endpoint == "a" && tr.State != "CANCELLED" {
			t.Errorf("transfer: %+v", tr)
		}
	}
}

// ENG-13: a job whose workflow DBOS lost is started again; one whose
// workflow ended without marking it is repaired; retention removes only
// settled jobs.
func TestZombies(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	srcDir := f.fileEndpoint(t, "source", "file", "lab-a")
	f.fileEndpoint(t, "a", "file", "lab-a")
	f.fileEndpoint(t, "b", "file", "lab-b")

	lost, _ := f.submitRun(t, srcDir, 1*mib, destPolicy(2, "", "a", "b"))
	if err := dbos.DeleteWorkflows(f.e.dbos, []string{JobWorkflowID(lost)}); err != nil {
		t.Fatal(err)
	}
	ended, _ := f.submitRun(t, srcDir, 1*mib, destPolicy(2, "", "a", "b"))
	if err := dbos.CancelWorkflow(f.e.dbos, JobWorkflowID(ended)); err != nil {
		t.Fatal(err)
	}
	f.sql(t, "update veho.jobs set updated_at = now() - interval '1 hour' where id = any($1)", []uuid.UUID{lost, ended})
	f.launch(t)
	f.housekeep(t)
	if s := f.wait(t, lost, isSettled); s.State != "SUCCEEDED" || f.jobEvents("SYSTEM_ZOMBIE_RESTARTED", lost) != 1 {
		t.Errorf("lost workflow: %+v", s)
	}
	if s := f.wait(t, ended, isSettled); s.State != "FAILED" || s.Transfers[0].State != "FAILED" || f.jobEvents("SYSTEM_ZOMBIE_REPAIRED", ended) != 1 {
		t.Errorf("ended workflow: %+v", s)
	}

	// retention: the settled job, settled long ago, goes; nothing else
	f.sql(t, "update veho.jobs set settled_at = now() - interval '100 days' where id = $1", lost)
	f.housekeep(t)
	if _, err := f.e.Status(ctx, f.tenant, lost); err == nil {
		t.Error("the old settled job is still there")
	}
	if _, err := f.e.Status(ctx, f.tenant, ended); err != nil {
		t.Errorf("a recent job was removed: %v", err)
	}
}

// AC-19: a poison pill, a workflow DBOS parked as
// MAX_RECOVERY_ATTEMPTS_EXCEEDED, is found by housekeeping: its transfers end
// FAILED with class Internal, the job is decided, the repair audited.
func TestPoisonPill(t *testing.T) {
	f := setup(t)
	srcDir := f.fileEndpoint(t, "source", "file", "lab-a")
	f.fileEndpoint(t, "a", "file", "lab-a")
	f.fileEndpoint(t, "b", "file", "lab-b")
	job, _ := f.submitRun(t, srcDir, 1*mib, destPolicy(2, "", "a", "b"))
	// as DBOS does after too many recoveries; only veho_dbos may write its tables
	dbosConn, err := pgx.Connect(context.Background(), os.Getenv("VEHO_TEST_DBOS_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dbosConn.Close(context.Background()) }()
	if _, err := dbosConn.Exec(context.Background(), "update dbos.workflow_status set status = 'MAX_RECOVERY_ATTEMPTS_EXCEEDED' where workflow_uuid like $1",
		JobWorkflowID(job)+"%"); err != nil {
		t.Fatal(err)
	}
	f.sql(t, "update veho.jobs set updated_at = now() - interval '1 hour' where id = $1", job)
	f.launch(t)
	f.housekeep(t)
	s := f.wait(t, job, isSettled)
	if s.State != "FAILED" || f.jobEvents("SYSTEM_ZOMBIE_REPAIRED", job) != 1 {
		t.Fatalf("poison pill: %+v", s)
	}
	for _, tr := range s.Transfers {
		var e struct{ Class string }
		_ = json.Unmarshal(tr.LastError, &e)
		if tr.State != "FAILED" || e.Class != "Internal" {
			t.Errorf("transfer %s: %s, class %q, want FAILED, Internal", tr.Endpoint, tr.State, e.Class)
		}
	}
}

// ENG-15: the primary fails over in the middle of a transfer: the job
// succeeds, every part is recorded once, the bytes are the source's.
func TestFailoverMidTransfer(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	srcDir := f.fileEndpoint(t, "source", "file", "lab-a")
	slow := f.fileEndpoint(t, "a", "flaky", "lab-a")
	f.fileEndpoint(t, "b", "file", "lab-b")
	f.flaky.set(func(int) error { time.Sleep(300 * time.Millisecond); return nil })
	f.launch(t)
	job, name := f.submitRun(t, srcDir, 40*mib, destPolicy(2, "", "a", "b"))
	f.wait(t, job, func(s JobStatus) bool {
		for _, tr := range s.Transfers {
			if tr.Endpoint == "a" && tr.BytesDone > 0 {
				return true
			}
		}
		return false
	})
	labtest.Switchover(t)
	s := f.wait(t, job, isSettled)
	if s.State != "SUCCEEDED" {
		t.Fatalf("job: %+v", s)
	}
	same(t, filepath.Join(srcDir, name), filepath.Join(slow, "copy", name))
	var parts, distinct int
	if err := store.InTenant(ctx, f.api, f.tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `select count(*), count(distinct (transfer_id, part_no)) from veho.parts p join veho.transfers t on t.id = p.transfer_id
			where t.job_id = $1`, job).Scan(&parts, &distinct)
	}); err != nil || parts != 16 || distinct != 16 {
		t.Errorf("%d parts (%d distinct), want 16: %v", parts, distinct, err)
	}
}

// A node's own PENDING workflow that it no longer runs (DBOS could not record
// its outcome during a failover, so the execution ended and the row stayed
// PENDING) is resumed by the node's watchdog: adoption covers dead nodes
// only, and DBOS recovers a node's own work only at start (found by AC-5).
func TestRecoverOwn(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	srcDir := f.fileEndpoint(t, "source", "file", "lab-a")
	f.fileEndpoint(t, "a", "flaky", "lab-a")
	f.fileEndpoint(t, "b", "file", "lab-b")
	cfg := testConfig(t)
	cfg.Node.ID = f.e.executor // the same node, in its previous life
	other, err := New(ctx, Deps{Config: cfg, API: f.api, Worker: f.worker, DBOSURL: dsn(t, "VEHO_TEST_DBOS_DATABASE_URL"),
		Version: "test", Audit: &audit.Recorder{}, Secrets: f.secrets,
		Drivers: map[string]plugin.Driver{"file": file.New(), "s3": s3.New(), "flaky": f.flaky}})
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Heartbeat(ctx); err != nil {
		t.Fatal(err)
	}
	blocked := make(chan struct{})
	var once sync.Once
	f.flaky.set(func(int) error {
		once.Do(func() { close(blocked) })
		time.Sleep(300 * time.Millisecond)
		return nil
	})
	if err := other.Launch(); err != nil {
		t.Fatal(err)
	}
	job, _ := f.submitRun(t, srcDir, 12*mib, destPolicy(2, "", "a", "b"))
	<-blocked
	_ = other.Shutdown(time.Second) // its workflows stay PENDING, run by nobody
	dbosConn, err := pgx.Connect(ctx, os.Getenv("VEHO_TEST_DBOS_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dbosConn.Close(ctx) }()
	if _, err := dbosConn.Exec(ctx, "update dbos.workflow_status set updated_at = updated_at - 120000 where executor_id = $1 and status = 'PENDING'", f.e.executor); err != nil {
		t.Fatal(err)
	}
	// before Launch, whose own recovery would pick them up: the watchdog's
	// case is a node that runs on, with a workflow nobody executes
	n, err := f.e.recoverOwn(ctx)
	if err != nil || n == 0 {
		t.Fatalf("recovered %d workflows: %v", n, err)
	}
	f.launch(t)
	if s := f.wait(t, job, isSettled); s.State != "SUCCEEDED" {
		t.Fatalf("job: %+v", s)
	}
}
