//go:build integration

package engine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/dihedron/veho/internal/store"
	"github.com/jackc/pgx/v5"
)

// CV-T4: endpoint create and update refuse a wrong configuration of a
// plugin driver, with the path of the value or the driver's reason, and
// store nothing.
func TestEndpointConfigValidated(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	os := `"auth_url": "https://keystone.example/v3", "region": "RegionOne"`
	for name, tc := range map[string]struct{ driver, cfg, want string }{
		"s3 bucket a number":     {"s3", `{"region": "r", "bucket": 3}`, "/bucket"},
		"s3 unknown key":         {"s3", `{"region": "r", "bucket": "bucket-1", "nonsense": 1}`, "nonsense"},
		"s3 archive class":       {"s3", `{"region": "r", "bucket": "bucket-1", "storage_class": "GLACIER"}`, "archive"},
		"glance upload ftp":      {"glance", `{` + os + `, "upload": "ftp"}`, "/upload"},
		"glance stores with put": {"glance", `{` + os + `, "stores": ["cheap"]}`, "glance-direct"},
		"nexus tag a number":     {"nexus", `{"url": "https://n.example", "repository": "r", "format": "npm", "npm_dist_tag": 1}`, "/npm_dist_tag"},
		"nexus tag without npm":  {"nexus", `{"url": "https://n.example", "repository": "r", "npm_dist_tag": "next"}`, "only for format npm"},
	} {
		_, err := f.e.CreateEndpoint(ctx, f.tenant, Endpoint{Name: "cv", Driver: tc.driver, Location: "lab-a", Config: json.RawMessage(tc.cfg)})
		if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want invalid and %q", name, err, tc.want)
		}
	}
	if _, err := f.e.GetEndpoint(ctx, f.tenant, "cv"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stored: %v", err)
	}
	if _, err := f.e.CreateEndpoint(ctx, f.tenant, Endpoint{Name: "cv", Driver: "s3", Location: "lab-a",
		Config: json.RawMessage(`{"region": "r", "bucket": "bucket-1"}`)}); err != nil {
		t.Fatal(err)
	}
	_, err := f.e.UpdateEndpoint(ctx, f.tenant, Endpoint{Name: "cv", Location: "lab-a", Config: json.RawMessage(`{"region": "r", "bucket": 3}`)})
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "/bucket") {
		t.Fatalf("update: %v", err)
	}
}

// CV-T6: endpoint list --invalid lists only the endpoints the drivers now
// refuse, each with its reason, and those of a driver this node does not
// offer.
func TestInvalidEndpoints(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	if _, err := f.e.CreateEndpoint(ctx, f.tenant, Endpoint{Name: "good", Driver: "s3", Location: "lab-a",
		Config: json.RawMessage(`{"region": "r", "bucket": "bucket-1"}`)}); err != nil {
		t.Fatal(err)
	}
	// stored behind the checks' back, as by an older veho
	if err := store.InTenant(ctx, f.api, f.tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `insert into veho.endpoints (tenant_id, name, location_id, driver, config) values
			(veho.current_tenant(), 'stale', 'lab-a', 's3', '{"region": "r", "bucket": "bucket-1", "storage_class": "DEEP_ARCHIVE"}')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	bad, err := f.e.InvalidEndpoints(ctx, f.tenant)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, b := range bad {
		got[b.Name] = b.Problem
	}
	if _, ok := got["good"]; ok || !strings.Contains(got["stale"], "archive") || !strings.Contains(got["gone"], "no driver") {
		t.Fatalf("%+v", got)
	}
}
