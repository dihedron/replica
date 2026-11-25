package process

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/dihedron/veho/pkg/plugin"
	"github.com/dihedron/veho/pkg/plugin/file"
	pb "github.com/dihedron/veho/pkg/plugin/process/proto"
)

// pullDriver is the file driver whose clients presign and whose uploads
// pull.
type pullDriver struct{ plugin.Driver }

func (d pullDriver) Open(ctx context.Context, cfg plugin.Config, creds plugin.Credentials) (plugin.Client, error) {
	c, err := d.Driver.Open(ctx, cfg, creds)
	return pullClient{c}, err
}

type pullClient struct{ plugin.Client }

func (pullClient) PresignRead(_ context.Context, ref plugin.Ref, version string, ttl time.Duration) (string, error) {
	return "https://src.example/" + ref.Key + "?v=" + version + "&ttl=" + ttl.String(), nil
}

func (c pullClient) Begin(ctx context.Context, ref plugin.Ref, info plugin.ObjectInfo, opts plugin.PutOptions) (plugin.Upload, error) {
	u, err := c.Client.Begin(ctx, ref, info, opts)
	return pullUpload{u}, err
}

type pullUpload struct{ plugin.Upload }

func (pullUpload) PullTTL() time.Duration { return 90 * time.Minute }

func (pullUpload) Pull(_ context.Context, url string) (plugin.Ref, error) {
	return plugin.Ref{Key: "pulled from " + url}, nil
}

// inProcess serves d over an in-memory gRPC connection.
func inProcess(t *testing.T, d plugin.Driver) plugin.Driver {
	t.Helper()
	ln := bufconn.Listen(1 << 20)
	s := grpc.NewServer()
	pb.RegisterDriverServer(s, newServer(d))
	go func() { _ = s.Serve(ln) }()
	t.Cleanup(s.Stop)
	conn, err := grpc.NewClient("passthrough:///bufconn", grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		return ln.DialContext(ctx)
	}), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	r, err := (&grpcPlugin{}).GRPCClient(context.Background(), nil, conn)
	if err != nil {
		t.Fatal(err)
	}
	return r.(plugin.Driver)
}

// PresignRead and Pull cross the process boundary; a driver without them
// answers Permanent.
func TestPresignAndPull(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		d    plugin.Driver
		ok   bool
	}{{"pulling", pullDriver{file.New()}, true}, {"plain", file.New(), false}} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := inProcess(t, tc.d).Open(ctx, fileConfig(t), nil)
			if err != nil {
				t.Fatal(err)
			}
			u, err := c.(plugin.Presigner).PresignRead(ctx, plugin.Ref{Key: "k"}, "v1", time.Hour)
			if tc.ok != (err == nil) || tc.ok && u != "https://src.example/k?v=v1&ttl=1h0m0s" {
				t.Fatalf("presign: %q, %v", u, err)
			}
			if !tc.ok && plugin.ClassOf(err) != plugin.Permanent {
				t.Fatalf("presign: %v", err)
			}
			up, err := c.Begin(ctx, plugin.Ref{Key: "k"}, plugin.ObjectInfo{}, plugin.PutOptions{})
			if err != nil {
				t.Fatal(err)
			}
			p := up.(plugin.Puller)
			ref, err := p.Pull(ctx, "https://x")
			switch {
			case tc.ok && (p.PullTTL() != 90*time.Minute || err != nil || ref.Key != "pulled from https://x"):
				t.Fatalf("pull: %s, %+v, %v", p.PullTTL(), ref, err)
			case !tc.ok && (p.PullTTL() != 0 || plugin.ClassOf(err) != plugin.Permanent):
				t.Fatalf("pull on a plain upload: %s, %v", p.PullTTL(), err)
			}
			_ = up.Abort(ctx)
		})
	}
}
