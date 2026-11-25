//go:build integration

package engine

import (
	"context"
	"testing"
	"time"
)

// NC-T7: the periodic task checks the endpoints due, oldest first, at most
// per_pass at a time; none checked within the interval; none with the
// interval 0; a check that could not find its flows waits for the interval
// too.
func TestPeriodicChecks(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	if err := f.e.Heartbeat(ctx); err != nil {
		t.Fatal(err)
	}
	// the other tests' tenants: just checked, so this tenant's are the only due
	if _, err := f.migrator.Exec(ctx, `insert into veho.endpoint_checks (tenant_id, endpoint_id, nodes)
		select tenant_id, id, '{}' from veho.endpoints where tenant_id <> $1`, f.tenant); err != nil {
		t.Fatal(err)
	}
	checked := func() map[string]int {
		t.Helper()
		rows, err := f.migrator.Query(ctx, `select e.name, count(c.id) from veho.endpoints e left join veho.endpoint_checks c on c.endpoint_id = e.id
			where e.tenant_id = $1 group by e.name`, f.tenant)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]int{}
		for rows.Next() {
			var n string
			var k int
			if err := rows.Scan(&n, &k); err != nil {
				t.Fatal(err)
			}
			out[n] = k
		}
		return out
	}
	total := len(checked())
	interval := 24 * time.Hour
	f.e.cfg.NetworkChecks.Interval, f.e.cfg.NetworkChecks.PerPass = &interval, 3

	if err := f.e.periodicChecks(ctx); err != nil {
		t.Fatal(err)
	}
	first := checked()
	n := 0
	for _, k := range first {
		n += k
	}
	if n != 3 {
		t.Fatalf("one pass: %d checks, want 3: %v", n, first)
	}
	for pass := 0; pass < total; pass++ {
		if err := f.e.periodicChecks(ctx); err != nil {
			t.Fatal(err)
		}
	}
	for name, k := range checked() {
		if k != 1 { // every endpoint once, "gone" (no driver) included, none twice
			t.Errorf("%s: %d checks after every pass", name, k)
		}
	}
	// an endpoint checked longer ago than the interval is due again
	if _, err := f.migrator.Exec(ctx, `update veho.endpoint_checks c set created_at = now() - interval '25 hours'
		from veho.endpoints e where e.id = c.endpoint_id and e.tenant_id = $1 and e.name = 'gone'`, f.tenant); err != nil {
		t.Fatal(err)
	}
	if err := f.e.periodicChecks(ctx); err != nil {
		t.Fatal(err)
	}
	if got := checked(); got["gone"] != 2 || got["src"] != 1 {
		t.Fatalf("after the interval: %v", got)
	}
	// off
	zero := time.Duration(0)
	f.e.cfg.NetworkChecks.Interval = &zero
	if _, err := f.migrator.Exec(ctx, `update veho.endpoint_checks c set created_at = now() - interval '25 hours'
		from veho.endpoints e where e.id = c.endpoint_id and e.tenant_id = $1`, f.tenant); err != nil {
		t.Fatal(err)
	}
	if err := f.e.periodicChecks(ctx); err != nil {
		t.Fatal(err)
	}
	if got := checked(); got["src"] != 1 {
		t.Fatalf("interval 0: %v", got)
	}
}

// NC-T8: the gauge counts the endpoints whose latest complete check has an
// unreachable flow; a check still running does not count.
func TestUnreachableGauge(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	count := func() int64 {
		t.Helper()
		var n int64
		if err := f.worker.QueryRow(ctx, "select veho.unreachable_endpoints()").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := count()
	add := func(name string, nodes []string, status string) {
		t.Helper()
		var id string
		if err := f.migrator.QueryRow(ctx, `insert into veho.endpoint_checks (tenant_id, endpoint_id, nodes)
			select tenant_id, id, $3 from veho.endpoints where tenant_id = $1 and name = $2 returning id`, f.tenant, name, nodes).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if status != "" {
			if _, err := f.migrator.Exec(ctx, `insert into veho.endpoint_check_results (check_id, tenant_id, node_id, region, results)
				values ($1, $2, 'n1', 'lab-a', jsonb_build_array(jsonb_build_object('status', $3::text)))`, id, f.tenant, status); err != nil {
				t.Fatal(err)
			}
		}
	}
	add("d1", []string{"n1"}, "unreachable")        // complete, unreachable: counts
	add("d2", []string{"n1"}, "reachable")          // complete, reachable
	add("src", []string{"n1", "n2"}, "unreachable") // running: does not count
	if got := count() - before; got != 1 {
		t.Fatalf("unreachable endpoints: %d more, want 1", got)
	}
	add("d1", []string{"n1"}, "reachable") // the latest complete check is fine now
	if got := count() - before; got != 0 {
		t.Fatalf("after a good check: %d more, want 0", got)
	}
}
