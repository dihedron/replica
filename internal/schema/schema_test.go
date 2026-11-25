package schema

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/dihedron/veho/pkg/plugin"
	"github.com/dihedron/veho/pkg/plugin/file"
	"github.com/dihedron/veho/pkg/plugin/ftp"
	"github.com/dihedron/veho/pkg/plugin/glance"
	"github.com/dihedron/veho/pkg/plugin/http"
	"github.com/dihedron/veho/pkg/plugin/nexus"
	"github.com/dihedron/veho/pkg/plugin/s3"
	"github.com/dihedron/veho/pkg/plugin/sftp"
	"github.com/dihedron/veho/pkg/plugin/swift"
)

func driver(t *testing.T, factory func(json.RawMessage) (plugin.Driver, error)) plugin.Driver {
	t.Helper()
	d, err := factory(nil)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// CV-T1: every shipped driver's schema compiles; a valid configuration
// passes; a wrong type, an unknown key and a bad enum are refused with the
// path of the value.
func TestShippedSchemas(t *testing.T) {
	os := `"auth_url": "https://keystone.example/v3", "region": "RegionOne"`
	for name, tc := range map[string]struct {
		d                 plugin.Driver
		valid             string
		wrong, enum, path string // a configuration with a wrong type, one with a bad enum, the path of the wrong type
	}{
		"file":   {file.New(), `{"root": "/srv"}`, `{"root": 3}`, ``, "/root"},
		"http":   {driver(t, http.Factory), `{"base_url": "https://files.example/"}`, `{"base_url": "https://f/", "headers": 3}`, ``, "/headers"},
		"sftp":   {driver(t, sftp.Factory), `{"host": "h", "root": "/srv", "host_key": "ssh-ed25519 AAAA"}`, `{"host": "h", "root": "/", "host_key": "k", "port": "22"}`, ``, "/port"},
		"ftp":    {driver(t, ftp.Factory), `{"host": "h", "root": "/pub"}`, `{"host": "h", "root": "/", "port": "21"}`, `{"host": "h", "root": "/", "tls": "maybe"}`, "/port"},
		"s3":     {s3.New(), `{"region": "r", "bucket": "bucket-1"}`, `{"region": "r", "bucket": 3}`, ``, "/bucket"},
		"glance": {driver(t, glance.Factory), `{` + os + `, "upload": "put"}`, `{` + os + `, "stores": "cheap"}`, `{` + os + `, "upload": "ftp"}`, "/stores"},
		"swift":  {driver(t, swift.Factory), `{` + os + `, "container": "c"}`, `{` + os + `, "container": 1}`, `{` + os + `, "container": "c", "interface": "outer"}`, "/container"},
		"nexus":  {driver(t, nexus.Factory), `{"url": "https://n.example", "repository": "r"}`, `{"url": "https://n", "repository": "r", "npm_dist_tag": 1}`, `{"url": "https://n", "repository": "r", "format": "docker"}`, "/npm_dist_tag"},
	} {
		s, err := Compile(name, tc.d.ConfigSchema())
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if err := s.Validate([]byte(tc.valid)); err != nil {
			t.Errorf("%s: valid refused: %v", name, err)
		}
		if err := s.Validate([]byte(tc.wrong)); err == nil || !strings.Contains(err.Error(), tc.path) {
			t.Errorf("%s: wrong type: %v, want the path %s", name, err, tc.path)
		}
		unknown := strings.TrimSuffix(strings.TrimSpace(tc.valid), "}") + `, "nonsense": 1}`
		if err := s.Validate([]byte(unknown)); err == nil || !strings.Contains(err.Error(), "nonsense") {
			t.Errorf("%s: unknown key: %v", name, err)
		}
		if tc.enum != "" {
			if err := s.Validate([]byte(tc.enum)); err == nil {
				t.Errorf("%s: bad enum accepted", name)
			}
		}
		if err := s.Validate([]byte(`[1]`)); err == nil {
			t.Errorf("%s: not an object", name)
		}
	}
}

// CV-T5: a schema that names a remote reference does not compile: a schema
// must not make the node fetch (CV-1); nor does a schema that is not JSON.
func TestRemoteRef(t *testing.T) {
	for name, s := range map[string]string{
		"remote ref":  `{"$schema": "https://json-schema.org/draft/2020-12/schema", "$ref": "https://evil.example/schema.json"}`,
		"remote prop": `{"type": "object", "properties": {"x": {"$ref": "http://169.254.169.254/latest"}}}`,
		"not JSON":    `{"type": `,
	} {
		if _, err := Compile("acme", []byte(s)); err == nil {
			t.Errorf("%s: compiled", name)
		}
	}
	if _, err := Compile("acme", []byte(`{"$defs": {"a": {"type": "string"}}, "properties": {"x": {"$ref": "#/$defs/a"}}}`)); err != nil {
		t.Errorf("a local reference: %v", err)
	}
	if _, err := Compile("acme", nil); err != nil {
		t.Errorf("no schema (a driver without one): %v", err)
	}
}

// CV-T2: each shipped driver's CheckConfig refuses what its Open refused
// before the credentials, one case per rule, and passes a valid
// configuration; none needs credentials or the network.
func TestCheckConfig(t *testing.T) {
	os := `"auth_url": "https://keystone.example/v3", "region": "RegionOne"`
	for name, tc := range map[string]struct {
		d     plugin.Driver
		valid string
		bad   []string
	}{
		"s3":   {s3.New(), `{"region": "r", "bucket": "bucket-1"}`, []string{`{"region": "r", "bucket": "b1", "storage_class": "GLACIER"}`, `{"bucket": "b1"}`}},
		"http": {driver(t, http.Factory), `{"base_url": "https://f.example/pub/"}`, []string{`{"base_url": "ftp://f.example/"}`, `{"base_url": "https://u:p@f.example/"}`, `{"base_url": "https://f/", "headers": {"Authorization": "x"}}`}},
		"sftp": {driver(t, sftp.Factory), `{"host": "h", "root": "/srv", "host_key": "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"}`,
			[]string{`{"host": "h", "root": "srv", "host_key": "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"}`, `{"host": "h", "root": "/srv", "host_key": "nonsense"}`, `{"host": "a/b", "root": "/", "host_key": "x"}`}},
		"ftp":    {driver(t, ftp.Factory), `{"host": "h", "root": "/pub"}`, []string{`{"host": "h", "root": "/pub", "tls": "none"}`, `{"host": "h", "root": "/a/../b"}`, `{"host": "h", "root": "/", "port": 70000}`}},
		"glance": {driver(t, glance.Factory), `{` + os + `, "upload": "glance-direct", "stores": ["cheap"]}`, []string{`{` + os + `, "stores": ["cheap"]}`, `{` + os + `, "upload": "web-download", "web_download_url_ttl": "10s"}`, `{` + os + `, "properties": {"os_hidden": "true"}}`, `{"auth_url": "http://keystone.example/v3"}`}},
		"swift":  {driver(t, swift.Factory), `{` + os + `, "container": "images"}`, []string{`{` + os + `, "container": "a/b"}`, `{"auth_url": "https://keystone.example/v2.0", "container": "c"}`, `{` + os + `, "container": "c", "ca_cert": "not PEM"}`}},
		"nexus":  {driver(t, nexus.Factory), `{"url": "https://n.example", "repository": "r", "format": "npm", "npm_dist_tag": "next"}`, []string{`{"url": "https://n", "repository": "r", "npm_dist_tag": "next"}`, `{"url": "http://n", "repository": "r"}`, `{"url": "https://n", "repository": "a/b"}`, `{"url": "https://n", "repository": "r", "ca_cert": "not PEM"}`}},
		"file":   {file.New(), ``, []string{`{"root": "relative"}`}},
	} {
		cc, ok := tc.d.(plugin.ConfigChecker)
		if !ok {
			t.Errorf("%s: no CheckConfig", name)
			continue
		}
		if tc.valid != "" {
			if err := cc.CheckConfig(context.Background(), plugin.Config(tc.valid)); err != nil {
				t.Errorf("%s: valid refused: %v", name, err)
			}
		}
		for _, bad := range tc.bad {
			if err := cc.CheckConfig(context.Background(), plugin.Config(bad)); err == nil || plugin.ClassOf(err) != plugin.Permanent {
				t.Errorf("%s: %s: %v", name, bad, err)
			}
		}
	}
}
