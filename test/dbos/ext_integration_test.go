//go:build integration

// Package dbos is the regression suite of the DBOS behaviours veho relies
// on (AC-21, .specs/veho-dbos-spike.md EXT-1 to EXT-6), promoted from the
// spike. Run it on every DBOS version bump: test/lab/lab.sh test ./test/dbos.
// The tests start real executors by running this test binary again as a
// worker process, so that they can stop and kill them.
package dbos

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"uuid"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/dihedron/veho/test/labtest"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	app     = "veho-ext"
	version = "ext-v1"
	qParts  = "ext-parts"
	qFair   = "ext-fair"
)

// Job is a workflow of Parts steps of SleepMs each; every step logs its
// begin and end to the events file.
type Job struct {
	ID      string
	Parts   int
	SleepMs int
}

var (
	executor string
	events   string
	logMu    sync.Mutex
)

func event(format string, a ...any) {
	logMu.Lock()
	defer logMu.Unlock()
	f, err := os.OpenFile(events, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(f, "%s %s\n", executor, fmt.Sprintf(format, a...))
	_ = f.Sync()
	_ = f.Close()
}

// Parts is the workflow the workers run.
func Parts(ctx dbos.Context, in Job) (string, error) {
	for i := range in.Parts {
		if _, err := dbos.RunAsStep(ctx, func(context.Context) (int, error) {
			event("%s part %d begin", in.ID, i)
			time.Sleep(time.Duration(in.SleepMs) * time.Millisecond)
			event("%s part %d end", in.ID, i)
			return i, nil
		}, dbos.WithStepName(fmt.Sprintf("part-%d", i))); err != nil {
			return "", err
		}
	}
	return "ok", nil
}

var quiet = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

func register(ctx dbos.Context) error {
	dbos.RegisterWorkflow(ctx, Parts, dbos.WithWorkflowName("Parts"))
	if _, err := dbos.RegisterQueue(ctx, qParts); err != nil {
		return err
	}
	_, err := dbos.RegisterQueue(ctx, qFair, dbos.WithPartitionConcurrency(1))
	return err
}

// TestMain runs a worker when the environment asks for one.
func TestMain(m *testing.M) {
	if id := os.Getenv("EXT_WORKER"); id != "" {
		os.Exit(worker(id))
	}
	os.Exit(m.Run())
}

// worker runs an executor until SIGTERM; with EXT_ADOPT it first adopts
// that executor's PENDING workflows (EXT-2b).
func worker(id string) int {
	executor, events = id, os.Getenv("EXT_EVENTS")
	ctx, err := dbos.NewContext(context.Background(), dbos.Config{AppName: app, DatabaseURL: os.Getenv("VEHO_TEST_DBOS_DATABASE_URL"),
		ExecutorID: id, ApplicationVersion: version, Logger: quiet})
	if err == nil {
		err = register(ctx)
	}
	if err == nil {
		err = dbos.Launch(ctx)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "worker:", err)
		return 1
	}
	event("launched")
	if dead := os.Getenv("EXT_ADOPT"); dead != "" {
		ws, err := dbos.ListWorkflows(ctx, dbos.WithFilterExecutorIDs(dead), dbos.WithFilterStatus(dbos.WorkflowStatusPending))
		ids := make([]string, len(ws))
		for i, w := range ws {
			ids[i] = w.ID
		}
		if err == nil && len(ids) > 0 {
			_, err = dbos.ResumeWorkflows[string](ctx, ids)
		}
		event("adopted %d %v", len(ids), err)
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM)
	<-sig
	_ = dbos.Shutdown(ctx, 5*time.Second)
	return 0
}

type suite struct {
	t      *testing.T
	dsn    string
	events string
	client dbos.Client
	pool   *pgxpool.Pool
}

func newSuite(t *testing.T) *suite {
	t.Helper()
	dsn := os.Getenv("VEHO_TEST_DBOS_DATABASE_URL")
	if dsn == "" {
		t.Skip("VEHO_TEST_DBOS_DATABASE_URL is not set: run with test/lab/lab.sh test")
	}
	ctx := context.Background()
	c, err := dbos.NewClient(ctx, dbos.ClientConfig{DatabaseURL: dsn, AppName: app, Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dbos.Shutdown(c, 5*time.Second) })
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return &suite{t: t, dsn: dsn, events: t.TempDir() + "/events.log", client: c, pool: pool}
}

// start runs a worker process; adopt names an executor to adopt.
func (s *suite) start(id, adopt string) *exec.Cmd {
	s.t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$") //nolint:gosec // this test binary
	cmd.Env = append(os.Environ(), "EXT_WORKER="+id, "EXT_EVENTS="+s.events, "EXT_ADOPT="+adopt)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		s.t.Fatal(err)
	}
	s.t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGCONT)
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	s.waitFor(id + " launched")
	return cmd
}

func (s *suite) lines() []string {
	data, _ := os.ReadFile(s.events)
	var out []string
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	for sc.Scan() {
		out = append(out, sc.Text())
	}
	return out
}

func (s *suite) count(substr string) int {
	n := 0
	for _, l := range s.lines() {
		if strings.Contains(l, substr) {
			n++
		}
	}
	return n
}

func (s *suite) waitFor(line string) {
	s.t.Helper()
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if s.count(line) > 0 {
			return
		}
	}
	s.t.Fatalf("no %q in the events:\n%s", line, strings.Join(s.lines(), "\n"))
}

func (s *suite) enqueue(queue string, j Job, partition string) dbos.WorkflowHandle[string] {
	s.t.Helper()
	opts := []dbos.EnqueueOption{dbos.WithEnqueueWorkflowID(j.ID), dbos.WithEnqueueApplicationVersion(version)}
	if partition != "" {
		opts = append(opts, dbos.WithEnqueueQueuePartitionKey(partition))
	}
	h, err := dbos.Enqueue[string](s.client, queue, "Parts", j, opts...)
	if err != nil {
		s.t.Fatal(err)
	}
	return h
}

func (s *suite) result(h dbos.WorkflowHandle[string]) {
	s.t.Helper()
	if r, err := h.GetResult(dbos.WithHandleTimeout(120 * time.Second)); err != nil || r != "ok" {
		s.t.Fatalf("%s: %q, %v\n%s", h.GetWorkflowID(), r, err, strings.Join(s.lines(), "\n"))
	}
}

func id(name string) string { return "ext-" + name + "-" + uuid.New().String()[:8] }

// EXT-1: the enqueue commits or rolls back with the caller's transaction.
// EXT-1b: a second enqueue of the same ID runs nothing more.
func TestTransactionalEnqueue(t *testing.T) {
	s := newSuite(t)
	ctx := context.Background()
	if _, err := s.pool.Exec(ctx, "create table if not exists dbos.ext_suite_rows (id text primary key)"); err != nil {
		t.Fatal(err)
	}
	for _, commit := range []bool{false, true} {
		wf := id("tx")
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, "insert into dbos.ext_suite_rows values ($1)", wf); err != nil {
			t.Fatal(err)
		}
		if _, err := dbos.Enqueue[string](s.client, qParts, "Parts", Job{ID: wf, Parts: 1},
			dbos.WithEnqueueWorkflowID(wf), dbos.WithEnqueueApplicationVersion(version), dbos.WithEnqueueTransaction(tx)); err != nil {
			t.Fatal(err)
		}
		if commit {
			err = tx.Commit(ctx)
		} else {
			err = tx.Rollback(ctx)
		}
		if err != nil {
			t.Fatal(err)
		}
		var rows, wfs int
		if err := s.pool.QueryRow(ctx, "select (select count(*) from dbos.ext_suite_rows where id = $1), (select count(*) from dbos.workflow_status where workflow_uuid = $1)", wf).Scan(&rows, &wfs); err != nil {
			t.Fatal(err)
		}
		want := 0
		if commit {
			want = 1
		}
		if rows != want || wfs != want {
			t.Errorf("commit %v: %d rows, %d workflows, want %d", commit, rows, wfs, want)
		}
	}
	s.start(id("w"), "")
	wf := id("dup")
	h := s.enqueue(qParts, Job{ID: wf, Parts: 1}, "")
	s.enqueue(qParts, Job{ID: wf, Parts: 1}, "")
	s.result(h)
	time.Sleep(time.Second)
	if n := s.count(wf + " part 0 begin"); n != 1 {
		t.Errorf("EXT-1b: the workflow ran %d times", n)
	}
}

// EXT-2b, EXT-2c: an executor stopped mid-part has its PENDING workflow
// adopted by another, which runs only the parts left; the stopped one,
// continued, finishes its in-flight part and starts no other.
func TestAdoptionAndZombie(t *testing.T) {
	s := newSuite(t)
	a := id("a")
	pa := s.start(a, "")
	wf := id("adopt")
	h := s.enqueue(qParts, Job{ID: wf, Parts: 6, SleepMs: 400}, "")
	s.waitFor(a + " " + wf + " part 2 begin")
	if err := pa.Process.Signal(syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	b := id("b")
	s.start(b, a)
	s.waitFor(b + " adopted 1")
	s.result(h)
	if err := pa.Process.Signal(syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Second)
	for i := range 6 {
		if s.count(fmt.Sprintf("%s part %d end", wf, i)) == 0 {
			t.Errorf("part %d never ended", i)
		}
	}
	if n := s.count(b + " " + wf + " part 0 begin"); n != 0 {
		t.Errorf("the adopter ran part 0 again (%d)", n)
	}
	begins := s.count(wf + " part")
	if extra := s.count(a+" "+wf+" part 3 begin") + s.count(a+" "+wf+" part 4 begin"); extra != 0 {
		t.Errorf("the zombie started %d parts after it was adopted", extra)
	}
	if begins > 2*(6+1) {
		t.Errorf("%d part events for 6 parts", begins)
	}
}

// EXT-2d: ResumeWorkflows resets DBOS's recovery counter, so veho counts
// adoptions itself.
func TestResumeResetsRecoveryAttempts(t *testing.T) {
	s := newSuite(t)
	ctx := context.Background()
	wf := id("attempts")
	s.enqueue(qParts, Job{ID: wf, Parts: 1}, "")
	if _, err := s.pool.Exec(ctx, "update dbos.workflow_status set status = 'PENDING', recovery_attempts = 50 where workflow_uuid = $1", wf); err != nil {
		t.Fatal(err)
	}
	if _, err := dbos.ResumeWorkflows[string](s.client, []string{wf}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.pool.QueryRow(ctx, "select recovery_attempts from dbos.workflow_status where workflow_uuid = $1", wf).Scan(&n); err != nil || n >= 50 {
		t.Errorf("recovery_attempts %d after resume, %v: veho's own counter is no longer needed, or the reset moved", n, err)
	}
	_ = dbos.CancelWorkflow(s.client, wf)
}

// EXT-3a: a partitioned queue with partition concurrency 1 runs one
// tenant's workflows one at a time, and another tenant's meanwhile.
func TestPartitionFairness(t *testing.T) {
	s := newSuite(t)
	s.start(id("fair"), "")
	x1, x2, y1 := id("x1"), id("x2"), id("y1")
	hs := []dbos.WorkflowHandle[string]{
		s.enqueue(qFair, Job{ID: x1, Parts: 2, SleepMs: 400}, "tenant-x"),
		s.enqueue(qFair, Job{ID: x2, Parts: 2, SleepMs: 400}, "tenant-x"),
		s.enqueue(qFair, Job{ID: y1, Parts: 2, SleepMs: 400}, "tenant-y"),
	}
	for _, h := range hs {
		s.result(h)
	}
	at := func(line string) int {
		for i, l := range s.lines() {
			if strings.Contains(l, line) {
				return i
			}
		}
		return -1
	}
	if at(x2+" part 0 begin") < at(x1+" part 1 end") {
		t.Error("two workflows of tenant-x ran at once")
	}
	if at(y1+" part 0 begin") > at(x1+" part 1 end") {
		t.Error("tenant-y waited for tenant-x")
	}
}

// EXT-5: an executor runs only the workflows of its application version.
func TestVersionFilter(t *testing.T) {
	s := newSuite(t)
	s.start(id("v"), "")
	wf := id("other-version")
	if _, err := dbos.Enqueue[string](s.client, qParts, "Parts", Job{ID: wf, Parts: 1},
		dbos.WithEnqueueWorkflowID(wf), dbos.WithEnqueueApplicationVersion("ext-v2")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Second)
	if n := s.count(wf); n != 0 {
		t.Errorf("a v1 executor ran a v2 workflow")
	}
	_ = dbos.CancelWorkflow(s.client, wf)
}

// Replay: a restarted executor resumes its PENDING workflow after the last
// completed step, without running the completed ones again (what the
// engine's steps rely on in place of EXT-6's step retry).
func TestReplay(t *testing.T) {
	s := newSuite(t)
	w := id("r")
	p := s.start(w, "")
	wf := id("replay")
	h := s.enqueue(qParts, Job{ID: wf, Parts: 4, SleepMs: 400}, "")
	s.waitFor(w + " " + wf + " part 2 begin")
	_ = p.Process.Kill()
	_ = p.Wait()
	s.start(w, "") // the same executor ID recovers its own work at launch
	s.result(h)
	for i := range 2 {
		if n := s.count(fmt.Sprintf("%s part %d begin", wf, i)); n != 1 {
			t.Errorf("part %d began %d times", i, n)
		}
	}
}

// EXT-4a: a failover of the primary mid-workflow: the executor reconnects
// and every part runs once.
func TestFailover(t *testing.T) {
	s := newSuite(t)
	s.start(id("f"), "")
	wf := id("failover")
	h := s.enqueue(qParts, Job{ID: wf, Parts: 12, SleepMs: 300}, "")
	s.waitFor(wf + " part 2 end")
	labtest.Switchover(t)
	s.result(h)
	for i := range 12 {
		if n := s.count(fmt.Sprintf("%s part %d end", wf, i)); n != 1 {
			t.Errorf("part %d ended %d times", i, n)
		}
	}
}
