//go:build integration

package nexus

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/dihedron/veho/pkg/plugin"
	"github.com/dihedron/veho/pkg/plugin/conformance"
)

// NX-T4: the conformance suite against the lab's Nexus (lab.sh nexus up),
// and its refused redeploy and anonymous reading.
func TestLabNexus(t *testing.T) {
	url := os.Getenv("VEHO_TEST_NEXUS_URL")
	if url == "" {
		t.Skip("no Nexus node: test/lab/lab.sh nexus up")
	}
	ca, err := os.ReadFile(os.Getenv("VEHO_TEST_NEXUS_CA"))
	if err != nil {
		t.Fatal(err)
	}
	settings, _ := json.Marshal(settings{AllowCIDRs: []string{os.Getenv("VEHO_TEST_NEXUS_ALLOW")}})
	d, err := Factory(settings)
	if err != nil {
		t.Fatal(err)
	}
	cfg := func(repo, format string) plugin.Config {
		b, _ := json.Marshal(config{URL: url, Repository: repo, Format: format, CACert: string(ca)})
		return b
	}
	creds := plugin.Credentials{"username": os.Getenv("VEHO_TEST_NEXUS_USER"), "password": os.Getenv("VEHO_TEST_NEXUS_PASSWORD")}
	prefix := fmt.Sprintf("conformance-%d/", time.Now().UnixNano())
	conformance.Run(t, conformance.Target{
		Driver: d, Config: cfg("veho-raw", "raw"), Credentials: creds, Prefix: prefix, PartSize: 1 << 20, Confined: true,
		BadCredentials: plugin.Credentials{"username": "veho", "password": "wrong"},
	})

	ctx := context.Background()
	once, err := d.Open(ctx, cfg("veho-raw-once", ""), creds)
	if err != nil {
		t.Fatal(err)
	}
	key := prefix + "once"
	for i, want := range []plugin.Class{plugin.Unknown, plugin.Permanent} {
		if _, err := put(t, once, key, []byte(fmt.Sprintf("version %d", i))); plugin.ClassOf(err) != want && !(want == plugin.Unknown && err == nil) {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	anon, err := d.Open(ctx, cfg("veho-raw-once", ""), nil)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := anon.Stat(ctx, plugin.Ref{Key: key}); err != nil || info.Size != 9 {
		t.Fatalf("anonymous read: %+v, %v", info, err)
	}
	_ = once.Delete(ctx, plugin.Ref{Key: key})
}
