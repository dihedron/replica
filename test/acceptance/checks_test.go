//go:build acceptance

package acceptance

import (
	"context"
	"testing"
	"time"

	vehov1 "github.com/dihedron/veho/api/veho/v1"
	"github.com/dihedron/veho/test/labtest"
)

// checkFrom runs a check of endpoint through node and waits until every
// executor answered.
func checkFrom(t *testing.T, c labtest.Clients, endpoint string) *vehov1.EndpointCheck {
	t.Helper()
	ctx := context.Background()
	chk, err := c.Endpoints.CheckEndpoint(ctx, &vehov1.EndpointName{Name: endpoint})
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(time.Minute); chk.GetState() == "running" && time.Now().Before(deadline); time.Sleep(time.Second) {
		if chk, err = c.Endpoints.GetEndpointCheck(ctx, &vehov1.EndpointCheckName{Name: endpoint, Id: chk.GetId()}); err != nil {
			t.Fatal(err)
		}
	}
	if chk.GetState() != "complete" {
		t.Fatalf("check %s of %s: %s", chk.GetId(), endpoint, chk.GetState())
	}
	return chk
}

// statusFrom is each node's status of the check's one flow.
func statusFrom(t *testing.T, chk *vehov1.EndpointCheck) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, n := range chk.GetNodes() {
		if len(n.GetResults()) != 1 {
			t.Fatalf("%s: %d results: %+v", n.GetNode(), len(n.GetResults()), n.GetResults())
		}
		out[n.GetNode()] = n.GetResults()[0].GetStatus()
	}
	return out
}

// NC-T5 (veho-network-checks.md §7): an endpoint on region a's Garage is
// reachable from the six nodes; a firewall rule on veho-c1 makes it
// unreachable from that node only; without the rule it is reachable again.
func TestNetworkChecks(t *testing.T) {
	l := labtest.New(t)
	tn := l.Tenant()
	c := tn.Clients("veho-b1")
	const endpoint = "a-dest-1" // http://10.60.1.20:3910, region a's fault proxy
	before := statusFrom(t, checkFrom(t, c, endpoint))
	if len(before) != len(l.Nodes()) {
		t.Fatalf("answered by %d nodes, want %d: %v", len(before), len(l.Nodes()), before)
	}
	for node, s := range before {
		if s != "reachable" {
			t.Fatalf("before the rule, %s: %s", node, s)
		}
	}

	// reject at once, so the probe fails fast rather than timing out
	l.Sh("ssh", "veho-c1", "sh", "-c", "nft add table inet veho_nc && "+
		"nft add chain inet veho_nc out '{ type filter hook output priority 0; policy accept; }' && "+
		"nft add rule inet veho_nc out ip daddr 10.60.1.20 tcp dport 3910 reject with tcp reset")
	t.Cleanup(func() { _, _ = l.TrySh("ssh", "veho-c1", "nft", "delete", "table", "inet", "veho_nc") })
	during := statusFrom(t, checkFrom(t, c, endpoint))
	for node, s := range during {
		if want := map[bool]string{true: "unreachable", false: "reachable"}[node == "veho-c1"]; s != want {
			t.Errorf("with the rule, %s: %s, want %s", node, s, want)
		}
	}

	l.Sh("ssh", "veho-c1", "nft", "delete", "table", "inet", "veho_nc")
	for node, s := range statusFrom(t, checkFrom(t, c, endpoint)) {
		if s != "reachable" {
			t.Errorf("after the rule, %s: %s", node, s)
		}
	}
}
