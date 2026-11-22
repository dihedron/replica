//go:build acceptance

// Package acceptance is the daytime acceptance suite of lab L3
// (.specs/veho-lab-l3.md §4): each test is one AC-nn scenario of
// .specs/veho-acceptance-matrix.md §4, against the six veho nodes of the lab.
// Run it with test/lab/lab.sh acceptance.
package acceptance

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	vehov1 "github.com/dihedron/veho/api/veho/v1"
	"github.com/dihedron/veho/test/labtest"
)

const mib = 1 << 20

// size is AC-1's object: LAB_AC1_SIZE bytes, default 1 GiB (10 GiB in the
// "long" campaign, §6).
func size(t *testing.T) int64 {
	if s := os.Getenv("LAB_AC1_SIZE"); s != "" {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			t.Fatalf("LAB_AC1_SIZE: %v", err)
		}
		return n
	}
	return 1 << 30
}

// bc are a destination in region b and one in region c.
var bc = []labtest.Dest{{Region: "b", Bucket: "dest-1"}, {Region: "c", Bucket: "dest-1"}}

// settled checks that the job succeeded and that every destination holds
// the source's bytes.
func settled(t *testing.T, tn *labtest.Tenant, j *vehov1.Job, want string, path string, dests []labtest.Dest) {
	t.Helper()
	if j.GetState() != "SUCCEEDED" {
		t.Fatalf("job %s: %s (%v)", j.GetId(), j.GetState(), j.GetLastError())
	}
	for _, d := range dests {
		if got := tn.SHA256(d.Region, d.Bucket, path); got != want {
			t.Errorf("%s-%s: sha256 %q, want %q", d.Region, d.Bucket, got, want)
		}
	}
}

// AC-1: the happy path from region a to b and c; progress never goes down.
func TestAC01HappyPath(t *testing.T) {
	l := labtest.New(t)
	tn := l.Tenant()
	want := tn.Put("a", "obj", size(t))
	c := tn.Clients("veho-a1")
	j := tn.Submit(c, "a", "obj", tn.Policy("obj", bc, nil))
	last := 0.0
	for deadline := time.Now().Add(30 * time.Minute); time.Now().Before(deadline); time.Sleep(time.Second) {
		got, err := c.Jobs.GetJob(context.Background(), &vehov1.GetJobRequest{Id: j.GetId()})
		if err != nil {
			t.Fatal(err)
		}
		if got.GetProgress() < last {
			t.Errorf("progress went down: %v after %v", got.GetProgress(), last)
		}
		last = got.GetProgress()
		if got.GetSettledAt() != nil {
			settled(t, tn, got, want, "obj", bc)
			return
		}
	}
	t.Fatal("the job did not settle")
}
