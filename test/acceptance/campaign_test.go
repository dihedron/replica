//go:build acceptance && campaign

package acceptance

import (
	"context"
	"math/rand/v2"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	vehov1 "github.com/dihedron/veho/api/veho/v1"
	"github.com/dihedron/veho/test/labtest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func envDuration(t *testing.T, name string, def time.Duration) time.Duration {
	if s := os.Getenv(name); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return d
	}
	return def
}

func envInt(t *testing.T, name string, def int) int {
	if s := os.Getenv(name); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return n
	}
	return def
}

// TestCampaignSoak is AC-17 (veho-lab-l3.md §6): LAB_SOAK_JOBS jobs (50) kept
// running for LAB_SOAK_DURATION (8h), each from one of eight source objects
// of 32 to 256 MiB to two of the nine destinations, while a chaos loop every
// 2 to 10 minutes kills a node (half of them kept down for 5 minutes),
// pauses one past its lease, kills the PostgreSQL leader, partitions a
// region for 2 minutes or makes a destination answer 503 for 5 minutes.
// Every job must succeed with the source's bytes. Each job's copies are
// deleted once it settles, whatever its outcome, and a janitor aborts stale
// uploads; below 4 GiB free on the lab VM the campaign stops, failed, rather
// than run on a full disk (the third soak filled it in under 4 hours).
func TestCampaignSoak(t *testing.T) {
	duration := envDuration(t, "LAB_SOAK_DURATION", 8*time.Hour)
	jobs := envInt(t, "LAB_SOAK_JOBS", 50)
	l := labtest.New(t)
	tn := l.Tenant()
	type object struct{ path, sha string }
	var objects []object
	for i := range 8 {
		path := "src-" + strconv.Itoa(i)
		objects = append(objects, object{path, tn.Put("a", path, int64(32+32*i)*mib)})
	}
	dests := all9()
	deadline := time.Now().Add(duration)
	stop := make(chan struct{}) // the disk guard
	var stopOnce sync.Once
	running := func() bool {
		select {
		case <-stop:
			return false
		default:
			return time.Now().Before(deadline)
		}
	}
	// the janitor: stale uploads, and the disk guard
	go func() {
		for running() {
			select {
			case <-stop:
				return
			case <-time.After(10 * time.Minute):
			}
			if n := tn.AbortStale(time.Hour); n > 0 {
				t.Logf("janitor: %d stale uploads aborted", n)
			}
			if free := l.FreeGiB(); free >= 0 && free < 4 {
				t.Errorf("the lab VM has %d GiB free: the soak stops", free)
				stopOnce.Do(func() { close(stop) })
			}
		}
	}()

	var mu sync.Mutex
	done, failed := 0, 0
	report := func(ok bool, format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		if ok {
			done++
			return
		}
		failed++
		t.Errorf(format, args...)
	}

	// the chaos loop; LAB_SOAK_CHAOS=0 turns it off (the wan24 campaign's
	// light load)
	chaosDone := make(chan struct{})
	go func() {
		defer close(chaosDone)
		if os.Getenv("LAB_SOAK_CHAOS") == "0" {
			return
		}
		r := rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), 1))
		for running() {
			time.Sleep(time.Duration(2+r.IntN(9)) * time.Minute)
			if !running() {
				return
			}
			// only nodes that run: a node still restarting has no process to
			// signal; a failed step is logged, and the loop goes on
			var up []string
			for _, n := range l.Nodes() {
				if l.Active(n) {
					up = append(up, n)
				}
			}
			if len(up) < 2 {
				t.Logf("chaos: only %d nodes run, skipping a round", len(up))
				continue
			}
			node := up[r.IntN(len(up))]
			region := labtest.Regions[r.IntN(3)]
			sh := func(args ...string) {
				if _, err := l.TrySh(args...); err != nil {
					t.Logf("chaos: %v", err)
				}
			}
			switch r.IntN(5) {
			case 0:
				t.Logf("chaos: kill %s", node)
				sh("node", "kill", node)
			case 1:
				t.Logf("chaos: kill %s, down for 5 minutes", node)
				sh("node", "kill", node, "--stay-down")
				time.Sleep(5 * time.Minute)
				sh("node", "start", node)
			case 2:
				t.Logf("chaos: pause %s past its lease", node)
				sh("node", "pause", node)
				time.Sleep(lease + 30*time.Second)
				sh("node", "resume", node)
			case 3:
				if leader := l.Leader(); leader != "" {
					t.Logf("chaos: kill the leader %s", leader)
					sh("db", "kill", leader[len("db-"):])
					time.Sleep(time.Minute)
					sh("db", "start", leader[len("db-"):])
				}
			case 4:
				if r.IntN(2) == 0 {
					t.Logf("chaos: partition region %s for 2 minutes", region)
					sh("wan", "partition", region)
					time.Sleep(2 * time.Minute)
					sh("wan", "heal")
				} else {
					t.Logf("chaos: region %s's destinations answer 503 for 5 minutes", region)
					sh("fault", region, "status", "503", "--for", "5m")
				}
			}
		}
	}()

	// the jobs: each worker submits, waits and verifies, until the deadline
	var wg sync.WaitGroup
	for w := range jobs {
		wg.Go(func() {
			r := rand.New(rand.NewPCG(uint64(w), uint64(time.Now().UnixNano())))
			for n := 0; running(); n++ {
				o := objects[r.IntN(len(objects))]
				first := r.IntN(len(dests))
				pair := []labtest.Dest{dests[first], dests[(first+1+r.IntN(len(dests)-1))%len(dests)]}
				path := "w" + strconv.Itoa(w) + "-" + strconv.Itoa(n)
				policy := tn.Policy(path, pair, map[string]any{"retry": map[string]any{"initial": "5s", "max_interval": "1m"}})
				var id string
				for id == "" && time.Now().Before(deadline.Add(time.Hour)) {
					node := l.Nodes()[r.IntN(6)]
					j, err := tn.Clients(node).Jobs.CreateJob(context.Background(), &vehov1.CreateJobRequest{
						Source: &vehov1.ObjectRef{Endpoint: "a-source", Path: tn.Prefix + o.path}, Policy: policy})
					switch status.Code(err) {
					case codes.OK:
						id = j.GetId()
						l.Track(id)
					case codes.Unavailable, codes.DeadlineExceeded, codes.ResourceExhausted:
						// a node fenced or cut off, or the rate limits of 50
						// workers on one principal: wait, then another node
						time.Sleep(time.Duration(1+r.IntN(5)) * time.Second)
					default:
						report(false, "submit: %v", err)
						time.Sleep(5 * time.Second)
					}
				}
				if id == "" {
					continue
				}
				j := waitAny(tn, l, id, 3*time.Hour)
				ok := j.GetState() == "SUCCEEDED"
				if !ok {
					report(false, "job %s: %s", id, j.GetState())
				}
				for _, d := range pair {
					if ok {
						if got := tn.SHA256(d.Region, d.Bucket, path); got != o.sha {
							ok = false
							report(false, "job %s, %s-%s: sha256 %s, want %s", id, d.Region, d.Bucket, got, o.sha)
						}
					}
					tn.Delete(d.Region, d.Bucket, path) // whatever the outcome
				}
				if ok {
					report(true, "")
				}
			}
		})
	}
	wg.Wait()
	<-chaosDone
	t.Logf("soak: %d jobs succeeded, %d failed, in %v", done, failed, duration)
}

// waitAny follows a job through whichever node answers.
func waitAny(tn *labtest.Tenant, l *labtest.Lab, id string, within time.Duration) *vehov1.Job {
	deadline := time.Now().Add(within)
	// the ETag of the last answer: without it GetJob answers at once, and
	// the loop polls as fast as the API answers
	etag := ""
	for time.Now().Before(deadline) {
		c := tn.Clients(l.Nodes()[rand.IntN(6)])
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		if etag != "" {
			ctx = metadata.AppendToOutgoingContext(ctx, "if-none-match", etag)
		}
		var header metadata.MD
		j, err := c.Jobs.GetJob(ctx, &vehov1.GetJobRequest{Id: id, Wait: 30}, grpc.Header(&header))
		cancel()
		if v := header.Get("etag"); len(v) > 0 {
			etag = v[0]
		}
		if err == nil && j.GetSettledAt() != nil {
			return j
		}
		if err != nil && status.Code(err) != codes.Aborted { // not_modified: ask again
			time.Sleep(5 * time.Second)
		}
	}
	return &vehov1.Job{Id: id, State: "UNSETTLED"}
}
