package sftp

import (
	"context"
	"net"
	"reflect"
	"strconv"
	"testing"

	"github.com/dihedron/veho/pkg/plugin"
)

// NC-T1: the server's flow, with SSH's handshake.
func TestFlows(t *testing.T) {
	s := newServer(t)
	c, err := loopback(t).Open(context.Background(), s.config(t, ""), s.creds())
	if err != nil {
		t.Fatal(err)
	}
	host, port, _ := net.SplitHostPort(s.addr)
	p, _ := strconv.Atoi(port)
	got, err := c.(plugin.FlowLister).Flows(context.Background())
	if err != nil || !reflect.DeepEqual(got, []plugin.Flow{{Purpose: "sftp", Host: host, Port: p, Handshake: "ssh"}}) {
		t.Errorf("%+v, %v", got, err)
	}
}
