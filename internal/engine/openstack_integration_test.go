//go:build integration

package engine

import (
	"context"
	"encoding/json"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// OS-T7: end to end through the engine and the real drivers, with
// DevStack (eval "$(test/devstack/lab.sh env)" before lab.sh test): one job
// from a file to Glance with put, Glance with glance-direct to a store, and
// Swift; then a job from an http source on the lab host's address to Glance
// with web-download, which Glance fetches itself. The engine verifies every
// copy against the source's digest before the job succeeds.
func TestOpenStack(t *testing.T) {
	auth, host := os.Getenv("VEHO_DEVSTACK_AUTH_URL"), os.Getenv("VEHO_DEVSTACK_HOST_IP")
	if auth == "" || host == "" {
		t.Skip(`no DevStack: eval "$(test/devstack/lab.sh env)" first`)
	}
	read := func(env string) string {
		b, err := os.ReadFile(os.Getenv(env))
		if err != nil {
			t.Fatalf("%s: %v", env, err)
		}
		return strings.TrimSpace(string(b))
	}
	f := setup(t)
	ctx := context.Background()
	creds, _ := json.Marshal(map[string]string{
		"application_credential_id":     os.Getenv("VEHO_DEVSTACK_APP_CRED_ID"),
		"application_credential_secret": read("VEHO_DEVSTACK_APP_CRED_SECRET_FILE"),
	})
	if _, err := f.secrets.Put(ctx, f.tenant, "devstack", creds); err != nil {
		t.Fatal(err)
	}
	cloud := map[string]any{"auth_url": auth, "region": os.Getenv("VEHO_DEVSTACK_REGION"), "ca_cert": read("VEHO_DEVSTACK_CA_FILE")}
	image := merge(cloud, map[string]any{"disk_format": "raw", "container_format": "bare"})
	f.endpoint(t, "glance-put", "glance", "lab-b", merge(image, map[string]any{"upload": "put"}), "db://devstack")
	f.endpoint(t, "glance-direct", "glance", "lab-b", merge(image, map[string]any{"upload": "glance-direct", "stores": []string{"cheap"}}), "db://devstack")
	f.endpoint(t, "glance-web", "glance", "lab-b", merge(image, map[string]any{"upload": "web-download"}), "db://devstack")
	f.endpoint(t, "swift", "swift", "lab-b", merge(cloud, map[string]any{"container": "veho-os-t7"}), "db://devstack")
	srcDir := f.fileEndpoint(t, "source", "file", "lab-a")

	// the http source: srcDir on the address the DevStack VM reaches
	ln, err := net.Listen("tcp", host+":0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.FileServer(http.Dir(srcDir)), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	f.endpoint(t, "web", "http", "lab-a", map[string]any{"base_url": "http://" + ln.Addr().String() + "/"}, "")
	f.launch(t)

	dests := []string{"glance-put", "glance-direct", "swift"}
	var ds []string
	for _, d := range dests {
		ds = append(ds, `{"endpoint": "`+d+`", "path": "veho-os-t7-OBJ"}`)
	}
	j := f.submit(t, srcDir, 12*mib+123, `{"destinations": [`+strings.Join(ds, ", ")+`], "completion": {"min_copies": 3},
		"retry": {"initial": "100ms", "max_interval": "400ms"}}`)
	j.run(t, dests...)
	s := j.state(t)
	if s.State != "SUCCEEDED" || s.Copies != 3 || len(s.Digest) != 64 {
		t.Fatalf("file to Glance and Swift: %+v", s)
	}

	name := filepath.Base(j.src)
	w := f.submitFrom(t, "web", name, `{"destinations": [{"endpoint": "glance-web", "path": "veho-os-t7-web-`+name+`"}],
		"completion": {"min_copies": 1}, "retry": {"initial": "100ms", "max_interval": "400ms"}}`)
	w.run(t, "glance-web")
	if ws := w.state(t); ws.State != "SUCCEEDED" || ws.Copies != 1 || ws.Digest != s.Digest {
		t.Fatalf("web-download: %+v, want digest %s", ws, s.Digest)
	}
}

// merge is a new map with a's entries, then b's.
func merge(a, b map[string]any) map[string]any {
	m := maps.Clone(a)
	maps.Copy(m, b)
	return m
}
