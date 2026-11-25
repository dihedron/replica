package nexus

import (
	"context"
	"net/url"
	"strconv"
	"testing"

	"github.com/dihedron/veho/pkg/plugin"
)

// NC-T1: Nexus's flow, with the endpoint's CA.
func TestFlows(t *testing.T) {
	f := newFake(t)
	got, err := open(t, f, "files", "", good).(plugin.FlowLister).Flows(context.Background())
	u, _ := url.Parse(f.srv.URL)
	port, _ := strconv.Atoi(u.Port())
	if err != nil || len(got) != 1 || got[0].Host != u.Hostname() || got[0].Port != port || !got[0].TLS || got[0].CACert == "" {
		t.Fatalf("%+v, %v", got, err)
	}
}
