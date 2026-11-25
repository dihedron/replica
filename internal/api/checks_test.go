package api

import (
	"testing"
	"time"

	"github.com/dihedron/veho/internal/engine"
	"github.com/dihedron/veho/internal/flowcheck"
	"github.com/dihedron/veho/pkg/plugin"
	"uuid"
)

// NC-T4: a check's flows and each node's results reach the API as they
// are; a node that did not answer has no time.
func TestToCheck(t *testing.T) {
	now := time.Now()
	c := engine.EndpointCheck{ID: uuid.New(), Endpoint: "e", Created: now, State: engine.CheckExpired,
		Flows: []plugin.Flow{{Purpose: "s3", Host: "s3.example", Port: 443, TLS: true, ServerName: "s3.example"}},
		Nodes: []engine.NodeCheck{
			{Node: "a1", Region: "a", Answered: true, Checked: now, Results: []flowcheck.Result{
				{Purpose: "s3", Host: "s3.example", Port: 443, Address: "192.0.2.7", Protocol: "tls", Status: flowcheck.Unreachable, Reason: "timeout", LatencyMS: 5}}},
			{Node: "b1"},
		}}
	out := toCheck(c)
	f, r := out.GetFlows()[0], out.GetNodes()[0].GetResults()[0]
	if out.GetId() != c.ID.String() || out.GetState() != "expired" || f.GetHost() != "s3.example" || f.GetPort() != 443 || !f.GetTls() ||
		r.GetAddress() != "192.0.2.7" || r.GetStatus() != "unreachable" || r.GetLatencyMs() != 5 ||
		out.GetNodes()[0].GetCheckedAt() == nil || out.GetNodes()[1].GetCheckedAt() != nil {
		t.Fatalf("%+v", out)
	}
}
