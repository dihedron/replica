package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/dihedron/veho/internal/audit"
	"github.com/dihedron/veho/internal/config"
	"github.com/dihedron/veho/internal/engine"
	"github.com/dihedron/veho/internal/secrets"
	"github.com/dihedron/veho/internal/store"
	"github.com/dihedron/veho/migrations"
	"github.com/dihedron/veho/pkg/metadata"
	"github.com/jackc/pgx/v5/pgxpool"
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
	e, err := engine.New(ctx, engine.Deps{Config: cfg, API: pools["api"], Worker: pools["worker"], DBOSURL: dbosDSN,
		Version: metadata.Version, Audit: aud, Secrets: secrets.NewStore(ring, pools["api"], pools["worker"], aud)})
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
	slog.InfoContext(ctx, "running", "executor", cfg.Node.ID)
	if cmd.ready != nil {
		cmd.ready <- e
	}
	<-ctx.Done()
	slog.Info("stopping", "executor", cfg.Node.ID)
	stopBeat()
	return e.Shutdown(shutdownTimeout)
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
