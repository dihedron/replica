//go:build integration

package process

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/dihedron/veho/pkg/plugin"
	"github.com/dihedron/veho/pkg/plugin/conformance"
)

// TestConformanceS3: the S3 driver, out of process, passes the suite on the
// lab's Garage of region B.
func TestConformanceS3(t *testing.T) {
	env := func(name string) string { return os.Getenv("VEHO_TEST_S3_B_" + name) }
	if env("ENDPOINT") == "" {
		t.Skip("VEHO_TEST_S3_B_ENDPOINT is not set: run with test/lab/lab.sh test")
	}
	cfg, _ := json.Marshal(map[string]any{"endpoint": env("ENDPOINT"), "region": env("REGION"), "bucket": "dest-2", "path_style": true})
	conformance.Run(t, conformance.Target{
		Driver:         load(t, "s3").Driver(),
		Config:         cfg,
		Credentials:    plugin.Credentials{"access_key_id": env("ACCESS_KEY"), "secret_access_key": env("SECRET_KEY")},
		BadCredentials: plugin.Credentials{"access_key_id": env("ACCESS_KEY"), "secret_access_key": strings.Repeat("0", 64)},
		Prefix:         "conformance-process/",
	})
}
