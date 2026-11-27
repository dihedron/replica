//go:build acceptance && campaign

package acceptance

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	vehov1 "github.com/dihedron/veho/api/veho/v1"
	"github.com/dihedron/veho/test/envelope"
	"github.com/dihedron/veho/test/labtest"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func envDur(t *testing.T, name string) time.Duration {
	t.Helper()
	d, err := time.ParseDuration(os.Getenv(name))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return d
}

// appendRow adds the cell's row to results.csv, with the header if new.
func appendRow(t *testing.T, path string, r envelope.Row) {
	t.Helper()
	_, err := os.Stat(path)
	fresh := errors.Is(err, os.ErrNotExist)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644) //nolint:gosec // the lab's evidence
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if fresh {
		_, _ = f.WriteString(envelope.Header + "\n")
	}
	if err := envelope.WriteRow(f, r); err != nil {
		t.Fatal(err)
	}
}

// sample is etcd's leader and term and Patroni's leader at an instant.
type sample struct {
	at      time.Time
	leaders []string
	terms   []int
	patroni string
}

func etcdSample(l *labtest.Lab) (leaders []string, terms []int) {
	out, err := l.TrySh("etcdctl", "endpoint", "status", "-w", "json")
	if err != nil {
		return nil, nil
	}
	var st []struct {
		Status struct {
			Leader   uint64 `json:"leader"`
			RaftTerm int    `json:"raftTerm"`
		}
	}
	if json.Unmarshal([]byte(out), &st) != nil {
		return nil, nil
	}
	for _, s := range st {
		leaders = append(leaders, strconv.FormatUint(s.Status.Leader, 10))
		terms = append(terms, s.Status.RaftTerm)
	}
	return leaders, terms
}

func timelines(l *labtest.Lab) int {
	out, err := l.TrySh("patronictl", "history", "-f", "json")
	if err != nil {
		return -1
	}
	var h []any
	if json.Unmarshal([]byte(out), &h) != nil {
		return -1
	}
	return len(h)
}

// cpuTimes is the lab VM's total and steal jiffies.
func cpuTimes(l *labtest.Lab) (total, steal float64) {
	out, _ := l.TrySh("ssh", "lxd", "head", "-1", "/proc/stat")
	f := strings.Fields(out)
	for i, v := range f[1:] {
		n, _ := strconv.ParseFloat(v, 64)
		total += n
		if i == 7 {
			steal = n
		}
	}
	return total, steal
}

func percentile(ms []int, p float64) int {
	if len(ms) == 0 {
		return 0
	}
	s := slices.Clone(ms)
	slices.Sort(s)
	return s[min(len(s)-1, int(float64(len(s))*p))]
}

// TestEnvelopeCell is one AC-24 cell (.specs/veho-latency-campaign.md),
// run by lab.sh campaign envelope after the profile, the links and 5 quiet
// minutes: a light load and a prober for the observation, the shift of a
// "shift" cell at a random time in it, then the Patroni leader and a node
// holding a transfer killed; one row of results.csv.
func TestEnvelopeCell(t *testing.T) {
	id := os.Getenv("VEHO_ENVELOPE_CELL")
	if id == "" {
		t.Skip("run by lab.sh campaign envelope")
	}
	results := os.Getenv("VEHO_ENVELOPE_RESULTS")
	row := envelope.Row{Commit: os.Getenv("VEHO_ENVELOPE_COMMIT"), Version: os.Getenv("VEHO_ENVELOPE_VERSION"),
		Date: time.Now().UTC().Format(time.DateOnly), Cell: id, Profile: os.Getenv("VEHO_ENVELOPE_PROFILE")}
	if os.Getenv("VEHO_ENVELOPE_INVALID") != "" {
		appendRow(t, results, row) // the cell never settled: invalid, run again
		return
	}
	observe, inject := envDur(t, "VEHO_ENVELOPE_OBSERVE"), envDur(t, "VEHO_ENVELOPE_INJECT")
	l := labtest.New(t)
	tn := l.Tenant()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), 3))
	tot0, steal0 := cpuTimes(l)

	// the sampler, every 10 s, kept until the end of the observation
	var mu sync.Mutex
	var samples []sample
	observing := true
	go func() {
		for ctx.Err() == nil {
			leaders, terms := etcdSample(l)
			s := sample{at: time.Now(), leaders: leaders, terms: terms, patroni: l.Leader()}
			mu.Lock()
			if observing {
				samples = append(samples, s)
			}
			mu.Unlock()
			select {
			case <-ctx.Done():
			case <-time.After(10 * time.Second):
			}
		}
	}()

	// the light load: one 32 MiB job at a time to a pair of destinations
	sha := tn.Put("a", "load", 32*mib)
	var done, failed int
	var loadWG sync.WaitGroup
	loadWG.Go(func() {
		dests := all9()
		for n := 0; ctx.Err() == nil; n++ {
			first := r.IntN(len(dests))
			pair := []labtest.Dest{dests[first], dests[(first+1+r.IntN(len(dests)-1))%len(dests)]}
			path := "l-" + strconv.Itoa(n)
			c := tn.Clients(l.Nodes()[r.IntN(6)])
			j, err := c.Jobs.CreateJob(ctx, &vehov1.CreateJobRequest{
				Source: &vehov1.ObjectRef{Endpoint: "a-source", Path: tn.Prefix + "load"}, Policy: tn.Policy(path, pair, nil)})
			if err != nil {
				time.Sleep(5 * time.Second)
				continue
			}
			got := waitAny(tn, l, j.GetId(), time.Hour)
			ok := got.GetState() == "SUCCEEDED"
			for _, d := range pair {
				ok = ok && tn.SHA256(d.Region, d.Bucket, path) == sha
				tn.Delete(d.Region, d.Bucket, path)
			}
			mu.Lock()
			if ok {
				done++
			} else {
				failed++
			}
			mu.Unlock()
		}
	})

	// the prober: CreateJob of a 1 MiB object every 15 s through a random
	// node; its latency is the acceptance's
	tn.Put("a", "probe", 1*mib)
	var latencies []int
	probing := true
	go func() {
		for n := 0; ctx.Err() == nil; n++ {
			c := tn.Clients(l.Nodes()[r.IntN(6)])
			start := time.Now()
			_, err := c.Jobs.CreateJob(ctx, &vehov1.CreateJobRequest{
				Source: &vehov1.ObjectRef{Endpoint: "a-source", Path: tn.Prefix + "probe"},
				Policy: tn.Policy("p-"+strconv.Itoa(n), []labtest.Dest{{Region: labtest.Regions[r.IntN(3)], Bucket: "dest-3"}}, nil)})
			mu.Lock()
			if err == nil && probing {
				latencies = append(latencies, int(time.Since(start).Milliseconds()))
			}
			mu.Unlock()
			select {
			case <-ctx.Done():
			case <-time.After(15 * time.Second):
			}
		}
	}()

	// the observation, with the baseline shift of a "shift" cell
	if shift := os.Getenv("VEHO_ENVELOPE_SHIFT"); shift != "" && shift != "-" {
		go func() {
			select {
			case <-ctx.Done():
				return
			case <-time.After(observe/6 + time.Duration(r.Int64N(int64(observe*2/3)))):
			}
			t.Logf("baseline shift: links %s", shift)
			l.Sh("wan", "set", "--link", "a-b="+shift, "--link", "a-c="+shift, "--link", "b-c="+shift)
		}()
	}
	tl0 := timelines(l)
	time.Sleep(observe)
	tl1 := timelines(l)
	mu.Lock()
	observing, probing = false, false
	terms, leaders := map[int]bool{}, map[string]bool{}
	for _, s := range samples {
		for _, term := range s.terms {
			terms[term] = true
		}
		for _, ld := range s.leaders {
			leaders[ld] = true
		}
	}
	row.Elections = max(len(terms)-1, len(leaders)-1, 0)
	row.Demotions = max(tl1-tl0, 0)
	row.P50, row.P99 = percentile(latencies, 0.5), percentile(latencies, 0.99)
	t.Logf("observed: %d samples, terms %v, %d probes, timelines %d to %d", len(samples), terms, len(latencies), tl0, tl1)
	mu.Unlock()

	// the injections, the node first (its adoption over this cell's WAN),
	// then the Patroni leader; their elections are on purpose, never counted
	injectEnd := time.Now().Add(inject)
	lab := true // false: a lab failure invalidates the cell
	row.Adoption = adoption(t, l, tn, r)
	if leader := l.Leader(); leader != "" {
		t0 := time.Now()
		l.Sh("db", "kill", leader[len("db-"):])
		for time.Since(t0) < 10*time.Minute {
			c := tn.Clients(l.Nodes()[r.IntN(6)])
			_, err := c.Jobs.CreateJob(ctx, &vehov1.CreateJobRequest{
				Source: &vehov1.ObjectRef{Endpoint: "a-source", Path: tn.Prefix + "probe"},
				Policy: tn.Policy("f-"+strconv.FormatInt(time.Now().UnixNano(), 10), []labtest.Dest{{Region: "b", Bucket: "dest-3"}}, nil)})
			if err == nil {
				row.Failover = int(time.Since(t0).Seconds())
				break
			}
			if c := status.Code(err); c != codes.Unavailable && c != codes.DeadlineExceeded {
				t.Logf("failover probe: %v", err)
			}
			time.Sleep(time.Second)
		}
		t.Logf("failover: %d s after killing %s", row.Failover, leader)
		// the killed member rejoins over the lab's usual WAN: over this
		// cell's (a lossy link moves a base backup at about 150 kB/s) it
		// would take hours, and its rejoin is not what the cell measures
		l.Sh("wan", "set", os.Getenv("VEHO_ENVELOPE_RESTORE_PROFILE"))
		if out, err := l.TrySh("db", "start", leader[len("db-"):]); err != nil {
			// the lab, not the product: the killed member did not rejoin in
			// time over this WAN; the campaign waits for it before the next
			// cell, and this one is run again
			t.Logf("%s did not rejoin: %v: %s", leader, err, out)
			lab = false
		}
	}
	if wait := time.Until(injectEnd); wait > 0 {
		time.Sleep(wait)
	}
	cancel()
	loadWG.Wait()
	row.JobsDone, row.JobsFailed = done, failed

	tot1, steal1 := cpuTimes(l)
	if tot1 > tot0 {
		row.HostSteal = 100 * (steal1 - steal0) / (tot1 - tot0)
	}
	if out, err := l.TrySh("ssh", "lxd", "sh", "-c", "echo $(cut -d' ' -f1 /proc/loadavg) $(nproc)"); err == nil {
		f := strings.Fields(out)
		if len(f) == 2 {
			load, _ := strconv.ParseFloat(f[0], 64)
			cpus, _ := strconv.ParseFloat(f[1], 64)
			row.HostLoad = load / max(cpus, 1)
		}
	}
	row.Valid = lab && row.HostLoad <= 1 && row.HostSteal <= 10 // decision 7
	appendRow(t, results, row)
	t.Logf("row: %+v", row)
}

// adoption kills the node that holds a running transfer and measures the
// time until another node holds its lease.
func adoption(t *testing.T, l *labtest.Lab, tn *labtest.Tenant, r *rand.Rand) int {
	t.Helper()
	ctx := context.Background()
	tn.Put("a", "big", 512*mib)
	c := tn.Clients(l.Nodes()[r.IntN(6)])
	j, err := c.Jobs.CreateJob(ctx, &vehov1.CreateJobRequest{
		Source: &vehov1.ObjectRef{Endpoint: "a-source", Path: tn.Prefix + "big"},
		Policy: tn.Policy("big", []labtest.Dest{{Region: "c", Bucket: "dest-3"}}, nil)})
	if err != nil {
		t.Logf("adoption: submit: %v", err)
		return -1
	}
	var owner string
	for deadline := time.Now().Add(3 * time.Minute); owner == "" && time.Now().Before(deadline); time.Sleep(2 * time.Second) {
		_ = l.DB().QueryRow(ctx, "select coalesce(lease_owner, '') from veho.transfers where job_id = $1 and state = 'COPYING'", j.GetId()).Scan(&owner)
	}
	if owner == "" {
		t.Log("adoption: the transfer never started copying")
		return -1
	}
	t0 := time.Now()
	l.Kill(owner, true)
	defer l.Start(owner)
	for time.Since(t0) < 10*time.Minute {
		var now string
		_ = l.DB().QueryRow(ctx, "select coalesce(lease_owner, '') from veho.transfers where job_id = $1", j.GetId()).Scan(&now)
		if now != "" && now != owner {
			t.Logf("adoption: %s took the transfer from %s", now, owner)
			return int(time.Since(t0).Seconds())
		}
		time.Sleep(time.Second)
	}
	return -1
}
