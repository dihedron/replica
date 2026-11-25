//go:build devstack

package glance

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/dihedron/veho/pkg/plugin"
	"github.com/dihedron/veho/pkg/plugin/conformance"
	"github.com/dihedron/veho/pkg/plugin/openstack"
)

// OS-T3 and OS-T4 on DevStack: the conformance suite with each upload
// method (glance-direct to a chosen store).
func TestDevStackConformance(t *testing.T) {
	s, ep, creds := openstack.DevStack(t)
	settings, _ := json.Marshal(s)
	d, err := Factory(settings)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []config{
		{Upload: "put"},
		{Upload: "glance-direct", Stores: []string{"cheap"}},
	} {
		t.Run(m.Upload, func(t *testing.T) {
			m.Endpoint, m.DiskFormat, m.ContainerFormat = ep, "raw", "bare"
			cfg, _ := json.Marshal(m)
			conformance.Run(t, conformance.Target{
				Driver: d, Config: cfg, Credentials: creds["application_credential"],
				Prefix: fmt.Sprintf("veho-conformance-%s-%d-", m.Upload, time.Now().UnixNano()), PartSize: 1 << 20,
				BadCredentials: plugin.Credentials{"application_credential_id": "nobody", "application_credential_secret": "wrong"},
			})
		})
	}
}
