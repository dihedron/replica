package ftp

import (
	"context"
	"reflect"
	"testing"

	"github.com/dihedron/veho/pkg/plugin"
)

// NC-T1: the control connection, TLS with implicit FTPS only, and the
// data connections, not probed.
func TestFlows(t *testing.T) {
	data := plugin.Flow{Purpose: "ftp data", Host: "ftp.example", Unprobed: "passive data ports, chosen by the server for each transfer"}
	for mode, control := range map[string]plugin.Flow{
		"implicit": {Purpose: "ftp control", Host: "ftp.example", Port: 990, TLS: true, ServerName: "ftp.example"},
		"explicit": {Purpose: "ftp control", Host: "ftp.example", Port: 990},
		"none":     {Purpose: "ftp control", Host: "ftp.example", Port: 990},
	} {
		c, err := loopback(t).Open(context.Background(), plugin.Config(`{"host": "ftp.example", "port": 990, "root": "/", "tls": "`+mode+`"}`), good)
		if err != nil {
			t.Fatal(err)
		}
		got, err := c.(plugin.FlowLister).Flows(context.Background())
		if err != nil || !reflect.DeepEqual(got, []plugin.Flow{control, data}) {
			t.Errorf("%s: %+v, %v", mode, got, err)
		}
	}
}
