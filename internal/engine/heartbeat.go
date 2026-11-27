package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// ErrFenced fails the work of a node that cannot reach the state store
// (§7): its leases may already belong to another executor.
var ErrFenced = errors.New("engine: this node is fenced: no heartbeat")

// Heartbeat records this executor as alive, on the database's clock (T-27),
// and renews the leases of its transfers.
func (e *Engine) Heartbeat(ctx context.Context) error {
	rtt, err := e.dbRTT(ctx)
	if err != nil {
		return fmt.Errorf("engine: heartbeat: %w", err)
	}
	e.metrics.dbRTT.Set(rtt.Seconds())
	var url *string
	if u := e.cfg.API.AdvertiseURL; u != "" {
		url = &u
	}
	if _, err := e.worker.Exec(ctx, `insert into veho.executors (id, region, app_version, api_url, db_rtt_us) values ($1, $2, $3, $4, $5)
		on conflict (id) do update set region = excluded.region, app_version = excluded.app_version, last_seen = now(),
			api_url = excluded.api_url, db_rtt_us = excluded.db_rtt_us`,
		e.executor, e.region, e.dbos.GetApplicationVersion(), url, rtt.Microseconds()); err != nil {
		return fmt.Errorf("engine: heartbeat: %w", err)
	}
	if _, err := e.worker.Exec(ctx, "select veho.renew_leases($1, make_interval(secs => $2))", e.executor, e.cfg.Engine.LeaseTTL.Seconds()); err != nil {
		return fmt.Errorf("engine: renew the leases: %w", err)
	}
	return nil
}

// dbRTT is the round trip to the state store: the least of three select 1,
// which the API compares between nodes to forward its calls to the nearest
// (veho-forwarding.md §2.1).
func (e *Engine) dbRTT(ctx context.Context) (time.Duration, error) {
	best := time.Duration(-1)
	for range 3 {
		start := time.Now()
		if _, err := e.worker.Exec(ctx, "select 1"); err != nil {
			return 0, fmt.Errorf("the round trip: %w", err)
		}
		if d := time.Since(start); best < 0 || d < best {
			best = d
		}
	}
	return best, nil
}

// RunHeartbeat beats every engine.heartbeat until ctx ends, and fences the
// node when no beat succeeded for lease_ttl / 2 (§7).
func (e *Engine) RunHeartbeat(ctx context.Context) {
	t := time.NewTicker(e.cfg.Engine.Heartbeat)
	defer t.Stop()
	for {
		e.beat(ctx, time.Now())
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// beat runs one heartbeat at now and updates the fencing.
func (e *Engine) beat(ctx context.Context, now time.Time) {
	hctx, cancel := context.WithTimeout(ctx, e.cfg.Engine.Heartbeat)
	err := e.Heartbeat(hctx)
	cancel()
	e.mu.Lock()
	defer e.mu.Unlock()
	fenced := e.stopped()
	switch {
	case err == nil:
		e.lastOK = now
		if fenced {
			e.stop = make(chan struct{})
			slog.InfoContext(ctx, "heartbeat restored: the node works again", "executor", e.executor)
		}
	case !fenced && now.Sub(e.lastOK) > e.cfg.Engine.LeaseTTL/2:
		close(e.stop)
		slog.ErrorContext(ctx, "no heartbeat for half the lease: the node stops its work", "executor", e.executor, "error", err)
	default:
		slog.WarnContext(ctx, "heartbeat failed", "executor", e.executor, "error", err)
	}
}

// stopped is true while fenced; the caller holds mu.
func (e *Engine) stopped() bool {
	select {
	case <-e.stop:
		return true
	default:
		return false
	}
}

// Fenced is true while the node is fenced.
func (e *Engine) Fenced() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.stopped()
}

// guard returns a context that ends when ctx does or when the node is
// fenced, for the work of a step; it fails at once while fenced (§7).
func (e *Engine) guard(ctx context.Context) (context.Context, context.CancelFunc, error) {
	e.mu.Lock()
	stop, fenced := e.stop, e.stopped()
	e.mu.Unlock()
	if fenced {
		return nil, nil, ErrFenced
	}
	gctx, cancel := context.WithCancelCause(ctx)
	go func() { // ends with gctx, which the caller's cancel ends
		select {
		case <-stop:
			cancel(ErrFenced)
		case <-gctx.Done():
		}
	}()
	return gctx, func() { cancel(nil) }, nil
}
