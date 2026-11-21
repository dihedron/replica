// Package engine runs veho's durable workflows on DBOS over the state store
// (.specs/veho-engine.md): acceptance, the executor registry, the job and
// transfer workflows and the housekeeping loop.
package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/dihedron/veho/internal/audit"
	"github.com/dihedron/veho/internal/config"
	"github.com/dihedron/veho/internal/secrets"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The DBOS application and its queues (§2).
const (
	AppName       = "veho"
	QueueJobs     = "jobs"
	QueueTransfer = "transfers"
	// workflow names, fixed so that they do not depend on the Go package path
	WorkflowJob      = "ReplicateJob"
	WorkflowTransfer = "Transfer"
)

// Deps are what the engine runs on.
type Deps struct {
	Config *config.Config
	// API and Worker are the pools of veho_api and veho_worker.
	API, Worker *pgxpool.Pool
	// DBOSURL is veho_dbos's DSN, for DBOS's own pool.
	DBOSURL string
	// Version is the application version of the workflows (EXT-5).
	Version string
	Audit   audit.Auditor
	// Secrets gives the attempts their credentials (SEC-10).
	Secrets *secrets.Store
	Logger  *slog.Logger
}

// Engine is one node's engine.
type Engine struct {
	cfg         *config.Config
	api, worker *pgxpool.Pool
	dbos        dbos.Context
	audit       audit.Auditor
	secrets     *secrets.Store
	executor    string
	region      string

	mu     sync.Mutex
	lastOK time.Time
	stop   chan struct{} // closed while the node is fenced (§7)

	beforeCommit func() error // tests only: fails Submit after its inserts
}

// New creates the DBOS context and registers the queues; Launch starts it.
func New(ctx context.Context, d Deps) (*Engine, error) {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	c := d.Config
	dc, err := dbos.NewContext(ctx, dbos.Config{
		AppName:            AppName,
		DatabaseURL:        d.DBOSURL,
		ApplicationVersion: d.Version,
		ExecutorID:         c.Node.ID,
		Logger:             d.Logger,
	})
	if err != nil {
		return nil, fmt.Errorf("engine: %w", err)
	}
	e := &Engine{cfg: c, api: d.API, worker: d.Worker, dbos: dc, audit: d.Audit, secrets: d.Secrets, executor: c.Node.ID, region: c.Node.Region,
		lastOK: time.Now(), stop: make(chan struct{})}
	for _, q := range []string{QueueJobs, QueueTransfer} {
		// one partition per tenant (EXT-3a): a tenant runs at most
		// tenant_concurrency workflows of a queue at a time
		if _, err := dbos.RegisterQueue(dc, q,
			dbos.WithPartitionConcurrency(c.Engine.TenantConcurrency),
			dbos.WithWorkerConcurrency(c.Engine.WorkerConcurrency),
			dbos.WithQueueOnConflict(dbos.QueueConflictUpdateIfLatestVersion)); err != nil {
			return nil, errors.Join(fmt.Errorf("engine: queue %s: %w", q, err), dbos.Shutdown(dc, 5*time.Second))
		}
	}
	return e, nil
}

// Launch starts DBOS: this node then runs the workflows of the queues.
func (e *Engine) Launch() error {
	if err := dbos.Launch(e.dbos); err != nil {
		return fmt.Errorf("engine: launch: %w", err)
	}
	return nil
}

// Shutdown stops DBOS, waiting up to timeout for the steps in flight.
func (e *Engine) Shutdown(timeout time.Duration) error {
	return dbos.Shutdown(e.dbos, timeout)
}
