// Package process runs storage drivers in their own processes, with
// hashicorp/go-plugin over gRPC (.specs/veho-plugins.md §6): Serve is a
// plugin binary's main, Load starts one from the core. The core sees a plugin
// as a plugin.Driver like any compiled-in one, so the conformance suite runs
// under both loaders.
package process

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/dihedron/veho/pkg/plugin"
	pb "github.com/dihedron/veho/pkg/plugin/process/proto"
	"github.com/hashicorp/go-hclog"
	goplugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"
)

// Handshake is the protocol of veho's plugins (PLG-12): a plugin of another
// protocol version is refused before the first call.
var Handshake = goplugin.HandshakeConfig{
	ProtocolVersion:  1,
	MagicCookieKey:   "VEHO_PLUGIN",
	MagicCookieValue: "4e2b7f0c9a1d4c3b8f6e5a7d2c1b0a99",
}

const pluginName = "driver"

// grpcPlugin binds the service to go-plugin.
type grpcPlugin struct {
	goplugin.NetRPCUnsupportedPlugin
	impl plugin.Driver
}

func (p *grpcPlugin) GRPCServer(_ *goplugin.GRPCBroker, s *grpc.Server) error {
	pb.RegisterDriverServer(s, newServer(p.impl))
	return nil
}

func (p *grpcPlugin) GRPCClient(ctx context.Context, _ *goplugin.GRPCBroker, c *grpc.ClientConn) (any, error) {
	rpc := pb.NewDriverClient(c)
	desc, err := rpc.Describe(ctx, &pb.Empty{})
	if err != nil {
		return nil, fmt.Errorf("plugin: describe: %w", err)
	}
	return &remote{rpc: rpc, desc: desc}, nil
}

// Options locate and check a plugin binary.
type Options struct {
	// Path of the binary.
	Path string
	// SHA256 the binary must have (PLG-14), from the release's manifest.
	SHA256 []byte
	// RequireRootOwned refuses a binary, or a directory holding it, that is
	// not owned by root or is writable by others than root (PLG-14). Only
	// tests turn it off.
	RequireRootOwned bool
	// Logger receives the plugin's log (PLG-19); default slog.Default().
	Logger *slog.Logger
}

// Process is a running plugin, restarted on the next Open after it died
// (PLG-18).
type Process struct {
	opts   Options
	mu     sync.Mutex
	client *goplugin.Client
	remote *remote
}

// Load checks and starts a plugin binary, and describes its driver.
func Load(opts Options) (*Process, error) {
	if len(opts.SHA256) != sha256.Size {
		return nil, errors.New("plugin: the binary's SHA-256 is required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.RequireRootOwned {
		for _, p := range []string{opts.Path, filepath.Dir(opts.Path)} {
			if err := rootOwned(p); err != nil {
				return nil, err
			}
		}
	}
	p := &Process{opts: opts}
	if err := p.start(); err != nil {
		return nil, err
	}
	return p, nil
}

// rootOwned fails unless path is owned by root and writable by root only.
func rootOwned(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("plugin: %w", err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != 0 || fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("plugin: %s must be owned by root and writable by root only", path)
	}
	return nil
}

// start launches the binary; go-plugin checks its checksum first, sets up
// mutual TLS (PLG-13) and passes it none of the core's environment.
func (p *Process) start() error {
	logger := hclog.New(&hclog.LoggerOptions{
		Name:   "plugin." + filepath.Base(p.opts.Path),
		Output: slogWriter{p.opts.Logger},
		Level:  hclog.Info,
	})
	client := goplugin.NewClient(&goplugin.ClientConfig{
		HandshakeConfig:  Handshake,
		Plugins:          goplugin.PluginSet{pluginName: &grpcPlugin{}},
		Cmd:              exec.Command(p.opts.Path), //nolint:gosec // the path is configuration, and its checksum is checked
		SecureConfig:     &goplugin.SecureConfig{Checksum: p.opts.SHA256, Hash: sha256.New()},
		AllowedProtocols: []goplugin.Protocol{goplugin.ProtocolGRPC},
		AutoMTLS:         true,
		SkipHostEnv:      true,
		Logger:           logger,
	})
	conn, err := client.Client()
	if err != nil {
		client.Kill()
		return fmt.Errorf("plugin: start %s: %w", p.opts.Path, err)
	}
	raw, err := conn.Dispense(pluginName)
	if err != nil {
		client.Kill()
		return fmt.Errorf("plugin: start %s: %w", p.opts.Path, err)
	}
	p.client, p.remote = client, raw.(*remote)
	return nil
}

// current is the live remote driver, after a restart if the process died.
func (p *Process) current() (*remote, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.client.Exited() {
		p.opts.Logger.Warn("plugin process exited: restarting", "plugin", p.opts.Path)
		if err := p.start(); err != nil {
			return nil, plugin.Wrap(err, plugin.Transient)
		}
	}
	return p.remote, nil
}

// Kill stops the process.
func (p *Process) Kill() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.client.Kill()
}

// Driver is the plugin as a plugin.Driver. Its description is the one of the
// first start: a restart runs the same, checked binary.
func (p *Process) Driver() plugin.Driver { return restarting{p: p, desc: p.remote} }

type restarting struct {
	p    *Process
	desc *remote
}

func (d restarting) Name() string                      { return d.desc.Name() }
func (d restarting) Schemes() []string                 { return d.desc.Schemes() }
func (d restarting) Capabilities() plugin.Capabilities { return d.desc.Capabilities() }
func (d restarting) ConfigSchema() []byte              { return d.desc.ConfigSchema() }

func (d restarting) ParseRef(url string) (plugin.Ref, error) {
	r, err := d.p.current()
	if err != nil {
		return plugin.Ref{}, err
	}
	return r.ParseRef(url)
}

func (d restarting) Open(ctx context.Context, cfg plugin.Config, creds plugin.Credentials) (plugin.Client, error) {
	r, err := d.p.current()
	if err != nil {
		return nil, err
	}
	return r.Open(ctx, cfg, creds)
}

// slogWriter writes the plugin's log lines through slog (PLG-19).
type slogWriter struct{ l *slog.Logger }

func (w slogWriter) Write(b []byte) (int, error) {
	for line := range bytes.Lines(b) {
		if line = bytes.TrimSpace(line); len(line) > 0 {
			w.l.Info("plugin", "line", string(line))
		}
	}
	return len(b), nil
}
