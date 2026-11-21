package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const minimal = `
node: {id: veho-a1, region: region-a}
secrets: {unwrap_key: "file:/etc/veho/unwrap.key"}
api: {tls: {cert_file: /etc/veho/api.crt, key_file: /etc/veho/api.key}}
`

func TestDefaults(t *testing.T) {
	c, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	if c.Engine.Heartbeat != 10*time.Second || c.Engine.LeaseTTL != time.Minute || c.Engine.PartSizeMiB != 64 ||
		c.Database.DBOS.Env != "VEHO_DBOS_DATABASE_URL" || c.Audit.Sink != "syslog" ||
		*c.Housekeeping.JobsRetention != 2160*time.Hour || *c.Housekeeping.WorkflowRetention != 168*time.Hour {
		t.Errorf("defaults: %+v", c)
	}
}

func TestLoadHash(t *testing.T) {
	p := filepath.Join(t.TempDir(), "veho.yaml")
	if err := os.WriteFile(p, []byte(minimal), 0o600); err != nil {
		t.Fatal(err)
	}
	_, h1, err := Load(p)
	if err != nil || len(h1) != 64 {
		t.Fatalf("%q, %v", h1, err)
	}
	if err := os.WriteFile(p, []byte(minimal+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, h2, _ := Load(p); h2 == h1 {
		t.Error("the hash does not change with the file")
	}
}

func TestRefused(t *testing.T) {
	const node, key = "node: {id: a, region: b}\n", "secrets: {unwrap_key: \"file:/k\"}\napi: {tls: {cert_file: /c, key_file: /k}}\n"
	zeros := strings.Repeat("0", 64)
	for want, doc := range map[string]string{
		"field nope":         node + key + "nope: 1\n",
		"node.region":        "node: {id: x}\n" + key,
		"unwrap_key":         node + "secrets: {unwrap_key: vault:x}\n",
		"audit.sink":         node + key + "audit: {sink: kafka}\n",
		"lease_ttl":          node + key + "engine: {heartbeat: 30s, lease_ttl: 60s}\n",
		"part_size_mib":      node + key + "engine: {part_size_mib: 1024}\n",
		"tenant_concurrency": node + key + "engine: {worker_concurrency: 2, tenant_concurrency: 3}\n",
		"jobs_retention":     node + key + "housekeeping: {jobs_retention: 1h}\n",
		"sha256 must be":     node + key + "plugins: [{name: s3, path: /usr/lib/veho/s3, sha256: abc}]\n",
		"a name is required": node + key + "plugins: [{name: s3, path: /a, sha256: " + zeros + "}, {name: s3, path: /b, sha256: " + zeros + "}]\n",
		"metrics.listen":     node + key + "metrics: {listen: nowhere}\n",
		"api.tls":            node + "secrets: {unwrap_key: \"file:/k\"}\n",
	} {
		if _, err := Parse([]byte(doc)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", want, err)
		}
	}
	c, err := Parse([]byte(minimal + "metrics: {listen: \"off\"}\nhousekeeping: {jobs_retention: 0s}\n"))
	if err != nil || *c.Housekeeping.JobsRetention != 0 {
		t.Errorf("metrics off and jobs kept: %v", err)
	}
}
