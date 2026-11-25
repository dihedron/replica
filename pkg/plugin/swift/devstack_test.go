//go:build devstack

package swift

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/dihedron/veho/pkg/plugin"
	"github.com/dihedron/veho/pkg/plugin/conformance"
	"github.com/dihedron/veho/pkg/plugin/openstack"
)

// OS-T6 on DevStack: the conformance suite as source and sink, with each
// authentication.
func TestDevStackConformance(t *testing.T) {
	s, ep, creds := openstack.DevStack(t)
	settings, _ := json.Marshal(s)
	d, err := Factory(settings)
	if err != nil {
		t.Fatal(err)
	}
	for name, cr := range creds {
		t.Run(name, func(t *testing.T) {
			cfg, _ := json.Marshal(config{Endpoint: ep, Container: "veho-conformance"})
			conformance.Run(t, conformance.Target{
				Driver: d, Config: cfg, Credentials: cr, Container: "veho-conformance",
				Prefix: fmt.Sprintf("%s-%d/", name, time.Now().UnixNano()), PartSize: 1 << 20,
				BadCredentials: plugin.Credentials{"application_credential_id": "nobody", "application_credential_secret": "wrong"},
			})
		})
	}
}
