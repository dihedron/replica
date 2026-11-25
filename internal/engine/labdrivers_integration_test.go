//go:build integration

package engine

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/dihedron/veho/pkg/plugin"
	"github.com/dihedron/veho/pkg/plugin/glance"
	"github.com/dihedron/veho/pkg/plugin/http"
	"github.com/dihedron/veho/pkg/plugin/nexus"
	"github.com/dihedron/veho/pkg/plugin/s3"
	"github.com/dihedron/veho/pkg/plugin/swift"
)

// labS3 is the s3 driver allowed to the lab's Garage, on its private
// network (testConfig's drivers).
func labS3(t *testing.T) plugin.Driver {
	t.Helper()
	d, err := s3.Factory(json.RawMessage(`{"allow_cidrs": ["10.0.0.0/8"]}`))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// labDrivers adds http on the loopback and the drivers of the optional
// labs that are up: Nexus (lab.sh nexus up) and DevStack's Glance and
// Swift, with http allowed to the DevStack network for web-download's
// source.
func labDrivers(t *testing.T, drivers map[string]plugin.Driver) {
	t.Helper()
	add := func(name string, factory func(json.RawMessage) (plugin.Driver, error), allow ...string) {
		settings, _ := json.Marshal(map[string][]string{"allow_cidrs": allow})
		d, err := factory(settings)
		if err != nil {
			t.Fatal(err)
		}
		drivers[name] = d
	}
	if allow := os.Getenv("VEHO_TEST_NEXUS_ALLOW"); allow != "" {
		add("nexus", nexus.Factory, allow)
	}
	// http on the loopback (testConfig's drivers), and DevStack's network
	// for web-download's source
	add("http", http.Factory, "127.0.0.0/8")
	if allow := os.Getenv("VEHO_DEVSTACK_ALLOW_CIDR"); allow != "" {
		add("glance", glance.Factory, allow)
		add("swift", swift.Factory, allow)
		add("http", http.Factory, "127.0.0.0/8", allow)
	}
}
