package process

import (
	"context"
	"reflect"
	"testing"

	"github.com/dihedron/veho/pkg/plugin"
	"github.com/dihedron/veho/pkg/plugin/file"
)

type flowDriver struct {
	plugin.Driver
	flows []plugin.Flow
}

func (d flowDriver) Open(ctx context.Context, cfg plugin.Config, creds plugin.Credentials) (plugin.Client, error) {
	c, err := d.Driver.Open(ctx, cfg, creds)
	return flowClient{c, d.flows}, err
}

type flowClient struct {
	plugin.Client
	flows []plugin.Flow
}

func (c flowClient) Flows(context.Context) ([]plugin.Flow, error) { return c.flows, nil }

// NC-T1 over gRPC: the flows cross the process boundary as they are; a
// driver without flows lists none; more than MaxFlows is refused.
func TestFlows(t *testing.T) {
	ctx := context.Background()
	want := []plugin.Flow{
		{Purpose: "keystone", Host: "keystone.example", Port: 5000, TLS: true, ServerName: "keystone.example", CACert: "PEM"},
		{Purpose: "sftp", Host: "10.0.0.1", Port: 22, Handshake: "ssh"},
		{Purpose: "ftp data", Host: "ftp.example", Unprobed: "passive ports chosen by the server"},
	}
	many := make([]plugin.Flow, plugin.MaxFlows+1)
	for name, tc := range map[string]struct {
		d    plugin.Driver
		want []plugin.Flow
		ok   bool
	}{
		"flows":    {flowDriver{file.New(), want}, want, true},
		"none":     {file.New(), nil, true},
		"too many": {flowDriver{file.New(), many}, nil, false},
	} {
		c, err := inProcess(t, tc.d).Open(ctx, fileConfig(t), nil)
		if err != nil {
			t.Fatal(err)
		}
		got, err := c.(plugin.FlowLister).Flows(ctx)
		if (err == nil) != tc.ok || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: %+v, %v", name, got, err)
		}
	}
}
