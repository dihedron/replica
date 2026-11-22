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
	"google.golang.org/grpc/codes"
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
// of 64 to 512 MiB to two of the nine destinations, while a chaos loop every
// 2 to 10 minutes kills a node (half of them kept down for 5 minutes),
// pauses one past its lease, kills the PostgreSQL leader, partitions a
// region for 2 minutes or makes a destination answer 503 for 5 minutes.
// Every job must succeed with the source's bytes; each job's copies are
// deleted once verified, so that the lab's disk holds.
func TestCampaignSoak(t *testing.T) {
	duration := envDuration(t, "LAB_SOAK_DURATION", 8*time.Hour)
	jobs := envInt(t, "LAB_SOAK_JOBS", 50)
	l := labtest.New(t)
	tn := l.Tenant()
	type object struct{ path, sha string }
	var objects []object
	for i := range 8 {
		path := "src-" + strconv.Itoa(i)
		objects = append(objects, object{path, tn.Put("a", path, int64(64+64*i)*mib)})
	}
	dests := all9()
	deadline := time.Now().Add(duration)

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
		for time.Now().Before(deadline) {
			time.Sleep(time.Duration(2+r.IntN(9)) * time.Minute)
			if !time.Now().Before(deadline) {
				return
			}
			nodes := l.Nodes()
			node := nodes[r.IntN(len(nodes))]
			region := labtest.Regions[r.IntN(3)]
			switch r.IntN(5) {
			case 0:
				t.Logf("chaos: kill %s", node)
				l.Sh("node", "kill", node)
			case 1:
				t.Logf("chaos: kill %s, down for 5 minutes", node)
				l.Sh("node", "kill", node, "--stay-down")
				time.Sleep(5 * time.Minute)
				l.Sh("node", "start", node)
			case 2:
				t.Logf("chaos: pause %s past its lease", node)
				l.Sh("node", "pause", node)
				time.Sleep(lease + 30*time.Second)
				l.Sh("node", "resume", node)
			case 3:
				if leader := l.Leader(); leader != "" {
					t.Logf("chaos: kill the leader %s", leader)
					l.Sh("db", "kill", leader[len("db-"):])
					time.Sleep(time.Minute)
					l.Sh("db", "start", leader[len("db-"):])
				}
			case 4:
				if r.IntN(2) == 0 {
					t.Logf("chaos: partition region %s for 2 minutes", region)
					l.Sh("wan", "partition", region)
					time.Sleep(2 * time.Minute)
					l.Sh("wan", "heal")
				} else {
					t.Logf("chaos: region %s's destinations answer 503 for 5 minutes", region)
					l.Sh("fault", region, "status", "503", "--for", "5m")
				}
			}
		}
	}()

	// the jobs: each worker submits, waits and verifies, until the deadline
	var wg sync.WaitGroup
	for w := range jobs {
		wg.Go(func() {
			r := rand.New(rand.NewPCG(uint64(w), uint64(time.Now().UnixNano())))
			for n := 0; time.Now().Before(deadline); n++ {
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
					switch {
					case err == nil:
						id = j.GetId()
						l.Track(id)
					case status.Code(err) == codes.Unavailable, status.Code(err) == codes.DeadlineExceeded:
						time.Sleep(5 * time.Second) // that node is fenced or cut off: another one
					default:
						report(false, "submit: %v", err)
						return
					}
				}
				if id == "" {
					return
				}
				j := waitAny(tn, l, id, 3*time.Hour)
				if j.GetState() != "SUCCEEDED" {
					report(false, "job %s: %s", id, j.GetState())
					continue
				}
				ok := true
				for _, d := range pair {
					if got := tn.SHA256(d.Region, d.Bucket, path); got != o.sha {
						ok = false
						report(false, "job %s, %s-%s: sha256 %s, want %s", id, d.Region, d.Bucket, got, o.sha)
					}
					tn.Delete(d.Region, d.Bucket, path)
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
	for time.Now().Before(deadline) {
		c := tn.Clients(l.Nodes()[rand.IntN(6)])
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		j, err := c.Jobs.GetJob(ctx, &vehov1.GetJobRequest{Id: id, Wait: 30})
		cancel()
		if err == nil && j.GetSettledAt() != nil {
			return j
		}
		if err != nil {
			time.Sleep(5 * time.Second)
		}
	}
	return &vehov1.Job{Id: id, State: "UNSETTLED"}
}
