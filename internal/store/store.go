// Package store is veho's access to PostgreSQL (.specs/veho-store.md): the
// connection pools, the tenant-scoped transaction, the advisory lock of the
// singletons and the start-up checks. The pools, the DSN check and TryLock
// are copied from ligo 9f11da9 (internal/store), with one pool per role.
package store

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// defaultStatementTimeout applies when Options leaves it unset.
const defaultStatementTimeout = 5 * time.Second

// ErrInsecureConnection refuses a DSN without sslmode=verify-full (STO-8).
var ErrInsecureConnection = errors.New("database connections must use sslmode=verify-full")

// Options are a pool's settings; a zero field keeps the default.
type Options struct {
	// Name is the sessions' application_name, veho/<executor-id>/<pool>.
	Name string
	// StatementTimeout bounds every statement (default 5 s).
	StatementTimeout time.Duration
	// MaxConns is pgx's connection limit.
	MaxConns int32
	// AllowInsecure accepts a DSN without verify-full: tests only.
	AllowInsecure bool
}

// Open connects a pool and pings it.
func Open(ctx context.Context, dsn string, o Options) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	if !o.AllowInsecure && !verifies(cfg.ConnConfig.Config) {
		return nil, ErrInsecureConnection
	}
	cfg.ConnConfig.RuntimeParams["statement_timeout"] = strconv.FormatInt(cmp.Or(o.StatementTimeout, defaultStatementTimeout).Milliseconds(), 10)
	if o.Name != "" {
		cfg.ConnConfig.RuntimeParams["application_name"] = o.Name
	}
	if o.MaxConns > 0 {
		cfg.MaxConns = o.MaxConns
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: %w", err)
	}
	return pool, nil
}

// verifies is true when every connection attempt verifies the server: TLS,
// certificate and host name, no plaintext or unverified fallback.
func verifies(cfg pgconn.Config) bool {
	if cfg.TLSConfig == nil || cfg.TLSConfig.InsecureSkipVerify {
		return false
	}
	for _, f := range cfg.Fallbacks {
		if f.TLSConfig == nil || f.TLSConfig.InsecureSkipVerify {
			return false
		}
	}
	return true
}

// CheckDSN refuses a DSN that does not verify the server, as Open does: for
// DBOS's own pool.
func CheckDSN(dsn string, allowInsecure bool) error {
	cfg, err := pgconn.ParseConfig(dsn)
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	if !allowInsecure && !verifies(*cfg) {
		return ErrInsecureConnection
	}
	return nil
}

// Connect opens one connection, for veho migrate, with the same check.
func Connect(ctx context.Context, dsn string, allowInsecure bool) (*pgx.Conn, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	if !allowInsecure && !verifies(cfg.Config) {
		return nil, ErrInsecureConnection
	}
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	return conn, nil
}

// InTenant runs fn in a transaction whose tenant is tenant (§4.1): it begins,
// sets veho.tenant_id for the transaction only, runs fn and commits; an error
// of fn rolls back and is returned as is.
func InTenant(ctx context.Context, pool *pgxpool.Pool, tenant uuid.UUID, fn func(pgx.Tx) error) error {
	if tenant == uuid.Nil() {
		return errors.New("store: no tenant")
	}
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "select set_config('veho.tenant_id', $1, true)", tenant.String()); err != nil {
			slog.ErrorContext(ctx, "cannot set the tenant", "tenant_id", tenant.String(), "error", err)
			return fmt.Errorf("store: tenant %s: %w", tenant, err)
		}
		return fn(tx)
	})
}

// TryLock takes the session advisory lock name of a singleton's pass, on a
// connection of its own: a pass holding a connection of pool while asking
// pool for another would wait for good if passes filled the pool. release
// ends the lock with the connection; it is nil unless locked.
// shortcut: it dials once per pass; give the locks a small pool of their own
// if that ever shows on the database.
func TryLock(ctx context.Context, pool *pgxpool.Pool, name string) (release func(), locked bool, err error) {
	conn, err := pgx.ConnectConfig(ctx, pool.Config().ConnConfig)
	if err != nil {
		return nil, false, fmt.Errorf("store: lock %s: %w", name, err)
	}
	release = func() { _ = conn.Close(context.WithoutCancel(ctx)) }
	if err := conn.QueryRow(ctx, "select pg_try_advisory_lock(hashtext($1))", name).Scan(&locked); err != nil {
		release()
		return nil, false, fmt.Errorf("store: lock %s: %w", name, err)
	}
	if !locked {
		release()
		return nil, false, nil
	}
	return release, true, nil
}
