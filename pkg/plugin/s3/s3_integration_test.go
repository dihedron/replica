//go:build integration

package s3

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/dihedron/veho/pkg/plugin"
	"github.com/dihedron/veho/pkg/plugin/conformance"
)

// TestConformance runs the suite on each region's Garage of the lab
// (test/lab/lab.sh env), in the bucket dest-1.
func TestConformance(t *testing.T) {
	for _, region := range []string{"A", "B", "C"} {
		t.Run(region, func(t *testing.T) {
			env := func(name string) string { return os.Getenv("VEHO_TEST_S3_" + region + "_" + name) }
			if env("ENDPOINT") == "" {
				t.Skip("VEHO_TEST_S3_" + region + "_ENDPOINT is not set: run with test/lab/lab.sh test")
			}
			cfg, _ := json.Marshal(config{Endpoint: env("ENDPOINT"), Region: env("REGION"), Bucket: "dest-1", PathStyle: true})
			conformance.Run(t, conformance.Target{
				Driver:         New(),
				Config:         cfg,
				Credentials:    plugin.Credentials{"access_key_id": env("ACCESS_KEY"), "secret_access_key": env("SECRET_KEY")},
				BadCredentials: plugin.Credentials{"access_key_id": env("ACCESS_KEY"), "secret_access_key": strings.Repeat("0", 64)},
				Prefix:         "conformance/",
			})
		})
	}
}
