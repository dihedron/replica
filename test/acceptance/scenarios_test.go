//go:build acceptance

package acceptance

import (
	"bytes"
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	vehov1 "github.com/dihedron/veho/api/veho/v1"
	"github.com/dihedron/veho/test/labtest"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	part = 64 * mib // the nodes' part size
	// lease is the nodes' lease TTL; adoption takes at most lease plus one
	// housekeeping interval (30 s) after an executor stops beating
	lease = 60 * time.Second
)

var (
	cDest = []labtest.Dest{{Region: "c", Bucket: "dest-1"}}
	bDest = []labtest.Dest{{Region: "b", Bucket: "dest-1"}}
)

// transfer is a transfer's row, as the database holds it.
type transfer struct {
	Endpoint, State, Owner, Class string
	Adoptions, Attempt, Parts     int
	Distinct                      int
}

func transfers(t *testing.T, l *labtest.Lab, job string) map[string]transfer {
	t.Helper()
	rows, err := l.DB().Query(context.Background(), `select e.name, t.state, coalesce(t.lease_owner, ''), coalesce(t.last_error->>'class', ''),
		t.adoptions, t.attempt, (select count(*) from veho.parts p where p.transfer_id = t.id),
		(select count(distinct p.part_no) from veho.parts p where p.transfer_id = t.id)
		from veho.transfers t join veho.endpoints e on e.id = t.destination_endpoint_id where t.job_id = $1`, job)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]transfer{}
	for rows.Next() {
		var tr transfer
		if err := rows.Scan(&tr.Endpoint, &tr.State, &tr.Owner, &tr.Class, &tr.Adoptions, &tr.Attempt, &tr.Parts, &tr.Distinct); err != nil {
			t.Fatal(err)
		}
		out[tr.Endpoint] = tr
	}
	return out
}

// until polls cond every second for at most d.
func until(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(d); time.Now().Before(deadline); time.Sleep(time.Second) {
		if cond() {
			return
		}
	}
	t.Fatalf("%s: not within %v", what, d)
}

// copying waits until the transfer to endpoint has committed parts and has
// an owner, and returns it.
func copying(t *testing.T, l *labtest.Lab, job, endpoint string, parts int) transfer {
	t.Helper()
	var tr transfer
	until(t, 5*time.Minute, "the transfer to "+endpoint+" copying", func() bool {
		tr = transfers(t, l, job)[endpoint]
		return tr.Owner != "" && tr.Parts >= parts
	})
	return tr
}

// slow delays every answer of region x's bucket, so that a transfer to it
// lasts long enough to be interfered with.
func slow(l *labtest.Lab, x, bucket string) { l.Fault(x, "delay", "3s", "--bucket", bucket) }

// AC-2: kill -9 the executor of a running transfer, and keep it down:
// another node adopts within lease + 30 s, resumes from the last committed
// part, and the final object is right.
func TestAC02KillExecutor(t *testing.T) {
	l := labtest.New(t)
	tn := l.Tenant()
	size := int64(1 << 30)
	want := tn.Put("a", "obj", size)
	slow(l, "c", "dest-1")
	c := tn.Clients("veho-b1")
	j := tn.Submit(c, "a", "obj", tn.Policy("obj", cDest, nil))
	tr := copying(t, l, j.GetId(), "c-dest-1", 2)
	before := map[string]float64{}
	for _, n := range l.Nodes() {
		if n != tr.Owner {
			before[n] = l.Metric(n, "veho_transfer_bytes_total")
		}
	}
	committed := transfers(t, l, j.GetId())["c-dest-1"].Parts
	if tr.Owner == "veho-b1" { // follow the job through a node that stays up
		c = tn.Clients("veho-c1")
	}
	killed := time.Now()
	l.Kill(tr.Owner, true)
	until(t, lease+40*time.Second, "adoption", func() bool {
		o := transfers(t, l, j.GetId())["c-dest-1"].Owner
		return o != "" && o != tr.Owner
	})
	t.Logf("adopted %v after the kill", time.Since(killed).Round(time.Second))
	if j := tn.Wait(c, j.GetId(), 15*time.Minute); true {
		settled(t, tn, j, want, "obj", cDest)
	}
	copied := 0.0
	for n, b := range before {
		copied += l.Metric(n, "veho_transfer_bytes_total") - b
	}
	// the adopter copies what was not committed, and at most the batch
	// (parts_in_flight 2) that was in flight at the kill
	if limit := float64(size - int64(committed)*part + 2*part); copied > limit {
		t.Errorf("the other nodes copied %.0f bytes after the kill, more than %.0f: the work was not resumed", copied, limit)
	}
	if got := transfers(t, l, j.GetId())["c-dest-1"]; got.Parts != got.Distinct {
		t.Errorf("parts recorded twice: %+v", got)
	}
}

// AC-3: kill the node that answered 202 at once; the job still completes.
func TestAC03KillAcceptingNode(t *testing.T) {
	l := labtest.New(t)
	tn := l.Tenant()
	want := tn.Put("a", "obj", 256*mib)
	j := tn.Submit(tn.Clients("veho-a1"), "a", "obj", tn.Policy("obj", bc, nil))
	l.Kill("veho-a1", true)
	settled(t, tn, tn.Wait(tn.Clients("veho-b1"), j.GetId(), 10*time.Minute), want, "obj", bc)
}

// AC-4: stop both nodes of region a while jobs from a's source run; the
// nodes of b and c finish them.
func TestAC04RegionNodesDown(t *testing.T) {
	l := labtest.New(t)
	tn := l.Tenant()
	want := tn.Put("a", "obj", 512*mib)
	slow(l, "b", "dest-1")
	c := tn.Clients("veho-b1")
	var jobs []string
	for range 2 {
		jobs = append(jobs, tn.Submit(c, "a", "obj", tn.Policy("obj", bDest, nil)).GetId())
	}
	for _, id := range jobs {
		copying(t, l, id, "b-dest-1", 1)
	}
	l.Stop("veho-a1")
	l.Stop("veho-a2")
	for _, id := range jobs {
		settled(t, tn, tn.Wait(c, id, 15*time.Minute), want, "obj", bDest)
	}
}

// AC-5: kill the Patroni leader's container during transfers: a new leader
// within 5 minutes, no job lost, every part recorded once.
func TestAC05DatabaseFailover(t *testing.T) {
	l := labtest.New(t)
	tn := l.Tenant()
	want := tn.Put("a", "obj", 1<<30)
	slow(l, "c", "dest-1")
	c := tn.Clients("veho-b1")
	j := tn.Submit(c, "a", "obj", tn.Policy("obj", cDest, nil))
	copying(t, l, j.GetId(), "c-dest-1", 2)
	old := l.Leader()
	if old == "" {
		t.Fatal("no leader")
	}
	killed := time.Now()
	l.KillDB(strings.TrimPrefix(old, "db-"))
	until(t, 5*time.Minute, "a new leader", func() bool { n := l.Leader(); return n != "" && n != old })
	t.Logf("new leader %s after %v", l.Leader(), time.Since(killed).Round(time.Second))
	settled(t, tn, tn.Wait(c, j.GetId(), 15*time.Minute), want, "obj", cDest)
	if got := transfers(t, l, j.GetId())["c-dest-1"]; got.Parts != got.Distinct {
		t.Errorf("parts recorded twice: %+v", got)
	}
}

// AC-6: partition region a. A node in a refuses submissions as unavailable,
// b and c go on, the transfer a's node held is taken over after its lease,
// and there is one right object after the heal.
func TestAC06PartitionRegion(t *testing.T) {
	l := labtest.New(t)
	tn := l.Tenant()
	want := tn.Put("b", "obj", 1<<30)
	slow(l, "c", "dest-1")
	// only a's nodes run, so that one of them owns the transfer
	others := []string{"veho-b1", "veho-b2", "veho-c1", "veho-c2"}
	for _, n := range others {
		l.Stop(n)
	}
	a := tn.Clients("veho-a1")
	j := tn.Submit(a, "b", "obj", tn.Policy("obj", cDest, nil))
	held := copying(t, l, j.GetId(), "c-dest-1", 1)
	if !strings.HasPrefix(held.Owner, "veho-a") {
		t.Fatalf("the transfer is owned by %s, not a node of a", held.Owner)
	}
	for _, n := range others {
		l.Start(n)
	}
	l.Partition("a")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_, err := a.Jobs.CreateJob(ctx, &vehov1.CreateJobRequest{Source: &vehov1.ObjectRef{Endpoint: "b-source", Path: tn.Prefix + "obj"},
		Policy: tn.Policy("obj2", cDest, nil)})
	if status.Code(err) != codes.Unavailable {
		t.Errorf("submit in the partitioned region: %v, want Unavailable", err)
	}
	b := tn.Clients("veho-b1")
	until(t, lease+60*time.Second, "takeover", func() bool {
		o := transfers(t, l, j.GetId())["c-dest-1"].Owner
		return o != "" && !strings.HasPrefix(o, "veho-a")
	})
	settled(t, tn, tn.Wait(b, j.GetId(), 15*time.Minute), want, "obj", cDest)
	l.Sh("wan", "heal")
	time.Sleep(lease) // a's node may now finish its in-flight step: the fence refuses its writes
	if got := tn.SHA256("c", "dest-1", "obj"); got != want {
		t.Errorf("after the heal: sha256 %s, want %s", got, want)
	}
	if got := transfers(t, l, j.GetId())["c-dest-1"]; got.State != "SUCCEEDED" || got.Parts != got.Distinct {
		t.Errorf("after the heal: %+v", got)
	}
}

// AC-7: SIGSTOP the executor past its lease, then SIGCONT: its writes are
// fenced, another node finishes, one right object.
func TestAC07Zombie(t *testing.T) {
	l := labtest.New(t)
	tn := l.Tenant()
	want := tn.Put("a", "obj", 1<<30)
	slow(l, "c", "dest-1")
	c := tn.Clients("veho-b1")
	j := tn.Submit(c, "a", "obj", tn.Policy("obj", cDest, nil))
	tr := copying(t, l, j.GetId(), "c-dest-1", 2)
	if tr.Owner == "veho-b1" {
		c = tn.Clients("veho-c1")
	}
	l.Pause(tr.Owner)
	until(t, lease+60*time.Second, "takeover", func() bool {
		o := transfers(t, l, j.GetId())["c-dest-1"].Owner
		return o != "" && o != tr.Owner
	})
	l.Resume(tr.Owner)
	settled(t, tn, tn.Wait(c, j.GetId(), 15*time.Minute), want, "obj", cDest)
	time.Sleep(10 * time.Second) // the zombie's last step, if any, is refused
	if got := tn.SHA256("c", "dest-1", "obj"); got != want {
		t.Errorf("after the zombie resumed: sha256 %s, want %s", got, want)
	}
	if got := transfers(t, l, j.GetId())["c-dest-1"]; got.State != "SUCCEEDED" || got.Parts != got.Distinct {
		t.Errorf("after the zombie resumed: %+v", got)
	}
}

// outage is AC-8's: LAB_AC8_OUTAGE, default 3 minutes (30 in the "long"
// campaign).
func outage(t *testing.T) time.Duration {
	if s := os.Getenv("LAB_AC8_OUTAGE"); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			t.Fatalf("LAB_AC8_OUTAGE: %v", err)
		}
		return d
	}
	return 3 * time.Minute
}

// AC-8: the destination answers 503 for the outage: retried with backoff,
// complete after it.
func TestAC08DestinationUnavailable(t *testing.T) {
	l := labtest.New(t)
	tn := l.Tenant()
	want := tn.Put("a", "obj", 128*mib)
	d := outage(t)
	l.Fault("c", "status", "503", "--for", d.String(), "--bucket", "dest-1")
	start := time.Now()
	c := tn.Clients("veho-b1")
	j := tn.Submit(c, "a", "obj", tn.Policy("obj", cDest, map[string]any{"retry": map[string]any{"initial": "5s", "max_interval": "30s"}}))
	got := tn.Wait(c, j.GetId(), d+10*time.Minute)
	settled(t, tn, got, want, "obj", cDest)
	if got.GetSettledAt().AsTime().Before(start.Add(d)) {
		t.Errorf("settled at %v, during the outage", got.GetSettledAt().AsTime())
	}
}

// AC-9 (amended): a 403 fails that transfer Permanent. A policy still
// satisfiable leaves the other destination alone; one that cannot be met
// fails at once and cancels the rest (fail fast, DP-6); a retry after the
// fix restarts both and succeeds.
func TestAC09DestinationForbidden(t *testing.T) {
	l := labtest.New(t)
	tn := l.Tenant()
	want := tn.Put("a", "obj", 128*mib)
	l.Fault("c", "status", "403", "--bucket", "dest-1")
	c := tn.Clients("veho-b1")
	dests := append(append([]labtest.Dest{}, bDest...), cDest...)

	one := tn.Submit(c, "a", "obj", tn.Policy("one", dests, map[string]any{"completion": map[string]any{"min_copies": 1}}))
	got := tn.Wait(c, one.GetId(), 10*time.Minute)
	until(t, 2*time.Minute, "the 403 transfer settled", func() bool { return transfers(t, l, one.GetId())["c-dest-1"].State == "FAILED" })
	trs := transfers(t, l, one.GetId())
	if got.GetState() != "SUCCEEDED" || trs["b-dest-1"].State != "SUCCEEDED" || trs["c-dest-1"].Class != "Permanent" {
		t.Errorf("min_copies 1: job %s, transfers %+v", got.GetState(), trs)
	}
	if sha := tn.SHA256("b", "dest-1", "one"); sha != want {
		t.Errorf("b-dest-1: sha256 %s, want %s", sha, want)
	}

	all := tn.Submit(c, "a", "obj", tn.Policy("obj", dests, nil))
	got = tn.Wait(c, all.GetId(), 10*time.Minute)
	trs = transfers(t, l, all.GetId())
	if got.GetState() != "FAILED" || trs["c-dest-1"].State != "FAILED" || trs["c-dest-1"].Class != "Permanent" || trs["b-dest-1"].State != "CANCELLED" {
		t.Fatalf("all required: job %s, transfers %+v", got.GetState(), trs)
	}
	l.Sh("fault", "c", "clear")
	if _, err := c.Jobs.RetryJob(context.Background(), &vehov1.JobRef{Id: all.GetId()}); err != nil {
		t.Fatalf("retry: %v", err)
	}
	settled(t, tn, tn.Wait(c, all.GetId(), 10*time.Minute), want, "obj", dests)
}

// AC-10: a part corrupted on its way is refused by the destination and
// written again; the object is right.
func TestAC10CorruptPart(t *testing.T) {
	l := labtest.New(t)
	tn := l.Tenant()
	want := tn.Put("a", "obj", 256*mib)
	l.Fault("c", "corrupt", "--count", "1", "--bucket", "dest-1")
	start := time.Now()
	c := tn.Clients("veho-b1")
	j := tn.Submit(c, "a", "obj", tn.Policy("obj", cDest, nil))
	settled(t, tn, tn.Wait(c, j.GetId(), 10*time.Minute), want, "obj", cDest)
	log := l.Sh("ssh", "s3-c", "journalctl", "-u", "faultproxy", "--since", "@"+itoa(start.Unix()), "--no-pager")
	if !strings.Contains(log, "mode=corrupt") {
		t.Errorf("the proxy corrupted nothing:\n%s", log)
	}
}

// AC-11 (lab variant): the source is overwritten mid-copy. The transfer
// never publishes a mix: it fails SourceChanged, or the copy is the new
// object's.
func TestAC11SourceChanged(t *testing.T) {
	l := labtest.New(t)
	tn := l.Tenant()
	tn.Put("a", "obj", 1<<30)
	slow(l, "c", "dest-1")
	c := tn.Clients("veho-b1")
	j := tn.Submit(c, "a", "obj", tn.Policy("obj", cDest, nil))
	copying(t, l, j.GetId(), "c-dest-1", 1)
	// a different size and content under the same name
	newSHA := tn.PutSeed("a", "obj", 512*mib, "v2")
	got := tn.Wait(c, j.GetId(), 15*time.Minute)
	tr := transfers(t, l, j.GetId())["c-dest-1"]
	switch {
	case got.GetState() == "FAILED" && tr.Class == "SourceChanged":
	case got.GetState() == "SUCCEEDED" && tn.SHA256("c", "dest-1", "obj") == newSHA:
	default:
		t.Errorf("job %s, transfer %+v, destination sha256 %s (new %s)", got.GetState(), tr, tn.SHA256("c", "dest-1", "obj"), newSHA)
	}
}

// AC-12 (amended, veho-lab-l3.md §4): with a's S3 down after c completed, b's
// transfer retries and completes when a returns.
func TestAC12SourceDownAfterOneCopy(t *testing.T) {
	l := labtest.New(t)
	tn := l.Tenant()
	want := tn.Put("a", "obj", 128*mib)
	l.Fault("b", "status", "503", "--bucket", "dest-1")
	c := tn.Clients("veho-c1")
	j := tn.Submit(c, "a", "obj", tn.Policy("obj", bc, map[string]any{"retry": map[string]any{"initial": "5s", "max_interval": "20s"}}))
	until(t, 5*time.Minute, "the copy to c", func() bool { return transfers(t, l, j.GetId())["c-dest-1"].State == "SUCCEEDED" })
	l.Fault("a", "status", "503", "--bucket", "source")
	l.Sh("fault", "b", "clear")
	time.Sleep(time.Minute)
	if tr := transfers(t, l, j.GetId())["b-dest-1"]; tr.State == "SUCCEEDED" || tr.State == "FAILED" {
		t.Fatalf("b's transfer ended while the source was down: %+v", tr)
	}
	l.Sh("fault", "a", "clear")
	settled(t, tn, tn.Wait(c, j.GetId(), 10*time.Minute), want, "obj", bc)
}

// AC-13 (lab variant): the same idempotency key on two nodes gives one job.
func TestAC13IdempotencyAcrossNodes(t *testing.T) {
	l := labtest.New(t)
	tn := l.Tenant()
	tn.Put("a", "obj", mib)
	var ids []string
	for _, n := range []string{"veho-a1", "veho-c2"} {
		j, err := tn.Clients(n).Jobs.CreateJob(context.Background(), &vehov1.CreateJobRequest{IdempotencyKey: "k1",
			Source: &vehov1.ObjectRef{Endpoint: "a-source", Path: tn.Prefix + "obj"}, Policy: tn.Policy("obj", cDest, nil)})
		if err != nil {
			t.Fatalf("%s: %v", n, err)
		}
		ids = append(ids, j.GetId())
	}
	if ids[0] != ids[1] {
		t.Errorf("two jobs: %v", ids)
	}
	var n int
	if err := l.DB().QueryRow(context.Background(), "select count(*) from veho.jobs j join veho.tenants t on t.id = j.tenant_id where t.name = $1", tn.Name).Scan(&n); err != nil || n != 1 {
		t.Errorf("%d jobs in the tenant: %v", n, err)
	}
}

// AC-15: at its quota of active jobs a tenant's submission is refused as
// resource_exhausted (429); the running job is unaffected.
func TestAC15Quota(t *testing.T) {
	l := labtest.New(t)
	tn := l.Tenant()
	want := tn.Put("a", "obj", 256*mib)
	if _, err := l.DB().Exec(context.Background(), `update veho.tenants set quotas = '{"max_active_jobs": 1}' where name = $1`, tn.Name); err != nil {
		t.Fatal(err)
	}
	slow(l, "c", "dest-1")
	c := tn.Clients("veho-b1")
	j := tn.Submit(c, "a", "obj", tn.Policy("obj", cDest, nil))
	_, err := c.Jobs.CreateJob(context.Background(), &vehov1.CreateJobRequest{Source: &vehov1.ObjectRef{Endpoint: "a-source", Path: tn.Prefix + "obj"},
		Policy: tn.Policy("obj2", cDest, nil)})
	if status.Code(err) != codes.ResourceExhausted {
		t.Errorf("a second job: %v, want ResourceExhausted", err)
	}
	settled(t, tn, tn.Wait(c, j.GetId(), 10*time.Minute), want, "obj", cDest)
}

// AC-16: cancel mid-copy: CANCELLED, no object under the final name, no
// multipart upload left.
func TestAC16CancelMidTransfer(t *testing.T) {
	l := labtest.New(t)
	tn := l.Tenant()
	tn.Put("a", "obj", 512*mib)
	slow(l, "c", "dest-1")
	c := tn.Clients("veho-b1")
	j := tn.Submit(c, "a", "obj", tn.Policy("obj", cDest, nil))
	copying(t, l, j.GetId(), "c-dest-1", 1)
	if _, err := c.Jobs.CancelJob(context.Background(), &vehov1.JobRef{Id: j.GetId()}); err != nil {
		t.Fatal(err)
	}
	if got := tn.Wait(c, j.GetId(), 5*time.Minute); got.GetState() != "CANCELLED" {
		t.Fatalf("job %s", got.GetState())
	}
	until(t, 2*time.Minute, "the upload aborted", func() bool { return tn.Uploads("c", "dest-1") == 0 })
	if sha := tn.SHA256("c", "dest-1", "obj"); sha != "" {
		t.Errorf("an object under the final name: %s", sha)
	}
}

// all9 are the nine destinations: three buckets in each region.
func all9() []labtest.Dest {
	var ds []labtest.Dest
	for _, x := range labtest.Regions {
		for _, b := range labtest.Buckets[1:] {
			ds = append(ds, labtest.Dest{Region: x, Bucket: b})
		}
	}
	return ds
}

// AC-23 (lab variant): nine destinations with the source in b. min_copies 7
// succeeds; min_copies 5 with spread region 3 fails while c refuses every
// write and succeeds when it does not.
func TestAC23Distribution(t *testing.T) {
	l := labtest.New(t)
	tn := l.Tenant()
	want := tn.Put("b", "obj", 64*mib)
	c := tn.Clients("veho-a2")
	j := tn.Submit(c, "b", "obj", tn.Policy("obj", all9(), map[string]any{"completion": map[string]any{"min_copies": 7}}))
	got := tn.Wait(c, j.GetId(), 10*time.Minute)
	if got.GetState() != "SUCCEEDED" || got.GetSatisfiedAt() == nil || got.GetCopies().GetDone() < 7 {
		t.Errorf("min_copies 7: %s, copies %v", got.GetState(), got.GetCopies())
	}
	spread := map[string]any{"completion": map[string]any{"min_copies": 5, "spread": []any{map[string]any{"level": "region", "min_distinct": 3}}}}
	l.Fault("c", "status", "403")
	j = tn.Submit(c, "b", "obj", tn.Policy("spread-1", all9(), spread))
	if got := tn.Wait(c, j.GetId(), 10*time.Minute); got.GetState() != "FAILED" {
		t.Errorf("spread 3 with c refusing: %s", got.GetState())
	}
	l.Sh("fault", "c", "clear")
	j = tn.Submit(c, "b", "obj", tn.Policy("spread-2", all9(), spread))
	got = tn.Wait(c, j.GetId(), 10*time.Minute)
	if got.GetState() != "SUCCEEDED" {
		t.Errorf("spread 3: %s", got.GetState())
	}
	if sha := tn.SHA256("c", "dest-3", "spread-2"); got.GetState() == "SUCCEEDED" && sha != want && sha != "" {
		t.Errorf("c-dest-3: sha256 %s, want %s", sha, want)
	}
}

// AC-25 (lab): each node writes the audit events of the requests it handles
// to its own journal.
func TestAC25AuditJournal(t *testing.T) {
	l := labtest.New(t)
	tn := l.Tenant()
	tn.Put("a", "obj", mib)
	start := time.Now()
	for _, n := range []string{"veho-a1", "veho-b2", "veho-c1"} {
		j := tn.Submit(tn.Clients(n), "a", "obj", tn.Policy("obj-"+n, cDest, nil))
		var log string
		until(t, 30*time.Second, n+"'s audit event", func() bool {
			log = l.Sh("ssh", n, "journalctl", "-t", "veho-audit", "--since", "@"+itoa(start.Unix()), "--no-pager")
			return strings.Contains(log, j.GetId())
		})
		if !strings.Contains(log, `"action":"JOB_SUBMIT"`) {
			t.Errorf("%s: no JOB_SUBMIT event:\n%s", n, log)
		}
	}
}

// AC-26 (lab): a dump of the database holds neither a secret's value nor the
// unwrap key.
func TestAC26NoPlaintextInDump(t *testing.T) {
	l := labtest.New(t)
	tn := l.Tenant()
	canary := "canary-" + itoa(time.Now().UnixNano())
	if _, err := tn.Clients("veho-a1").Secrets.PutSecret(context.Background(), &vehov1.PutSecretRequest{Name: "canary", Value: []byte(canary)}); err != nil {
		t.Fatal(err)
	}
	leader := l.Leader()
	dump := l.Sh("ssh", leader, "runuser", "-u", "postgres", "--", "pg_dump", "veho")
	if !strings.Contains(dump, "veho.secrets") {
		t.Fatalf("the dump has no secrets table: %d bytes", len(dump))
	}
	key, err := os.ReadFile(filepath.Join(l.StateDir(), "unwrap.key"))
	if err != nil {
		t.Fatal(err)
	}
	for name, needle := range map[string][]byte{"the canary": []byte(canary), "the unwrap key": key, "the unwrap key in hex": []byte(hexOf(key))} {
		if bytes.Contains([]byte(dump), needle) {
			t.Errorf("the dump contains %s", name)
		}
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func hexOf(b []byte) string { return hex.EncodeToString(b) }

// AC-20: rolling upgrades with transfers in flight. A build of the same
// workflow version replaces the nodes one at a time and the work goes on. A
// build of another workflow version replaces four nodes, the owner of a
// running transfer among them: DBOS gives each version only its own work, so
// the restarted owner hands its old work back to the queue (recoverOwn), the
// two old nodes finish it, the new nodes run the new jobs, and once nothing
// of the old version is left the last old nodes are upgraded.
func TestAC20RollingUpgrade(t *testing.T) {
	l := labtest.New(t)
	t.Cleanup(func() { l.Sh("nodes", "deploy") }) // the current build everywhere, for the next tests
	tn := l.Tenant()
	want := tn.Put("a", "obj", 512*mib)
	slow(l, "c", "dest-1")
	old := tn.Clients("veho-c2")

	// a compatible build, node by node, mid-transfer
	j := tn.Submit(old, "a", "obj", tn.Policy("compat", cDest, nil))
	copying(t, l, j.GetId(), "c-dest-1", 1)
	l.Sh("nodes", "deploy")
	settled(t, tn, tn.Wait(old, j.GetId(), 15*time.Minute), want, "compat", cDest)

	// an incompatible build on four nodes, the transfer's owner among them:
	// its old work must go back to the two old nodes
	j1 := tn.Submit(old, "a", "obj", tn.Policy("v1", cDest, nil))
	owner := copying(t, l, j1.GetId(), "c-dest-1", 1).Owner
	var keep, upgrade []string
	for _, n := range l.Nodes() {
		if n != owner && len(keep) < 2 {
			keep = append(keep, n)
		} else {
			upgrade = append(upgrade, n)
		}
	}
	old = tn.Clients(keep[0])
	l.Sh(append([]string{"nodes", "deploy", "--workflow-version", "2"}, upgrade...)...)
	upgraded := tn.Clients(upgrade[0])
	j2 := tn.Submit(upgraded, "a", "obj", tn.Policy("v2", cDest, nil))
	settled(t, tn, tn.Wait(old, j1.GetId(), 15*time.Minute), want, "v1", cDest)
	settled(t, tn, tn.Wait(upgraded, j2.GetId(), 15*time.Minute), want, "v2", cDest)
	// a job settles a moment before DBOS records the end of its workflow
	drained := func(version, job string) int {
		n := l.Pending(version, job)
		for deadline := time.Now().Add(30 * time.Second); n != 0 && time.Now().Before(deadline); n = l.Pending(version, job) {
			time.Sleep(time.Second)
		}
		return n
	}
	if n := drained("1", j1.GetId()); n != 0 {
		t.Fatalf("%d workflows of version 1 still pending 30 s after their job settled", n)
	}
	if n := drained("2", j2.GetId()); n != 0 {
		t.Fatalf("%d workflows of version 2 still pending 30 s after their job settled", n)
	}
	// nothing of version 1 is left: the last old nodes can go
	l.Sh(append([]string{"nodes", "deploy", "--workflow-version", "2"}, keep...)...)
	j3 := tn.Submit(upgraded, "a", "obj", tn.Policy("v2-all", cDest, nil))
	settled(t, tn, tn.Wait(upgraded, j3.GetId(), 15*time.Minute), want, "v2-all", cDest)
}
