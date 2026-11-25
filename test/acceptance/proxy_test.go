//go:build acceptance

package acceptance

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	vehov1 "github.com/dihedron/veho/api/veho/v1"
	"github.com/dihedron/veho/test/labtest"
	"google.golang.org/protobuf/types/known/structpb"
)

// PX-T5 (veho-proxy.md §8): an S3 that only the proxy resolves and
// reaches (lab.sh proxy up, then nodes deploy): a job from region a to it
// and one back, its check from the six nodes through the proxy, and the
// same S3 by its address refused without the proxy seeing a request.
func TestProxy(t *testing.T) {
	if os.Getenv("VEHO_TEST_OUTSIDE_ENDPOINT") == "" {
		t.Skip("no proxy node: test/lab/lab.sh proxy up, then lab.sh acceptance")
	}
	l := labtest.New(t)
	tn := l.Tenant()
	c := tn.Clients("veho-b1")
	ctx := context.Background()
	secret, _ := json.Marshal(map[string]string{"access_key_id": os.Getenv("VEHO_TEST_OUTSIDE_ACCESS_KEY"),
		"secret_access_key": os.Getenv("VEHO_TEST_OUTSIDE_SECRET_KEY")})
	if _, err := c.Secrets.PutSecret(ctx, &vehov1.PutSecretRequest{Name: "s3-outside", Value: secret}); err != nil {
		t.Fatal(err)
	}
	for name, ep := range map[string][2]string{
		"outside-dest-1":   {os.Getenv("VEHO_TEST_OUTSIDE_ENDPOINT"), "dest-1"},
		"outside-source":   {os.Getenv("VEHO_TEST_OUTSIDE_ENDPOINT"), "dest-1"}, // where the first job writes
		"outsideip-dest-1": {os.Getenv("VEHO_TEST_OUTSIDE_IP_ENDPOINT"), "dest-1"},
	} {
		cfg, _ := structpb.NewStruct(map[string]any{"endpoint": ep[0], "region": "outside", "bucket": ep[1], "path_style": true})
		if _, err := c.Endpoints.CreateEndpoint(ctx, &vehov1.Endpoint{Name: name, Driver: "s3", Location: "region-a",
			Config: cfg, CredentialRef: "db://s3-outside"}); err != nil {
			t.Fatalf("create endpoint %s: %v", name, err)
		}
	}

	want := tn.Put("a", "obj", 24*mib+7)
	out := tn.Wait(c, tn.Submit(c, "a", "obj", tn.Policy("obj", []labtest.Dest{{Region: "outside", Bucket: "dest-1"}}, nil)).GetId(), 5*time.Minute)
	if out.GetState() != "SUCCEEDED" {
		t.Fatalf("to the outside S3: %v", out.GetState())
	}
	back := tn.Wait(c, tn.Submit(c, "outside", "obj", tn.Policy("back", []labtest.Dest{{Region: "a", Bucket: "dest-2"}}, nil)).GetId(), 5*time.Minute)
	if back.GetState() != "SUCCEEDED" {
		t.Fatalf("back from the outside S3: %v", back.GetState())
	}
	if got := tn.SHA256("a", "dest-2", "back"); got != want {
		t.Fatalf("the round trip changed the object: %s, want %s", got, want)
	}

	chk := checkFrom(t, c, "outside-dest-1")
	if len(chk.GetNodes()) != len(l.Nodes()) {
		t.Fatalf("answered by %d nodes", len(chk.GetNodes()))
	}
	for _, n := range chk.GetNodes() {
		r := n.GetResults()
		if len(r) != 1 || r[0].GetStatus() != "reachable" || r[0].GetProtocol() != "tls via proxy" {
			t.Errorf("%s: %+v", n.GetNode(), r)
		}
	}

	before := l.Sh("proxy", "requests")
	byIP := tn.Wait(c, tn.Submit(c, "a", "obj", tn.Policy("ip", []labtest.Dest{{Region: "outsideip", Bucket: "dest-1"}}, nil)).GetId(), 5*time.Minute)
	if byIP.GetState() != "FAILED" {
		t.Fatalf("by its address: %v", byIP.GetState())
	}
	if after := l.Sh("proxy", "requests"); strings.TrimSpace(after) != strings.TrimSpace(before) {
		t.Fatalf("the proxy saw a request for the address: %s, then %s", before, after)
	}
}
