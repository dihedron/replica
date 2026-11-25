//go:build devstack

package openstack

import (
	"os"
	"strings"
	"testing"

	"github.com/dihedron/veho/pkg/plugin"
)

// DevStack is the lab's cloud (test/devstack/lab.sh env) for the drivers'
// tests, or a skip: its settings, endpoint and both kinds of credentials.
func DevStack(t *testing.T) (Settings, Endpoint, map[string]plugin.Credentials) {
	t.Helper()
	auth := os.Getenv("VEHO_DEVSTACK_AUTH_URL")
	if auth == "" {
		t.Skip("no DevStack: run through test/devstack/lab.sh test")
	}
	read := func(env string) string {
		b, err := os.ReadFile(os.Getenv(env))
		if err != nil {
			t.Fatalf("%s: %v", env, err)
		}
		return strings.TrimSpace(string(b))
	}
	ep := Endpoint{AuthURL: auth, Region: os.Getenv("VEHO_DEVSTACK_REGION"), CACert: read("VEHO_DEVSTACK_CA_FILE")}
	return Settings{AllowCIDRs: []string{os.Getenv("VEHO_DEVSTACK_ALLOW_CIDR")}}, ep, map[string]plugin.Credentials{
		"application_credential": {
			"application_credential_id":     os.Getenv("VEHO_DEVSTACK_APP_CRED_ID"),
			"application_credential_secret": read("VEHO_DEVSTACK_APP_CRED_SECRET_FILE"),
		},
		"password": {
			"username":     os.Getenv("VEHO_DEVSTACK_USERNAME"),
			"password":     read("VEHO_DEVSTACK_PASSWORD_FILE"),
			"project_name": os.Getenv("VEHO_DEVSTACK_PROJECT"),
		},
	}
}
