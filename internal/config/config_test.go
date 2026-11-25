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
		*c.Housekeeping.JobsRetention != 2160*time.Hour || *c.Housekeeping.WorkflowRetention != 168*time.Hour ||
		c.Checks.Primary != "refuse" || c.Checks.SynchronousCommit != "refuse" || c.Checks.Quorum != "refuse" ||
		c.Checks.Clock != "refuse" || c.Checks.MaxClockOffset != time.Second {
		t.Errorf("defaults: %+v", c)
	}
}

// Webhooks are off by default (veho-webhooks.md §2).
func TestNotificationsDefaults(t *testing.T) {
	c, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	n := c.Notifications
	if n.Enabled || len(n.AllowCIDRs) != 0 || *n.DeliveredRetention != 24*time.Hour || *n.AbandonedRetention != 720*time.Hour {
		t.Errorf("defaults: %+v", n)
	}
	c, err = Parse([]byte(minimal + "notifications: {enabled: true, allow_cidrs: [10.20.0.0/16], delivered_retention: 0s}\n"))
	if err != nil || !c.Notifications.Enabled || *c.Notifications.DeliveredRetention != 0 {
		t.Errorf("enabled: %+v, %v", c.Notifications, err)
	}
}

// NC-T8: periodic network checks every 24 h, 20 per pass, by default;
// 0 turns them off.
func TestNetworkChecksDefaults(t *testing.T) {
	c, err := Parse([]byte(minimal))
	if err != nil || *c.NetworkChecks.Interval != 24*time.Hour || c.NetworkChecks.PerPass != 20 {
		t.Fatalf("defaults: %+v, %v", c.NetworkChecks, err)
	}
	c, err = Parse([]byte(minimal + "network_checks: {interval: 0s}\n"))
	if err != nil || *c.NetworkChecks.Interval != 0 {
		t.Fatalf("off: %+v, %v", c.NetworkChecks, err)
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
		"field nope":              node + key + "nope: 1\n",
		"node.region":             "node: {id: x}\n" + key,
		"unwrap_key":              node + "secrets: {unwrap_key: vault:x}\n",
		"audit.sink":              node + key + "audit: {sink: kafka}\n",
		"lease_ttl":               node + key + "engine: {heartbeat: 30s, lease_ttl: 60s}\n",
		"part_size_mib":           node + key + "engine: {part_size_mib: 1024}\n",
		"tenant_concurrency":      node + key + "engine: {worker_concurrency: 2, tenant_concurrency: 3}\n",
		"jobs_retention":          node + key + "housekeeping: {jobs_retention: 1h}\n",
		"sha256 must be":          node + key + "drivers: [{name: s3, path: /usr/lib/veho/s3, sha256: abc}]\n",
		"a name":                  node + key + "drivers: [{name: s3, path: /a, sha256: " + zeros + "}, {name: s3}]\n",
		"drivers[0]: a name":      node + key + "drivers: [{name: ../s3}]\n",
		"go together":             node + key + "drivers: [{name: acme, path: /opt/acme}]\n",
		"key must match":          node + key + "drivers: [{name: acme, key: ../acme}]\n",
		"not one pinned":          node + key + "drivers: [{name: acme, key: acme, path: /a, sha256: " + zeros + "}]\n",
		"metrics.listen":          node + key + "metrics: {listen: nowhere}\n",
		"api.tls":                 node + "secrets: {unwrap_key: \"file:/k\"}\n",
		"checks.primary":          node + key + "checks: {primary: ignore}\n",
		"checks.clock":            node + key + "checks: {clock: maybe}\n",
		"max_clock_offset":        node + key + "checks: {max_clock_offset: -1s}\n",
		"field schema":            node + key + "checks: {schema: warn}\n",
		"allow_cidrs":             node + key + "notifications: {enabled: true, allow_cidrs: [nonsense]}\n",
		"delivered_retention":     node + key + "notifications: {delivered_retention: 1s}\n",
		"abandoned_retention":     node + key + "notifications: {abandoned_retention: 1s}\n",
		"network_checks.interval": node + key + "network_checks: {interval: 30m}\n",
		"network_checks.per_pass": node + key + "network_checks: {per_pass: 5000}\n",
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
