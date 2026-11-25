package glance

import (
	"context"
	"testing"

	"github.com/dihedron/veho/pkg/plugin"
)

// NC-T1: Keystone's flow and Glance's from the catalog.
func TestFlows(t *testing.T) {
	f := newFakeGlance(t, 9)
	got, err := open(t, f, "").(plugin.FlowLister).Flows(context.Background())
	if err != nil || len(got) != 2 || got[0].Purpose != "keystone" || got[1].Purpose != "glance" || !got[1].TLS || got[1].CACert == "" {
		t.Fatalf("%+v, %v", got, err)
	}
}
