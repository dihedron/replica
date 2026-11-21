package main

import (
	"context"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/dihedron/veho/internal/api"
	"github.com/dihedron/veho/internal/audit"
	"github.com/dihedron/veho/internal/config"
	"github.com/dihedron/veho/internal/engine"
	"github.com/dihedron/veho/internal/secrets"
	"github.com/dihedron/veho/internal/store"
	"github.com/dihedron/veho/migrations"
	"github.com/dihedron/veho/pkg/metadata"
	"github.com/dihedron/veho/pkg/plugin"
	"github.com/dihedron/veho/pkg/plugin/file"
	"github.com/dihedron/veho/pkg/plugin/process"
	"github.com/dihedron/veho/pkg/plugin/s3"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// ServeCommand is "veho serve" (.specs/veho-engine.md §2): it runs a node.
type ServeCommand struct {
	Config string `long:"config" default:"/etc/veho/veho.yaml" description:"The configuration file"`

	ready chan<- *engine.Engine // tests only: receives the engine once it runs
}

// shutdownTimeout bounds the wait for the steps in flight at a stop.
const shutdownTimeout = 30 * time.Second

// Execute runs the node until SIGTERM or SIGINT.
func (cmd *ServeCommand) Execute([]string) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	return cmd.run(ctx)
}

func (cmd *ServeCommand) run(ctx context.Context) error {
	cfg, hash, err := config.Load(cmd.Config)
	if err != nil {
		return err
	}
	slog.InfoContext(ctx, "starting", "executor", cfg.Node.ID, "region", cfg.Node.Region, "config_sha256", hash, "version", metadata.Version)
	aud, err := audit.Open(cfg.Audit.Sink, cfg.Node.ID)
	if err != nil {
		return err
	}
	pools, ring, err := connect(ctx, cfg)
	defer func() {
		for _, p := range pools {
			p.Close()
		}
	}()
	if err != nil {
		return err
	}
	dbosDSN := os.Getenv(cfg.Database.DBOS.Env)
	if err := store.CheckDSN(dbosDSN, false); err != nil {
		return fmt.Errorf("serve: database.dbos (%s): %w", cfg.Database.DBOS.Env, err)
	}
	drivers, stopPlugins, err := loadDrivers(cfg)
	defer stopPlugins()
	if err != nil {
		return err
	}
	e, err := engine.New(ctx, engine.Deps{Config: cfg, API: pools["api"], Worker: pools["worker"], DBOSURL: dbosDSN,
		Version: metadata.Version, Audit: aud, Secrets: secrets.NewStore(ring, pools["api"], pools["worker"], aud), Drivers: drivers})
	if err != nil {
		return err
	}
	// registered before it dequeues anything
	if err := e.Heartbeat(ctx); err != nil {
		return errors.Join(err, e.Shutdown(shutdownTimeout))
	}
	if err := e.Launch(); err != nil {
		return errors.Join(err, e.Shutdown(shutdownTimeout))
	}
	hctx, stopBeat := context.WithCancel(ctx)
	go e.RunHeartbeat(hctx)
	go e.RunHousekeeping(hctx)
	stopMetrics, err := serveMetrics(cfg, e, aud)
	if err != nil {
		stopBeat()
		return errors.Join(err, e.Shutdown(shutdownTimeout))
	}
	defer stopMetrics()
	apiDone, err := serveAPI(hctx, cfg, e, pools["api"])
	if err != nil {
		stopBeat()
		return errors.Join(err, e.Shutdown(shutdownTimeout))
	}
	slog.InfoContext(ctx, "running", "executor", cfg.Node.ID)
	if cmd.ready != nil {
		cmd.ready <- e
	}
	<-ctx.Done()
	slog.Info("stopping", "executor", cfg.Node.ID)
	stopBeat() // the API stops with it
	return errors.Join(<-apiDone, e.Shutdown(shutdownTimeout))
}

// connect opens the pools of veho_api and veho_worker, runs the start-up
// checks, loads the keyring and checks that node.region is a location; the
// caller closes the pools, also on an error.
func connect(ctx context.Context, cfg *config.Config) (map[string]*pgxpool.Pool, *secrets.Keyring, error) {
	pools := map[string]*pgxpool.Pool{}
	for name, env := range map[string]string{"api": cfg.Database.API.Env, "worker": cfg.Database.Worker.Env} {
		dsn := os.Getenv(env)
		if dsn == "" {
			return pools, nil, fmt.Errorf("serve: environment variable %q (database.%s.dsn_env) is not set", env, name)
		}
		p, err := store.Open(ctx, dsn, store.Options{Name: "veho/" + cfg.Node.ID + "/" + name})
		if err != nil {
			return pools, nil, fmt.Errorf("serve: %s pool: %w", name, err)
		}
		pools[name] = p
	}
	if errs := store.Checks(ctx, pools["worker"], migrations.FS); len(errs) > 0 {
		for _, err := range errs {
			slog.ErrorContext(ctx, "start-up check failed", "error", err)
		}
		return pools, nil, fmt.Errorf("serve: start-up checks: %w", errors.Join(errs...))
	}
	src, err := secrets.ParseSource(cfg.Secrets.UnwrapKey)
	if err != nil {
		return pools, nil, err
	}
	ring, err := secrets.LoadKeyring(ctx, pools["worker"], src)
	if err != nil {
		return pools, nil, fmt.Errorf("serve: %w", err)
	}
	topo, err := engine.LoadTopology(ctx, pools["worker"])
	if err != nil {
		return pools, nil, fmt.Errorf("serve: %w", err)
	}
	if _, err := topo.Position(cfg.Node.Region); err != nil {
		return pools, nil, fmt.Errorf("serve: node.region %q: %w", cfg.Node.Region, err)
	}
	return pools, ring, nil
}

// loadDrivers returns the compiled-in drivers and the process plugins of the
// configuration (.specs/veho-engine.md §6); stop kills the plugins.
func loadDrivers(cfg *config.Config) (map[string]plugin.Driver, func(), error) {
	drivers := map[string]plugin.Driver{}
	for _, d := range []plugin.Driver{file.New(), s3.New()} {
		drivers[d.Name()] = d
	}
	var procs []*process.Process
	stop := func() {
		for _, p := range procs {
			p.Kill()
		}
	}
	for _, p := range cfg.Plugins {
		sum, _ := hex.DecodeString(p.SHA256) // checked by config
		proc, err := process.Load(process.Options{Path: p.Path, SHA256: sum, RequireRootOwned: true})
		if err != nil {
			return nil, stop, fmt.Errorf("serve: plugin %s: %w", p.Name, err)
		}
		procs = append(procs, proc)
		if _, ok := drivers[p.Name]; ok {
			return nil, stop, fmt.Errorf("serve: plugin %s: a driver of that name exists", p.Name)
		}
		drivers[p.Name] = proc.Driver()
	}
	return drivers, stop, nil
}

// serveMetrics exposes the metrics on metrics.listen, unless it is off.
func serveMetrics(cfg *config.Config, e *engine.Engine, aud *audit.Logger) (func(), error) {
	if cfg.Metrics.Listen == "off" {
		return func() {}, nil
	}
	reg := prometheus.NewRegistry()
	if err := e.Register(reg); err != nil {
		return nil, fmt.Errorf("serve: metrics: %w", err)
	}
	reg.MustRegister(prometheus.NewCounterFunc(prometheus.CounterOpts{Name: "veho_audit_dropped_total", Help: "Audit events that could not be written."},
		func() float64 { return float64(aud.Dropped()) }))
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	ln, err := net.Listen("tcp", cfg.Metrics.Listen)
	if err != nil {
		return nil, fmt.Errorf("serve: metrics: %w", err)
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("the metrics listener stopped", "error", err)
		}
	}()
	return func() { _ = srv.Close() }, nil
}

// serveAPI starts the API on api.listen (.specs/veho-api-server.md §2); the
// channel gets its end once ctx is done.
func serveAPI(ctx context.Context, cfg *config.Config, e *engine.Engine, pool *pgxpool.Pool) (<-chan error, error) {
	cert, err := tls.LoadX509KeyPair(cfg.API.TLS.CertFile, cfg.API.TLS.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("serve: api.tls: %w", err)
	}
	srv, err := api.New(api.Options{Engine: e, API: pool, MaxWatches: cfg.API.MaxWatches, Ready: func(ctx context.Context) error {
		if e.Fenced() {
			return engine.ErrFenced
		}
		return pool.Ping(ctx)
	}})
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", cfg.API.Listen)
	if err != nil {
		srv.Close()
		return nil, fmt.Errorf("serve: api.listen: %w", err)
	}
	done := make(chan error, 1)
	go func() {
		err := srv.Serve(ctx, ln, cert)
		srv.Close()
		done <- err
	}()
	slog.InfoContext(ctx, "API listening", "address", ln.Addr().String())
	return done, nil
}
