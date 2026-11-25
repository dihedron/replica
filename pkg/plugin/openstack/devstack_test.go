//go:build devstack

package openstack

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/dihedron/veho/pkg/plugin"
)

// devstack is the lab's cloud (test/devstack/lab.sh env), or a skip.
func devstack(t *testing.T) (*Cloud, Endpoint, map[string]plugin.Credentials) {
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
	c, err := NewCloud(Settings{AllowCIDRs: []string{os.Getenv("VEHO_DEVSTACK_ALLOW_CIDR")}})
	if err != nil {
		t.Fatal(err)
	}
	ep := Endpoint{AuthURL: auth, Region: os.Getenv("VEHO_DEVSTACK_REGION"), CACert: read("VEHO_DEVSTACK_CA_FILE")}
	return c, ep, map[string]plugin.Credentials{
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

// OS-T1 on DevStack: both authentications reach Glance and Swift through the
// TLS proxy; a wrong secret is Permanent.
func TestDevStackConnect(t *testing.T) {
	c, ep, creds := devstack(t)
	ctx := context.Background()
	for name, cr := range creds {
		for _, svc := range []Service{Image, ObjectStore} {
			t.Run(name+"/"+svc.name, func(t *testing.T) {
				sc, err := c.Connect(ctx, ep, cr, svc)
				if err != nil {
					t.Fatal(err)
				}
				path := "images"
				if svc.name == ObjectStore.name {
					path = ""
				}
				resp, err := sc.Get(ctx, sc.ServiceURL(path), nil, nil)
				if err != nil {
					t.Fatal(Classify(err, svc.name))
				}
				_ = resp.Body.Close()
			})
		}
	}
	bad := plugin.Credentials{"application_credential_id": creds["application_credential"]["application_credential_id"], "application_credential_secret": "wrong"}
	if _, err := c.Connect(ctx, ep, bad, Image); plugin.ClassOf(err) != plugin.Permanent {
		t.Fatalf("a wrong secret: %v", err)
	}
}
