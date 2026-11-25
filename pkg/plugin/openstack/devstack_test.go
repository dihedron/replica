//go:build devstack

package openstack

import (
	"context"
	"testing"

	"github.com/dihedron/veho/pkg/plugin"
)

func devstack(t *testing.T) (*Cloud, Endpoint, map[string]plugin.Credentials) {
	t.Helper()
	s, ep, creds := DevStack(t)
	c, err := NewCloud(s)
	if err != nil {
		t.Fatal(err)
	}
	return c, ep, creds
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
