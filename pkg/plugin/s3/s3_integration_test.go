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
				Driver:         lab(t),
				Config:         cfg,
				Credentials:    plugin.Credentials{"access_key_id": env("ACCESS_KEY"), "secret_access_key": env("SECRET_KEY")},
				BadCredentials: plugin.Credentials{"access_key_id": env("ACCESS_KEY"), "secret_access_key": strings.Repeat("0", 64)},
				Prefix:         "conformance/",
			})
		})
	}
}

// TestGarageStorageClass records what region A's Garage does with a storage
// class (SC-T5, veho-storage-class.md): STANDARD must commit; the outcome of
// STANDARD_IA is logged for the Operator's Guide.
func TestGarageStorageClass(t *testing.T) {
	env := func(name string) string { return os.Getenv("VEHO_TEST_S3_A_" + name) }
	if env("ENDPOINT") == "" {
		t.Skip("VEHO_TEST_S3_A_ENDPOINT is not set: run with test/lab/lab.sh test")
	}
	ctx := t.Context()
	for _, class := range []string{"STANDARD", "STANDARD_IA"} {
		cfg, _ := json.Marshal(config{Endpoint: env("ENDPOINT"), Region: env("REGION"), Bucket: "dest-1", PathStyle: true, StorageClass: class})
		c, err := lab(t).Open(ctx, cfg, plugin.Credentials{"access_key_id": env("ACCESS_KEY"), "secret_access_key": env("SECRET_KEY")})
		if err != nil {
			t.Fatal(err)
		}
		ref := plugin.Ref{Key: "storage-class/" + class}
		up, err := c.Begin(ctx, ref, plugin.ObjectInfo{}, plugin.PutOptions{})
		var part plugin.PartResult
		if err == nil {
			part, err = up.WritePart(ctx, 0, strings.NewReader(strings.Repeat("x", 1024)), 1024)
		}
		if err == nil {
			_, err = up.Commit(ctx, []plugin.PartResult{part})
		}
		t.Logf("Garage, storage class %s: %v", class, err)
		if class == "STANDARD" && err != nil {
			t.Error(err)
		}
		_ = c.Delete(ctx, ref)
		_ = c.Close()
	}
}

// lab is the driver allowed to the lab's Garage, on its private network.
func lab(t *testing.T) plugin.Driver {
	t.Helper()
	d, err := Factory(json.RawMessage(`{"allow_cidrs": ["10.0.0.0/8"]}`))
	if err != nil {
		t.Fatal(err)
	}
	return d
}
