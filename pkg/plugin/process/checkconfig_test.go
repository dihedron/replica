package process

import (
	"context"
	"strings"
	"testing"

	"github.com/dihedron/veho/pkg/plugin"
	"github.com/dihedron/veho/pkg/plugin/file"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// plainDriver hides file's CheckConfig: a driver without one.
type plainDriver struct{ d plugin.Driver }

func (p plainDriver) Name() string                      { return p.d.Name() }
func (p plainDriver) Schemes() []string                 { return p.d.Schemes() }
func (p plainDriver) Capabilities() plugin.Capabilities { return p.d.Capabilities() }
func (p plainDriver) ConfigSchema() []byte              { return p.d.ConfigSchema() }
func (p plainDriver) ParseRef(u string) (plugin.Ref, error) {
	return p.d.ParseRef(u)
}
func (p plainDriver) Open(ctx context.Context, cfg plugin.Config, creds plugin.Credentials) (plugin.Client, error) {
	return p.d.Open(ctx, cfg, creds)
}

// CV-T3: CheckConfig crosses the process boundary with the driver's
// message; a driver without it passes; an older plugin (Unimplemented)
// passes.
func TestCheckConfigRemote(t *testing.T) {
	ctx := context.Background()
	d := inProcess(t, file.New())
	cc, ok := d.(plugin.ConfigChecker)
	if !ok {
		t.Fatal("the remote driver is no ConfigChecker")
	}
	err := cc.CheckConfig(ctx, plugin.Config(`{"root": "relative"}`))
	if err == nil || plugin.ClassOf(err) != plugin.Permanent || !strings.Contains(err.Error(), "root") {
		t.Fatalf("refused: %v", err)
	}
	if err := inProcess(t, plainDriver{file.New()}).(plugin.ConfigChecker).CheckConfig(ctx, plugin.Config(`{"root": "relative"}`)); err != nil {
		t.Fatalf("a driver without CheckConfig: %v", err)
	}
	if err := checkConfigAnswer(ctx, status.Error(codes.Unimplemented, "unknown method CheckConfig")); err != nil {
		t.Fatalf("an older plugin: %v", err)
	}
}
