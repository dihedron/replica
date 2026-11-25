package s3

import (
	"context"
	"reflect"
	"testing"

	"github.com/dihedron/veho/pkg/plugin"
)

// NC-T1: the endpoint's flow, outside the dial guard as the driver is.
func TestFlows(t *testing.T) {
	creds := plugin.Credentials{"access_key_id": "a", "secret_access_key": "s"}
	for cfg, want := range map[string]plugin.Flow{
		`{"endpoint": "http://10.254.0.20:3900", "region": "garage", "bucket": "b", "path_style": true}`: {Purpose: "s3", Host: "10.254.0.20", Port: 3900, Unguarded: true},
		`{"endpoint": "https://s3.example", "region": "r", "bucket": "b"}`:                               {Purpose: "s3", Host: "s3.example", Port: 443, TLS: true, ServerName: "s3.example", Unguarded: true},
		`{"region": "eu-west-1", "bucket": "b"}`:                                                         {Purpose: "s3", Host: "b.s3.eu-west-1.amazonaws.com", Port: 443, TLS: true, ServerName: "b.s3.eu-west-1.amazonaws.com", Unguarded: true},
		`{"region": "eu-west-1", "bucket": "b", "path_style": true}`:                                     {Purpose: "s3", Host: "s3.eu-west-1.amazonaws.com", Port: 443, TLS: true, ServerName: "s3.eu-west-1.amazonaws.com", Unguarded: true},
	} {
		c, err := New().Open(context.Background(), plugin.Config(cfg), creds)
		if err != nil {
			t.Fatal(err)
		}
		got, err := c.(plugin.FlowLister).Flows(context.Background())
		if err != nil || !reflect.DeepEqual(got, []plugin.Flow{want}) {
			t.Errorf("%s: %+v, %v", cfg, got, err)
		}
	}
}
