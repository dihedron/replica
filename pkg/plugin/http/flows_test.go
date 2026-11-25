package http

import (
	"context"
	"reflect"
	"testing"

	"github.com/dihedron/veho/pkg/plugin"
)

// NC-T1: the base URL's flow.
func TestFlows(t *testing.T) {
	d, _ := Factory(nil)
	for cfg, want := range map[string]plugin.Flow{
		`{"base_url": "https://files.example:8443/pub/"}`: {Purpose: "http", Host: "files.example", Port: 8443, TLS: true, ServerName: "files.example"},
		`{"base_url": "http://files.example/pub/"}`:       {Purpose: "http", Host: "files.example", Port: 80},
	} {
		c, err := d.Open(context.Background(), plugin.Config(cfg), nil)
		if err != nil {
			t.Fatal(err)
		}
		got, err := c.(plugin.FlowLister).Flows(context.Background())
		if err != nil || !reflect.DeepEqual(got, []plugin.Flow{want}) {
			t.Errorf("%s: %+v, %v", cfg, got, err)
		}
	}
}
