//go:build integration

package engine

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/dihedron/veho/internal/flowcheck"
)

// NC-T3: a check's life: created with the alive nodes and the endpoint's
// flows, one in flight per endpoint, answered, complete; expired with a
// node that never answers; the last ten kept.
func TestEndpointChecks(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	if err := f.e.Heartbeat(ctx); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	f.endpoint(t, "web", "http", "lab-a", map[string]string{"base_url": "http://" + l.Addr().String() + "/"}, "")
	only := func(id any, nodes ...string) {
		t.Helper()
		if _, err := f.migrator.Exec(ctx, "update veho.endpoint_checks set nodes = $2 where id = $1", id, nodes); err != nil {
			t.Fatal(err)
		}
	}

	c, err := f.e.CheckEndpoint(ctx, f.tenant, "web")
	if err != nil {
		t.Fatal(err)
	}
	if c.State != CheckRunning || len(c.Flows) != 1 || c.Flows[0].Purpose != "http" || !hasNode(c, f.e.executor) {
		t.Fatalf("created: %+v", c)
	}
	only(c.ID, f.e.executor)
	if again, err := f.e.CheckEndpoint(ctx, f.tenant, "web"); err != nil || again.ID != c.ID {
		t.Fatalf("one in flight: %v, %v", again.ID, err)
	}
	if err := f.e.answerChecks(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := f.e.GetEndpointCheck(ctx, f.tenant, "web", c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != CheckComplete || len(got.Nodes) != 1 || !got.Nodes[0].Answered || got.Nodes[0].Region != "lab-a" ||
		len(got.Nodes[0].Results) != 1 || got.Nodes[0].Results[0].Status != flowcheck.Reachable {
		t.Fatalf("answered: %+v", got)
	}

	// a node that never answers: expired once the window is over
	c2, err := f.e.CheckEndpoint(ctx, f.tenant, "web")
	if err != nil || c2.ID == c.ID {
		t.Fatalf("a new check: %v, %v", c2.ID, err)
	}
	only(c2.ID, f.e.executor, "ghost")
	if err := f.e.answerChecks(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.e.GetEndpointCheck(ctx, f.tenant, "web", c2.ID); got.State != CheckRunning {
		t.Fatalf("waiting for ghost: %+v", got)
	}
	if _, err := f.migrator.Exec(ctx, "update veho.endpoint_checks set created_at = now() - $2::interval where id = $1", c2.ID, checkWindow.String()); err != nil {
		t.Fatal(err)
	}
	got, _ = f.e.GetEndpointCheck(ctx, f.tenant, "web", c2.ID)
	if got.State != CheckExpired || len(got.Nodes) != 2 {
		t.Fatalf("expired: %+v", got)
	}
	for _, n := range got.Nodes {
		if n.Answered != (n.Node == f.e.executor) {
			t.Fatalf("expired, %s: %+v", n.Node, n)
		}
	}

	// the last ten, newest first
	for range 11 {
		ci, err := f.e.CheckEndpoint(ctx, f.tenant, "web")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.migrator.Exec(ctx, "update veho.endpoint_checks set created_at = created_at - $2::interval where id = $1", ci.ID, checkWindow.String()); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}
	all, err := f.e.ListEndpointChecks(ctx, f.tenant, "web")
	if err != nil || len(all) != keptChecks || all[0].Created.Before(all[len(all)-1].Created) {
		t.Fatalf("kept: %d, %v", len(all), err)
	}

	// an endpoint without flows: complete once answered, with no results
	s, err := f.e.CheckEndpoint(ctx, f.tenant, "src")
	if err != nil || len(s.Flows) != 0 {
		t.Fatalf("no flows: %+v, %v", s, err)
	}
	only(s.ID, f.e.executor)
	if err := f.e.answerChecks(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.e.GetEndpointCheck(ctx, f.tenant, "src", s.ID); got.State != CheckComplete {
		t.Fatalf("no flows, answered: %+v", got)
	}
	if _, err := f.e.CheckEndpoint(ctx, f.tenant, "nope"); err == nil {
		t.Fatal("an endpoint that does not exist")
	}
}

func hasNode(c EndpointCheck, node string) bool {
	for _, n := range c.Nodes {
		if n.Node == node {
			return true
		}
	}
	return false
}
