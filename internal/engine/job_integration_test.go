//go:build integration

package engine

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/dihedron/veho/pkg/plugin"
)

// submitRun writes a source of size bytes and submits a job the engine
// runs, ReplicateJob included.
func (f fixture) submitRun(t *testing.T, srcDir string, size int, pol string) (uuid.UUID, string) {
	t.Helper()
	name := "obj-" + uuid.New().String()[:8]
	data := make([]byte, size)
	_, _ = rand.Read(data)
	if err := os.WriteFile(filepath.Join(srcDir, name), data, 0o600); err != nil {
		t.Fatal(err)
	}
	id, _, err := f.e.Submit(context.Background(), f.tenant, Request{CreatedBy: "key:test", SourceEndpoint: "source", SourcePath: name, Policy: []byte(strings.ReplaceAll(pol, "OBJ", name))})
	if err != nil {
		t.Fatal(err)
	}
	return id, name
}

// wait polls the job until it settles, or fails the test.
func (f fixture) wait(t *testing.T, job uuid.UUID, settled func(JobStatus) bool) JobStatus {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for {
		s, err := f.e.Status(context.Background(), f.tenant, job)
		if err != nil {
			t.Fatal(err)
		}
		if settled(s) {
			return s
		}
		if time.Now().After(deadline) {
			t.Fatalf("the job did not settle: %+v", s)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func isSettled(s JobStatus) bool { return s.SettledAt != nil }

// ENG-2 end to end: ReplicateJob resolves, starts the transfers and waits.
func TestJob(t *testing.T) {
	f := setup(t)
	srcDir := f.fileEndpoint(t, "source", "file", "lab-a")
	d1 := f.fileEndpoint(t, "a", "file", "lab-a")
	d2 := f.fileEndpoint(t, "b", "file", "lab-b")
	f.launch(t)
	job, name := f.submitRun(t, srcDir, 11*mib, destPolicy(2, "", "a", "b"))
	s := f.wait(t, job, isSettled)
	if s.State != "SUCCEEDED" || s.Copies != 2 || s.CompletedAt == nil || len(s.Transfers) != 2 {
		t.Fatalf("job: %+v", s)
	}
	for _, tr := range s.Transfers {
		if tr.State != "SUCCEEDED" || tr.BytesDone != 11*mib || tr.Attempt != 1 {
			t.Errorf("transfer: %+v", tr)
		}
	}
	same(t, filepath.Join(srcDir, name), filepath.Join(d1, "copy", name))
	same(t, filepath.Join(srcDir, name), filepath.Join(d2, "copy", name))
	h, err := dbos.RetrieveWorkflow[string](f.e.dbos, JobWorkflowID(job))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.GetResult(dbos.WithHandleTimeout(30 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var settled int
	for _, e := range f.audit.Events() {
		if e.Action == "JOB_SETTLED" && e.JobID == job.String() {
			settled++
		}
	}
	if settled != 1 {
		t.Errorf("%d JOB_SETTLED events", settled)
	}
}

// A missing source fails the job and cancels its transfers.
func TestJobNoSource(t *testing.T) {
	f := setup(t)
	srcDir := f.fileEndpoint(t, "source", "file", "lab-a")
	f.fileEndpoint(t, "a", "file", "lab-a")
	f.fileEndpoint(t, "b", "file", "lab-b")
	f.launch(t)
	job, name := f.submitRun(t, srcDir, 1, destPolicy(2, "", "a", "b"))
	_ = os.Remove(filepath.Join(srcDir, name))
	s := f.wait(t, job, isSettled)
	if s.State != "FAILED" || s.Transfers[0].State != "CANCELLED" || !strings.Contains(string(s.LastError), "Permanent") {
		t.Errorf("job: %+v, %s", s, s.LastError)
	}
}

// ENG-8: Cancel mid-copy stops the job with no object at the destination;
// Retry of a failed job copies it.
func TestCancelRetry(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	srcDir := f.fileEndpoint(t, "source", "file", "lab-a")
	slow := f.fileEndpoint(t, "fl", "flaky", "lab-a")
	f.fileEndpoint(t, "b", "file", "lab-b")
	f.launch(t)

	started := make(chan struct{}, 16)
	f.flaky.set(func(int) error {
		started <- struct{}{}
		time.Sleep(300 * time.Millisecond)
		return nil
	})
	job, name := f.submitRun(t, srcDir, 30*mib, destPolicy(2, "", "fl", "b"))
	<-started
	if err := f.e.Cancel(ctx, f.tenant, job); err != nil {
		t.Fatal(err)
	}
	s := f.wait(t, job, func(s JobStatus) bool { return s.State == "CANCELLED" })
	for _, tr := range s.Transfers {
		if tr.Endpoint == "fl" && tr.State != "CANCELLED" {
			t.Errorf("the slow transfer: %+v", tr)
		}
	}
	if _, err := os.Stat(filepath.Join(slow, "copy", name)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a cancelled copy is visible: %v", err)
	}
	if err := f.e.Cancel(ctx, f.tenant, job); err != nil {
		t.Errorf("cancel again: %v", err)
	}
	if err := f.e.Retry(ctx, f.tenant, job); !errors.Is(err, ErrState) {
		t.Errorf("retry of a cancelled job: %v", err)
	}

	f.flaky.set(func(int) error { return plugin.Wrapf(plugin.Permanent, "forbidden") })
	job, name = f.submitRun(t, srcDir, 6*mib, destPolicy(2, "", "fl", "b"))
	if s := f.wait(t, job, isSettled); s.State != "FAILED" {
		t.Fatalf("job: %+v", s)
	}
	f.flaky.set(nil)
	if err := f.e.Retry(ctx, f.tenant, job); err != nil {
		t.Fatal(err)
	}
	s = f.wait(t, job, func(s JobStatus) bool { return s.State == "SUCCEEDED" && s.SettledAt != nil })
	if s.Copies != 2 {
		t.Errorf("after retry: %+v", s)
	}
	same(t, filepath.Join(srcDir, name), filepath.Join(slow, "copy", name))
	if _, err := f.e.Status(ctx, uuid.New(), job); !errors.Is(err, ErrNotFound) {
		t.Errorf("another tenant's job: %v", err)
	}
}
