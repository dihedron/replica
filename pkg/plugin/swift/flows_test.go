package swift

import (
	"context"
	"net/url"
	"strconv"
	"testing"

	"github.com/dihedron/veho/pkg/plugin"
)

// NC-T1: Keystone's flow and Swift's from the catalog, with the CA.
func TestFlows(t *testing.T) {
	f := newFakeSwift(t)
	got, err := open(t, f, "").(plugin.FlowLister).Flows(context.Background())
	u, _ := url.Parse(f.srv.URL)
	port, _ := strconv.Atoi(u.Port())
	if err != nil || len(got) != 2 || got[0].Purpose != "keystone" || got[1].Purpose != "swift" {
		t.Fatalf("%+v, %v", got, err)
	}
	for _, fl := range got {
		if fl.Host != u.Hostname() || fl.Port != port || !fl.TLS || fl.CACert != f.ca() {
			t.Errorf("%+v", fl)
		}
	}
}
