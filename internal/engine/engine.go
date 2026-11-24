// Package engine runs veho's durable workflows on DBOS over the state store
// (.specs/veho-engine.md): acceptance, the executor registry, the job and
// transfer workflows and the housekeeping loop.
package engine

import (
	"cmp"
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
	"github.com/dihedron/veho/internal/store"
	"github.com/dihedron/veho/pkg/plugin"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/semaphore"
)

// WorkflowVersion is the version of the workflows' code as DBOS sees it
// (EXT-5): DBOS recovers and dequeues only the workflows of its own version.
// It is not the build's version, which would strand the work of every older
// build at each upgrade; raise it only when a change makes a recorded
// workflow unsafe to replay (its steps, their order or their results), and
// upgrade as the Operator's Guide says (AC-20). A variable, so that a build
// can set it with -ldflags -X.
var WorkflowVersion = "1"

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
	// Version is the application version of the workflows (EXT-5); empty
	// is WorkflowVersion. Tests set their own, to keep apart from nodes.
	Version string
	Audit   audit.Auditor
	// Secrets gives the attempts their credentials (SEC-10).
	Secrets *secrets.Store
	// Drivers are the storage drivers by name (§6).
	Drivers map[string]plugin.Driver
	Logger  *slog.Logger
}

// Engine is one node's engine.
type Engine struct {
	cfg         *config.Config
	api, worker *pgxpool.Pool
	dbos        dbos.Context
	audit       audit.Auditor
	secrets     *secrets.Store
	drivers     map[string]plugin.Driver
	queues      map[string]dbos.Queue
	metrics     *metrics
	executor    string
	region      string
	// dbosPool is DBOS's pool, closed after DBOS (store.DBOSPool)
	dbosPool *pgxpool.Pool
	// partMemory is the node's budget of part bytes in memory (NFR-5)
	partMemory *semaphore.Weighted
	// active holds the IDs of the workflows this process is executing, for
	// recoverOwn
	active sync.Map

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
	// not cancelled with ctx: DBOS durably cancels a workflow whose context
	// ends for any cause but its own Shutdown, and a stopping node must leave
	// its work PENDING for adoption (found by AC-4); Shutdown stops it
	pool, err := store.DBOSPool(d.DBOSURL, "veho/"+c.Node.ID+"/dbos")
	if err != nil {
		return nil, fmt.Errorf("engine: %w", err)
	}
	dc, err := dbos.NewContext(context.WithoutCancel(ctx), dbos.Config{
		AppName:            AppName,
		SystemDBPool:       pool,
		ApplicationVersion: cmp.Or(d.Version, WorkflowVersion),
		ExecutorID:         c.Node.ID,
		Logger:             d.Logger,
	})
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("engine: %w", err)
	}
	e := &Engine{cfg: c, api: d.API, worker: d.Worker, dbos: dc, audit: d.Audit, secrets: d.Secrets, drivers: d.Drivers, executor: c.Node.ID, region: c.Node.Region,
		dbosPool:   pool,
		partMemory: semaphore.NewWeighted(int64(c.Engine.PartMemoryMiB) << 20),
		lastOK:     time.Now(), stop: make(chan struct{}), queues: map[string]dbos.Queue{}, metrics: newMetrics()}
	for _, q := range []string{QueueJobs, QueueTransfer} {
		// one partition per tenant (EXT-3a): a tenant runs at most
		// tenant_concurrency workflows of a queue at a time
		queue, err := dbos.RegisterQueue(dc, q,
			dbos.WithPartitionConcurrency(c.Engine.TenantConcurrency),
			dbos.WithWorkerConcurrency(c.Engine.WorkerConcurrency),
			dbos.WithQueueOnConflict(dbos.QueueConflictUpdateIfLatestVersion))
		if err != nil {
			err = errors.Join(fmt.Errorf("engine: queue %s: %w", q, err), dbos.Shutdown(dc, 5*time.Second))
			pool.Close()
			return nil, err
		}
		e.queues[q] = queue
	}
	dbos.RegisterWorkflow(dc, e.jobWorkflow, dbos.WithWorkflowName(WorkflowJob))
	dbos.RegisterWorkflow(dc, e.transferWorkflow, dbos.WithWorkflowName(WorkflowTransfer))
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
	defer e.dbosPool.Close()
	return dbos.Shutdown(e.dbos, timeout)
}
