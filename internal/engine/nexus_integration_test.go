//go:build integration

package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// NX-T5: end to end through the engine and the real drivers, with the
// lab's Nexus (lab.sh nexus up) and Garage: a file to Garage, Garage (S3)
// to Nexus, Nexus back to Garage; every job succeeds with the first one's
// digest, so the bytes went round unchanged.
func TestNexus(t *testing.T) {
	url := os.Getenv("VEHO_TEST_NEXUS_URL")
	if url == "" || os.Getenv("VEHO_TEST_S3_A_ENDPOINT") == "" {
		t.Skip("no Nexus node or no Garage: test/lab/lab.sh nexus up")
	}
	f := setup(t)
	ctx := context.Background()
	ca, err := os.ReadFile(os.Getenv("VEHO_TEST_NEXUS_CA"))
	if err != nil {
		t.Fatal(err)
	}
	for name, creds := range map[string]map[string]string{
		"garage-a": {"access_key_id": os.Getenv("VEHO_TEST_S3_A_ACCESS_KEY"), "secret_access_key": os.Getenv("VEHO_TEST_S3_A_SECRET_KEY")},
		"nexus":    {"username": os.Getenv("VEHO_TEST_NEXUS_USER"), "password": os.Getenv("VEHO_TEST_NEXUS_PASSWORD")},
	} {
		b, _ := json.Marshal(creds)
		if _, err := f.secrets.Put(ctx, f.tenant, name, b); err != nil {
			t.Fatal(err)
		}
	}
	srcDir := f.fileEndpoint(t, "source", "file", "lab-a")
	f.endpoint(t, "s3a", "s3", "lab-b", map[string]any{"endpoint": os.Getenv("VEHO_TEST_S3_A_ENDPOINT"),
		"region": os.Getenv("VEHO_TEST_S3_A_REGION"), "bucket": "dest-1", "path_style": true}, "db://garage-a")
	f.endpoint(t, "nx", "nexus", "lab-a", map[string]any{"url": url, "repository": "veho-raw", "format": "raw", "ca_cert": string(ca)}, "db://nexus")
	f.launch(t)

	policy := func(dest, path string) string {
		return `{"destinations": [{"endpoint": "` + dest + `", "path": "` + path + `"}], "completion": {"min_copies": 1},
			"retry": {"initial": "100ms", "max_interval": "400ms"}}`
	}
	first := f.submit(t, srcDir, 12*mib+123, policy("s3a", "nx-t5/OBJ"))
	name := filepath.Base(first.src)
	first.run(t, "s3a")
	want := first.state(t).Digest
	for _, hop := range []struct{ from, path, to, dest string }{
		{"s3a", "nx-t5/" + name, "nx", "nx-t5/" + name},
		{"nx", "nx-t5/" + name, "s3a", "nx-t5/back/" + name},
	} {
		j := f.submitFrom(t, hop.from, hop.path, policy(hop.to, hop.dest))
		j.run(t, hop.to)
		if s := j.state(t); s.State != "SUCCEEDED" || s.Copies != 1 || s.Digest != want || len(want) != 64 {
			t.Fatalf("%s to %s: %+v, want digest %s", hop.from, hop.to, s, want)
		}
	}
}
