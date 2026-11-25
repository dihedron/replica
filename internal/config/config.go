// Package config loads and checks veho.yaml, the configuration of veho serve
// (.specs/veho-engine.md §2). Adapted from ligo 9f11da9 (internal/config):
// the same strict decoding, defaults and fail-fast validation.
package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/dihedron/veho/internal/secrets"
	"go.yaml.in/yaml/v3"
)

// Config is veho.yaml.
type Config struct {
	Node          Node          `yaml:"node"`
	Database      Database      `yaml:"database"`
	Secrets       Secrets       `yaml:"secrets"`
	Audit         Audit         `yaml:"audit"`
	Engine        Engine        `yaml:"engine"`
	Housekeeping  Housekeeping  `yaml:"housekeeping"`
	Drivers       []Driver      `yaml:"drivers"`
	Metrics       Metrics       `yaml:"metrics"`
	API           API           `yaml:"api"`
	Checks        Checks        `yaml:"checks"`
	Notifications Notifications `yaml:"notifications"`
	NetworkChecks NetworkChecks `yaml:"network_checks"`
}

// NetworkChecks are the periodic checks of the endpoints' network flows
// (veho-network-checks.md §11): every Interval (0: none), at most PerPass
// endpoints per housekeeping pass.
type NetworkChecks struct {
	Interval *time.Duration `yaml:"interval"`
	PerPass  int            `yaml:"per_pass"`
}

// Notifications are the webhooks (veho-webhooks.md): off unless enabled.
// AllowCIDRs open private ranges to deliveries (never the metadata
// addresses); the retentions are of the outbox, 0 purging at once.
type Notifications struct {
	Enabled            bool           `yaml:"enabled"`
	AllowCIDRs         []string       `yaml:"allow_cidrs"`
	DeliveredRetention *time.Duration `yaml:"delivered_retention"`
	AbandonedRetention *time.Duration `yaml:"abandoned_retention"`
}

// Checks are the severities of the start-up checks (veho-store.md §7):
// refuse (the node does not start) or warn (it logs and starts). The schema
// version always refuses.
type Checks struct {
	Primary           string        `yaml:"primary"`
	SynchronousCommit string        `yaml:"synchronous_commit"`
	Quorum            string        `yaml:"quorum"`
	Clock             string        `yaml:"clock"`
	MaxClockOffset    time.Duration `yaml:"max_clock_offset"`
}

// API is the API's listener (veho-api-server.md §2, §7).
type API struct {
	Listen     string    `yaml:"listen"`
	TLS        TLS       `yaml:"tls"`
	MaxWatches int       `yaml:"max_watches"`
	OIDC       []OIDC    `yaml:"oidc"`
	SPIFFE     SPIFFE    `yaml:"spiffe"`
	RateLimit  RateLimit `yaml:"rate_limit"`
}

// RateLimit is the API's rate limits (§5.2).
type RateLimit struct {
	TenantPerSecond    float64 `yaml:"tenant_per_second"`
	PrincipalPerSecond float64 `yaml:"principal_per_second"`
	// Burst is a multiple of the rate; default 2.
	Burst             float64 `yaml:"burst"`
	FailuresPerMinute float64 `yaml:"failures_per_minute"`
}

// OIDC is an issuer whose bearer tokens the API accepts (§4.4).
type OIDC struct {
	Issuer      string `yaml:"issuer"`
	Audience    string `yaml:"audience"`
	GroupsClaim string `yaml:"groups_claim"`
	// CAFile verifies the issuer's TLS; empty, the system's roots.
	CAFile string `yaml:"ca_file"`
}

// SPIFFE is the trust domain whose SVIDs the API accepts (§4.4); an empty
// trust domain turns SPIFFE off.
type SPIFFE struct {
	TrustDomain    string `yaml:"trust_domain"`
	X509BundleFile string `yaml:"x509_bundle_file"`
	JWTBundleFile  string `yaml:"jwt_bundle_file"`
	Audience       string `yaml:"audience"`
}

// TLS names the certificate and key files of a listener.
type TLS struct {
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

// Node is this node: its executor ID and its location.
type Node struct {
	ID     string `yaml:"id"`
	Region string `yaml:"region"`
}

// Database names the environment variables of the three DSNs; a DSN never
// appears in the file.
type Database struct {
	API    DSN `yaml:"api"`
	Worker DSN `yaml:"worker"`
	DBOS   DSN `yaml:"dbos"`
}

// DSN is the name of the environment variable that holds a DSN.
type DSN struct {
	Env string `yaml:"dsn_env"`
}

// Secrets locates the unwrap key (veho-secrets.md §3).
type Secrets struct {
	UnwrapKey string `yaml:"unwrap_key"`
}

// Audit chooses the audit sink (veho-audit.md §3).
type Audit struct {
	Sink string `yaml:"sink"`
}

// Engine tunes the workflows.
type Engine struct {
	Heartbeat     time.Duration `yaml:"heartbeat"`
	LeaseTTL      time.Duration `yaml:"lease_ttl"`
	PartSizeMiB   int           `yaml:"part_size_mib"`
	PartsInFlight int           `yaml:"parts_in_flight"`
	// PartMemoryMiB bounds the parts this node holds at once, across all its
	// transfers (NFR-5): a part waits for room before it is read.
	PartMemoryMiB     int `yaml:"part_memory_mib"`
	WorkerConcurrency int `yaml:"worker_concurrency"`
	TenantConcurrency int `yaml:"tenant_concurrency"`
}

// Housekeeping tunes the loop and the retention (veho-audit-housekeeping.md
// §3); a retention of 0 keeps everything, an unset one is the default.
type Housekeeping struct {
	Interval          time.Duration  `yaml:"interval"`
	WorkflowRetention *time.Duration `yaml:"workflow_retention"`
	JobsRetention     *time.Duration `yaml:"jobs_retention"`
}

// validDriverName is a driver's name, also the suffix of its plugin binary.
var validDriverName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// Driver enables a storage driver (veho-plugin-distribution.md §2): a
// built-in one, a plugin installed from its package (no Path), or a plugin
// pinned by Path and SHA256. A driver not listed is not available. Key
// names the key file whose keys sign an installed plugin's manifest;
// empty is the release's.
type Driver struct {
	Name     string         `yaml:"name"`
	Key      string         `yaml:"key"`
	Path     string         `yaml:"path"`
	SHA256   string         `yaml:"sha256"`
	Settings map[string]any `yaml:"settings"`
}

// Metrics is the Prometheus listener; empty turns it off.
type Metrics struct {
	Listen string `yaml:"listen"`
}

// Load reads, completes and checks the file at path, and returns it with the
// SHA-256 of its bytes, which the node logs at start (T-24).
func Load(path string) (*Config, string, error) {
	data, err := os.ReadFile(path) //nolint:gosec // the path is the operator's
	if err != nil {
		return nil, "", fmt.Errorf("config: %w", err)
	}
	c, err := Parse(data)
	if err != nil {
		return nil, "", fmt.Errorf("config: %s: %w", path, err)
	}
	sum := sha256.Sum256(data)
	return c, hex.EncodeToString(sum[:]), nil
}

// Parse decodes data, refusing unknown keys, fills the defaults and checks
// the result.
func Parse(data []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, err
	}
	if err := c.complete(); err != nil {
		return nil, err
	}
	return &c, c.validate()
}

func (c *Config) complete() error {
	if c.Node.ID == "" {
		host, err := os.Hostname()
		if err != nil {
			return fmt.Errorf("node.id: %w", err)
		}
		c.Node.ID = host
	}
	set := func(s *string, v string) {
		if *s == "" {
			*s = v
		}
	}
	setD := func(d *time.Duration, v time.Duration) {
		if *d == 0 {
			*d = v
		}
	}
	setI := func(i *int, v int) {
		if *i == 0 {
			*i = v
		}
	}
	set(&c.Database.API.Env, "VEHO_API_DATABASE_URL")
	set(&c.Database.Worker.Env, "VEHO_WORKER_DATABASE_URL")
	set(&c.Database.DBOS.Env, "VEHO_DBOS_DATABASE_URL")
	set(&c.Audit.Sink, "syslog")
	set(&c.Metrics.Listen, "127.0.0.1:9464")
	for _, s := range []*string{&c.Checks.Primary, &c.Checks.SynchronousCommit, &c.Checks.Quorum, &c.Checks.Clock} {
		set(s, "refuse")
	}
	set(&c.API.Listen, ":8443")
	setI(&c.API.MaxWatches, 1000)
	setF := func(f *float64, v float64) {
		if *f == 0 {
			*f = v
		}
	}
	setF(&c.API.RateLimit.TenantPerSecond, 50)
	setF(&c.API.RateLimit.PrincipalPerSecond, 20)
	setF(&c.API.RateLimit.Burst, 2)
	setF(&c.API.RateLimit.FailuresPerMinute, 30)
	setD(&c.Engine.Heartbeat, 10*time.Second)
	setD(&c.Engine.LeaseTTL, 60*time.Second)
	setI(&c.Engine.PartSizeMiB, 64)
	setI(&c.Engine.PartsInFlight, 4)
	setI(&c.Engine.PartMemoryMiB, 1024)
	setD(&c.Checks.MaxClockOffset, time.Second)
	setI(&c.Engine.WorkerConcurrency, 16)
	setI(&c.Engine.TenantConcurrency, 4)
	setD(&c.Housekeeping.Interval, 30*time.Second)
	setP := func(d **time.Duration, v time.Duration) {
		if *d == nil {
			*d = &v
		}
	}
	setP(&c.Housekeeping.WorkflowRetention, 168*time.Hour)
	setP(&c.NetworkChecks.Interval, 24*time.Hour)
	setI(&c.NetworkChecks.PerPass, 20)
	setP(&c.Housekeeping.JobsRetention, 90*24*time.Hour)
	setP(&c.Notifications.DeliveredRetention, 24*time.Hour)
	setP(&c.Notifications.AbandonedRetention, 720*time.Hour)
	return nil
}

// minRetention is the smallest retention accepted (HK-3), so that a typo
// cannot empty a table.
const minRetention = 24 * time.Hour

func (c *Config) validate() error {
	var errs []error
	check := func(ok bool, format string, args ...any) {
		if !ok {
			errs = append(errs, fmt.Errorf(format, args...))
		}
	}
	check(c.Node.Region != "", "node.region is required")
	if _, err := secrets.ParseSource(c.Secrets.UnwrapKey); err != nil {
		errs = append(errs, fmt.Errorf("secrets.unwrap_key: %w", err))
	}
	check(c.Audit.Sink == "syslog" || c.Audit.Sink == "stderr", "audit.sink %q: want syslog or stderr", c.Audit.Sink)
	e := c.Engine
	check(e.Heartbeat > 0, "engine.heartbeat must be positive")
	check(e.LeaseTTL >= 3*e.Heartbeat, "engine.lease_ttl (%v) must be at least three heartbeats (%v)", e.LeaseTTL, 3*e.Heartbeat)
	check(e.PartSizeMiB >= 5 && e.PartSizeMiB <= 512, "engine.part_size_mib %d: want 5 to 512", e.PartSizeMiB)
	check(e.PartsInFlight >= 1 && e.PartsInFlight <= 64, "engine.parts_in_flight %d: want 1 to 64", e.PartsInFlight)
	check(e.PartMemoryMiB >= e.PartSizeMiB, "engine.part_memory_mib %d: want at least engine.part_size_mib (%d)", e.PartMemoryMiB, e.PartSizeMiB)
	check(e.WorkerConcurrency >= 1, "engine.worker_concurrency must be positive")
	check(e.TenantConcurrency >= 1 && e.TenantConcurrency <= e.WorkerConcurrency, "engine.tenant_concurrency %d: want 1 to worker_concurrency", e.TenantConcurrency)
	for name, sev := range map[string]string{"primary": c.Checks.Primary, "synchronous_commit": c.Checks.SynchronousCommit,
		"quorum": c.Checks.Quorum, "clock": c.Checks.Clock} {
		check(sev == "refuse" || sev == "warn", "checks.%s %q: want refuse or warn", name, sev)
	}
	check(c.Checks.MaxClockOffset > 0, "checks.max_clock_offset must be positive")
	h := c.Housekeeping
	check(h.Interval > 0, "housekeeping.interval must be positive")
	for name, d := range map[string]time.Duration{"workflow_retention": *h.WorkflowRetention, "jobs_retention": *h.JobsRetention} {
		check(d == 0 || d >= minRetention, "housekeeping.%s %v: want 0 (keep) or at least %v", name, d, minRetention)
	}
	nc := c.NetworkChecks
	check(*nc.Interval == 0 || *nc.Interval >= time.Hour, "network_checks.interval %v: want 0 (off) or at least 1h", *nc.Interval)
	check(nc.PerPass >= 1 && nc.PerPass <= 1000, "network_checks.per_pass %d: want 1 to 1000", nc.PerPass)
	for _, cidr := range c.Notifications.AllowCIDRs {
		_, err := netip.ParsePrefix(cidr)
		check(err == nil, "notifications.allow_cidrs %q: want a CIDR such as 10.20.0.0/16", cidr)
	}
	n := c.Notifications
	check(*n.DeliveredRetention == 0 || *n.DeliveredRetention >= time.Minute, "notifications.delivered_retention %v: want 0 or at least 1m", *n.DeliveredRetention)
	check(*n.AbandonedRetention >= time.Hour, "notifications.abandoned_retention %v: want at least 1h", *n.AbandonedRetention)
	names := map[string]bool{}
	for i, d := range c.Drivers {
		check(validDriverName.MatchString(d.Name) && !names[d.Name], "drivers[%d]: a name (%s) is required, once", i, validDriverName)
		check((d.Path == "") == (d.SHA256 == ""), "drivers[%d]: path and sha256 go together", i)
		check(d.Key == "" || validDriverName.MatchString(d.Key), "drivers[%d]: key must match %s", i, validDriverName)
		check(d.Key == "" || d.Path == "", "drivers[%d]: key is for an installed plugin, not one pinned by path", i)
		if d.SHA256 != "" {
			sum, err := hex.DecodeString(d.SHA256)
			check(err == nil && len(sum) == sha256.Size, "drivers[%d]: sha256 must be 64 hexadecimal digits", i)
		}
		names[d.Name] = true
	}
	_, _, err := net.SplitHostPort(c.API.Listen)
	check(err == nil, "api.listen %q: want host:port", c.API.Listen)
	check(c.API.TLS.CertFile != "" && c.API.TLS.KeyFile != "", "api.tls.cert_file and api.tls.key_file are required")
	check(c.API.MaxWatches > 0, "api.max_watches must be positive")
	rl := c.API.RateLimit
	check(rl.TenantPerSecond > 0 && rl.PrincipalPerSecond > 0 && rl.Burst >= 1 && rl.FailuresPerMinute > 0, "api.rate_limit: the rates must be positive and the burst at least 1")
	issuers := map[string]bool{}
	for i := range c.API.OIDC {
		o := &c.API.OIDC[i]
		if o.GroupsClaim == "" {
			o.GroupsClaim = "groups"
		}
		check(strings.HasPrefix(o.Issuer, "https://") && !issuers[o.Issuer], "api.oidc[%d].issuer: an https URL is required, once", i)
		check(o.Audience != "", "api.oidc[%d].audience is required", i)
		issuers[o.Issuer] = true
	}
	if sp := c.API.SPIFFE; sp.TrustDomain != "" {
		check(sp.X509BundleFile != "" || sp.JWTBundleFile != "", "api.spiffe: x509_bundle_file or jwt_bundle_file is required")
		check(sp.JWTBundleFile == "" || sp.Audience != "", "api.spiffe.audience is required with jwt_bundle_file")
	}
	if c.Metrics.Listen != "off" {
		_, _, err := net.SplitHostPort(c.Metrics.Listen)
		check(err == nil, "metrics.listen %q: want host:port or off", c.Metrics.Listen)
	}
	return errors.Join(errs...)
}
