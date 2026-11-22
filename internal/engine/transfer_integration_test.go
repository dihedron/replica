//go:build integration

package engine

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/dihedron/veho/internal/store"
	"github.com/dihedron/veho/pkg/plugin"
	"github.com/jackc/pgx/v5"
	"golang.org/x/sync/semaphore"
)

// flaky is the file driver with a hook before every WritePart, which can
// fail the part or act meanwhile (a takeover, a change of the source).
type flaky struct {
	plugin.Driver
	mu   sync.Mutex
	hook func(partNo int) error
}

func (f *flaky) Name() string { return "flaky" }

func (f *flaky) set(hook func(int) error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hook = hook
}

func (f *flaky) before(partNo int) error {
	f.mu.Lock()
	h := f.hook
	f.mu.Unlock()
	if h == nil {
		return nil
	}
	return h(partNo)
}

func (f *flaky) Open(ctx context.Context, cfg plugin.Config, creds plugin.Credentials) (plugin.Client, error) {
	c, err := f.Driver.Open(ctx, cfg, creds)
	return flakyClient{Client: c, f: f}, err
}

type flakyClient struct {
	plugin.Client
	f *flaky
}

func (c flakyClient) Begin(ctx context.Context, ref plugin.Ref, info plugin.ObjectInfo, opts plugin.PutOptions) (plugin.Upload, error) {
	u, err := c.Client.Begin(ctx, ref, info, opts)
	return flakyUpload{Upload: u, f: c.f}, err
}

func (c flakyClient) Resume(ctx context.Context, ref plugin.Ref, token []byte) (plugin.Upload, error) {
	u, err := c.Client.Resume(ctx, ref, token)
	return flakyUpload{Upload: u, f: c.f}, err
}

type flakyUpload struct {
	plugin.Upload
	f *flaky
}

func (u flakyUpload) WritePart(ctx context.Context, partNo int, r io.Reader, length int64) (plugin.PartResult, error) {
	if err := u.f.before(partNo); err != nil {
		return plugin.PartResult{}, err
	}
	return u.Upload.WritePart(ctx, partNo, r, length)
}

const mib = 1 << 20

// job is a submitted job whose ReplicateJob is cancelled (C6c runs it):
// the test resolves it and runs its transfers itself.
type job struct {
	f         fixture
	id        uuid.UUID
	src       string               // the source file
	transfers map[string]uuid.UUID // by destination endpoint
}

// endpoint adds an endpoint of the tenant.
func (f fixture) endpoint(t *testing.T, name, driver, loc string, cfg any, credRef string) {
	t.Helper()
	ctx := context.Background()
	c, _ := json.Marshal(cfg)
	var ref *string
	if credRef != "" {
		ref = &credRef
	}
	if err := store.InTenant(ctx, f.api, f.tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "insert into veho.endpoints (tenant_id, name, location_id, driver, config, credential_ref) values ($1, $2, $3, $4, $5, $6)",
			f.tenant, name, loc, driver, c, ref)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// fileEndpoint adds a file endpoint on a new directory and returns it.
func (f fixture) fileEndpoint(t *testing.T, name, driver, loc string) string {
	t.Helper()
	dir := t.TempDir()
	f.endpoint(t, name, driver, loc, map[string]string{"root": dir}, "")
	return dir
}

// submit writes size random bytes as the source and submits a job of the
// policy; it launches the engine on the first call.
func (f fixture) submit(t *testing.T, srcDir string, size int, pol string) job {
	t.Helper()
	ctx := context.Background()
	name := "obj-" + uuid.New().String()[:8]
	data := make([]byte, size)
	_, _ = rand.Read(data)
	if err := os.WriteFile(filepath.Join(srcDir, name), data, 0o600); err != nil {
		t.Fatal(err)
	}
	pol = strings.ReplaceAll(pol, "OBJ", name)
	id, _, err := f.e.Submit(ctx, f.tenant, Request{SourceEndpoint: "source", SourcePath: name, Policy: []byte(pol)})
	if err != nil {
		t.Fatal(err)
	}
	if err := dbos.CancelWorkflow(f.e.dbos, JobWorkflowID(id)); err != nil {
		t.Fatal(err)
	}
	j := job{f: f, id: id, src: filepath.Join(srcDir, name), transfers: map[string]uuid.UUID{}}
	if err := store.InTenant(ctx, f.api, f.tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, "select e.name, t.id from veho.transfers t join veho.endpoints e on e.id = t.destination_endpoint_id where t.job_id = $1", id)
		if err != nil {
			return err
		}
		for rows.Next() {
			var n string
			var tid uuid.UUID
			if err := rows.Scan(&n, &tid); err != nil {
				return err
			}
			j.transfers[n] = tid
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	if r := f.e.resolve(ctx, JobInput{Tenant: f.tenant, Job: id}); r.failed() {
		t.Fatalf("resolve: %+v", r)
	}
	return j
}

// launch starts the engine's executor, once.
func (f fixture) launch(t *testing.T) {
	t.Helper()
	if err := f.e.Launch(); err != nil && !strings.Contains(err.Error(), "already") {
		t.Fatal(err)
	}
}

// run runs the transfers to the endpoints dests, concurrently, and waits.
func (j job) run(t *testing.T, dests ...string) {
	t.Helper()
	var hs []dbos.WorkflowHandle[string]
	for _, d := range dests {
		in := TransferInput{Tenant: j.f.tenant, Job: j.id, Transfer: j.transfers[d]}
		h, err := dbos.RunWorkflow(j.f.e.dbos, j.f.e.transferWorkflow, in, dbos.WithWorkflowID(TransferWorkflowID(in.Transfer)+"/"+uuid.New().String()[:6]))
		if err != nil {
			t.Fatal(err)
		}
		hs = append(hs, h)
	}
	for i, h := range hs {
		if _, err := h.GetResult(dbos.WithHandleTimeout(90 * time.Second)); err != nil {
			t.Fatalf("transfer to %s: %v", dests[i], err)
		}
	}
}

type jobState struct {
	State               string
	Copies              int
	Satisfied, Complete bool
	Settled             bool
	Digest              string
	Transfers           map[string]string
	Attempts            map[string]int
	LastError           map[string]string
}

func (j job) state(t *testing.T) jobState {
	t.Helper()
	ctx := context.Background()
	s := jobState{Transfers: map[string]string{}, Attempts: map[string]int{}, LastError: map[string]string{}}
	if err := store.InTenant(ctx, j.f.api, j.f.tenant, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `select state, copies_done, satisfied_at is not null, completed_at is not null, settled_at is not null,
			coalesce(source_info->>'digest', '') from veho.jobs where id = $1`, j.id).Scan(&s.State, &s.Copies, &s.Satisfied, &s.Complete, &s.Settled, &s.Digest); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `select e.name, t.state, t.attempt, coalesce(t.last_error->>'class', '') from veho.transfers t
			join veho.endpoints e on e.id = t.destination_endpoint_id where t.job_id = $1`, j.id)
		if err != nil {
			return err
		}
		for rows.Next() {
			var n, st, class string
			var a int
			if err := rows.Scan(&n, &st, &a, &class); err != nil {
				return err
			}
			s.Transfers[n], s.Attempts[n], s.LastError[n] = st, a, class
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	return s
}

func same(t *testing.T, a, b string) {
	t.Helper()
	x, err := os.ReadFile(a)
	if err != nil {
		t.Fatal(err)
	}
	y, err := os.ReadFile(b)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(x, y) {
		t.Errorf("%s and %s differ (%d and %d bytes)", a, b, len(x), len(y))
	}
}

func destPolicy(minCopies int, extra string, dests ...string) string {
	var ds []string
	for _, d := range dests {
		ds = append(ds, `{"endpoint": "`+d+`", "path": "copy/OBJ"}`)
	}
	return `{"destinations": [` + strings.Join(ds, ", ") + `], "completion": {"min_copies": ` + string(rune('0'+minCopies)) +
		`}, "retry": {"initial": "100ms", "max_interval": "400ms"}` + extra + `}`
}

// ENG-2, ENG-9: a job to two file destinations and one on Garage succeeds
// with the source's bytes; no credential lands in DBOS's tables.
func TestTransfer(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	srcDir := f.fileEndpoint(t, "source", "file", "lab-a")
	d1 := f.fileEndpoint(t, "f1", "file", "lab-a")
	d2 := f.fileEndpoint(t, "f2", "file", "lab-b")
	dests := []string{"f1", "f2"}
	secretKey := os.Getenv("VEHO_TEST_S3_A_SECRET_KEY")
	if os.Getenv("VEHO_TEST_S3_A_ENDPOINT") != "" {
		creds, _ := json.Marshal(map[string]string{"access_key_id": os.Getenv("VEHO_TEST_S3_A_ACCESS_KEY"), "secret_access_key": secretKey})
		if _, err := f.secrets.Put(ctx, f.tenant, "garage-a", creds); err != nil {
			t.Fatal(err)
		}
		f.endpoint(t, "s3a", "s3", "lab-b", map[string]any{"endpoint": os.Getenv("VEHO_TEST_S3_A_ENDPOINT"),
			"region": os.Getenv("VEHO_TEST_S3_A_REGION"), "bucket": "dest-1", "path_style": true}, "db://garage-a")
		dests = append(dests, "s3a")
	}
	f.launch(t)
	j := f.submit(t, srcDir, 12*mib+123, destPolicy(len(dests), "", dests...))
	j.run(t, dests...)
	s := j.state(t)
	if s.State != "SUCCEEDED" || s.Copies != len(dests) || !s.Satisfied || !s.Complete || !s.Settled || len(s.Digest) != 64 {
		t.Errorf("job: %+v", s)
	}
	name := filepath.Base(j.src)
	same(t, j.src, filepath.Join(d1, "copy", name))
	same(t, j.src, filepath.Join(d2, "copy", name))
	var bytesDone, bytesTotal int64
	if err := store.InTenant(ctx, f.api, f.tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "select bytes_done, bytes_total from veho.transfers where id = $1", j.transfers["f1"]).Scan(&bytesDone, &bytesTotal)
	}); err != nil || bytesDone != 12*mib+123 || bytesTotal != bytesDone {
		t.Errorf("bytes %d of %d, %v", bytesDone, bytesTotal, err)
	}
	if secretKey != "" { // ENG-9
		var n int
		if err := f.api.QueryRow(ctx, `select count(*) from dbos.workflow_status s left join dbos.operation_outputs o using (workflow_uuid)
			where s.inputs like '%'||$1||'%' or s.output like '%'||$1||'%' or o.output like '%'||$1||'%' or o.error like '%'||$1||'%'`, secretKey).Scan(&n); err != nil || n != 0 {
			t.Errorf("the secret is in DBOS's tables: %d rows, %v", n, err)
		}
	}
}

// ENG-5: Transient failures are retried; Integrity three times at most;
// Permanent at once; the job follows its policy (ENG-3).
func TestRetries(t *testing.T) {
	f := setup(t)
	srcDir := f.fileEndpoint(t, "source", "file", "lab-a")
	f.fileEndpoint(t, "ok", "file", "lab-a")
	f.fileEndpoint(t, "fl", "flaky", "lab-b")
	f.launch(t)

	var n atomic.Int32
	f.flaky.set(func(int) error {
		if n.Add(1) <= 2 {
			return plugin.Wrapf(plugin.Transient, "throttled")
		}
		return nil
	})
	j := f.submit(t, srcDir, 6*mib, destPolicy(2, "", "ok", "fl"))
	j.run(t, "ok", "fl")
	// the two failures hit the two parts of the first batch: one retry
	if s := j.state(t); s.State != "SUCCEEDED" || s.Transfers["fl"] != "SUCCEEDED" || s.Attempts["fl"] != 2 {
		t.Errorf("transient: %+v", s)
	}

	// the upload lost: the transfer starts again from scratch
	var lost atomic.Bool
	f.flaky.set(func(int) error {
		if lost.CompareAndSwap(false, true) {
			return plugin.Wrapf(plugin.ResumeInvalid, "no such upload")
		}
		return nil
	})
	j = f.submit(t, srcDir, 6*mib, destPolicy(2, "", "ok", "fl"))
	j.run(t, "ok", "fl")
	if s := j.state(t); s.Transfers["fl"] != "SUCCEEDED" || s.Attempts["fl"] != 2 {
		t.Errorf("resume invalid: %+v", s)
	}

	f.flaky.set(func(int) error { return plugin.Wrapf(plugin.Integrity, "bad bytes") })
	j = f.submit(t, srcDir, 6*mib, destPolicy(1, "", "ok", "fl"))
	j.run(t, "ok", "fl")
	if s := j.state(t); s.State != "SUCCEEDED" || s.Transfers["fl"] != "FAILED" || s.Attempts["fl"] != 3 || s.LastError["fl"] != "Integrity" || !s.Settled || s.Complete {
		t.Errorf("integrity, 1 of 2: %+v", s)
	}

	// 2 of 2 with a permanent failure: FAILED at once, the other CANCELLED
	f.flaky.set(func(int) error { return plugin.Wrapf(plugin.Permanent, "forbidden") })
	j = f.submit(t, srcDir, 6*mib, destPolicy(2, "", "ok", "fl"))
	j.run(t, "fl")
	if s := j.state(t); s.State != "FAILED" || s.Transfers["fl"] != "FAILED" || s.Attempts["fl"] != 1 || s.Transfers["ok"] != "CANCELLED" || !s.Settled {
		t.Errorf("permanent, 2 of 2: %+v", s)
	}
	j.run(t, "ok") // its workflow finds it cancelled and ends
	if s := j.state(t); s.Transfers["ok"] != "CANCELLED" || s.Attempts["ok"] != 0 {
		t.Errorf("the cancelled transfer ran: %+v", s)
	}

	// an unclassified driver error is Unknown: 10 in a row, then FAILED
	f.flaky.set(func(int) error { return io.ErrClosedPipe })
	j = f.submit(t, srcDir, 1*mib, destPolicy(1, "", "ok", "fl"))
	j.run(t, "fl")
	if s := j.state(t); s.Transfers["fl"] != "FAILED" || s.Attempts["fl"] != maxUnknown || s.LastError["fl"] != "Unknown" {
		t.Errorf("unknown: %+v", s)
	}
}

// ENG-4: on_satisfied stop cancels the transfers left once the job is met.
func TestStop(t *testing.T) {
	f := setup(t)
	srcDir := f.fileEndpoint(t, "source", "file", "lab-a")
	f.fileEndpoint(t, "a", "file", "lab-a")
	f.fileEndpoint(t, "b", "file", "lab-b")
	f.launch(t)
	j := f.submit(t, srcDir, 1*mib, destPolicy(1, `, "on_satisfied": "stop"`, "a", "b"))
	j.run(t, "a")
	if s := j.state(t); s.State != "SUCCEEDED" || s.Transfers["b"] != "CANCELLED" || !s.Settled || s.Complete {
		t.Errorf("stop: %+v", s)
	}
}

// ENG-6 (in one process): a takeover while a step writes its parts: the
// step records nothing and the workflow ends; INV-3.
func TestFenced(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	srcDir := f.fileEndpoint(t, "source", "file", "lab-a")
	f.fileEndpoint(t, "a", "file", "lab-a")
	f.fileEndpoint(t, "fl", "flaky", "lab-b")
	f.launch(t)
	var j job
	f.flaky.set(func(int) error { // another executor takes the transfer meanwhile
		return store.InTenant(ctx, f.worker, f.tenant, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, "update veho.transfers set fence = fence + 1, lease_owner = 'other' where id = $1", j.transfers["fl"])
			return err
		})
	})
	j = f.submit(t, srcDir, 6*mib, destPolicy(1, "", "a", "fl"))
	j.run(t, "fl")
	var parts int
	var owner, state string
	if err := store.InTenant(ctx, f.api, f.tenant, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, "select count(*) from veho.parts where transfer_id = $1", j.transfers["fl"]).Scan(&parts); err != nil {
			return err
		}
		return tx.QueryRow(ctx, "select lease_owner, state from veho.transfers where id = $1", j.transfers["fl"]).Scan(&owner, &state)
	}); err != nil {
		t.Fatal(err)
	}
	if parts != 0 || owner != "other" || state != "COPYING" {
		t.Errorf("after the takeover: %d parts, owner %s, %s", parts, owner, state)
	}
}

// ENG-11: a source changed during the copy fails the transfer.
func TestSourceChanged(t *testing.T) {
	f := setup(t)
	srcDir := f.fileEndpoint(t, "source", "file", "lab-a")
	f.fileEndpoint(t, "ok", "file", "lab-a")
	f.fileEndpoint(t, "fl", "flaky", "lab-b")
	f.launch(t)
	var j job
	f.flaky.set(func(part int) error {
		if part == 1 {
			return os.WriteFile(j.src, []byte("changed"), 0o600)
		}
		return nil
	})
	j = f.submit(t, srcDir, 12*mib, destPolicy(1, "", "ok", "fl"))
	j.run(t, "fl")
	if s := j.state(t); s.Transfers["fl"] != "FAILED" || s.LastError["fl"] != "SourceChanged" {
		t.Errorf("source changed: %+v", s)
	}
}

// NFR-5: the node's part memory budget bounds the parts being copied at
// once across all its transfers: with room for one part, two transfers of
// parts_in_flight 2 never write two parts together, and still succeed.
func TestPartMemoryBudget(t *testing.T) {
	f := setup(t)
	f.e.partMemory = semaphore.NewWeighted(5 * mib) // one part of the tests' 5 MiB
	srcDir := f.fileEndpoint(t, "source", "file", "lab-a")
	f.fileEndpoint(t, "a", "flaky", "lab-a")
	f.fileEndpoint(t, "b", "flaky", "lab-b")
	var now, peak atomic.Int32
	f.flaky.set(func(int) error {
		n := now.Add(1)
		defer now.Add(-1)
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		time.Sleep(50 * time.Millisecond)
		return nil
	})
	job, _ := f.submitRun(t, srcDir, 20*mib, destPolicy(2, "", "a", "b"))
	f.launch(t)
	if s := f.wait(t, job, isSettled); s.State != "SUCCEEDED" {
		t.Fatalf("job: %+v", s)
	}
	if p := peak.Load(); p != 1 {
		t.Errorf("%d parts written at once, want 1", p)
	}
}
