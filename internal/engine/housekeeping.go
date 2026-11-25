package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"uuid"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/dihedron/veho/internal/audit"
	"github.com/dihedron/veho/internal/store"
	"github.com/jackc/pgx/v5"
)

// The housekeeping's constants (veho-audit-housekeeping.md §3).
const (
	housekeepingLock = "veho_housekeeping"
	// stale: a job quiet for longer has its workflow checked
	stale = 60 * time.Second
	// maxAdoptions fails a transfer that keeps killing executors (AC-19)
	maxAdoptions = 20
	// executorRetention keeps the rows of nodes gone
	executorRetention = 7 * 24 * time.Hour
	batch             = 500
	// classInternal is the class of a transfer the engine itself fails, not
	// a driver: a poison pill, a workflow that ended without marking it
	// (AC-19, veho-job-lifecycle.md §7)
	classInternal = "Internal"
)

// RunHousekeeping runs Housekeep every housekeeping.interval until ctx ends.
func (e *Engine) RunHousekeeping(ctx context.Context) {
	t := time.NewTicker(e.cfg.Housekeeping.Interval)
	defer t.Stop()
	for {
		if _, err := e.recoverOwn(ctx); err != nil {
			slog.WarnContext(ctx, "recovering this node's own workflows", "error", err)
		}
		if _, err := e.Housekeep(ctx); err != nil {
			slog.WarnContext(ctx, "housekeeping", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Housekeep runs one pass if this node takes the lock (HK-1); every task
// runs even if one fails (HK-4), and their errors are returned together.
func (e *Engine) Housekeep(ctx context.Context) (bool, error) {
	// a node cut off mid-pass frees the lock after two intervals at most
	release, locked, err := store.TryLock(ctx, e.worker, housekeepingLock, 2*e.cfg.Housekeeping.Interval)
	if err != nil || !locked {
		return false, err
	}
	defer release()
	var errs []error
	// in this order: the repairs first, the periodic network checks last,
	// which open endpoints and may take the rest of the pass
	for _, t := range []struct {
		name string
		run  func(context.Context) error
	}{
		{"adopt", e.adopt}, {"deadlines", e.deadlines}, {"zombies", e.zombies}, {"leases", e.leases},
		{"retention", e.retain}, {"rewrap", e.rewrap}, {"checks", e.periodicChecks},
	} {
		if err := t.run(ctx); err != nil {
			slog.ErrorContext(ctx, "housekeeping task failed", "task", t.name, "error", err)
			errs = append(errs, fmt.Errorf("%s: %w", t.name, err))
		}
	}
	return true, errors.Join(errs...)
}

func (e *Engine) system(ctx context.Context, action, tenant string, details any) {
	e.audit.Emit(ctx, audit.Event{Action: action, Outcome: audit.Success, TenantID: tenant, Principal: "system:" + e.executor, Details: details})
}

// adopt resumes the PENDING workflows of the executors whose heartbeat is
// older than the lease, on the database's clock (§7, EXT-2b); a transfer
// adopted more than maxAdoptions times fails.
func (e *Engine) adopt(ctx context.Context) error {
	rows, err := e.worker.Query(ctx, "select id from veho.executors where last_seen < now() - make_interval(secs => $1) and id <> $2",
		e.cfg.Engine.LeaseTTL.Seconds(), e.executor)
	if err != nil {
		return err
	}
	dead, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || len(dead) == 0 {
		return err
	}
	pending, err := dbos.ListWorkflows(e.dbos, dbos.WithFilterExecutorIDs(dead...), dbos.WithFilterStatus(dbos.WorkflowStatusPending),
		dbos.WithFilterLoadInput(false), dbos.WithFilterLoadOutput(false))
	if err != nil {
		return err
	}
	var resume []string
	for _, w := range pending {
		if t, ok := transferOf(w.ID); ok {
			var tenant uuid.UUID
			var n int
			if err := e.worker.QueryRow(ctx, "select tenant_id, adoptions from veho.adopted($1)", t).Scan(&tenant, &n); err != nil {
				return err
			}
			if n > maxAdoptions {
				if err := e.failAdopted(ctx, tenant, t, w.ID); err != nil {
					return err
				}
				continue
			}
		}
		resume = append(resume, w.ID)
	}
	if len(resume) == 0 {
		return nil
	}
	if _, err := dbos.ResumeWorkflows[string](e.dbos, resume); err != nil {
		return err
	}
	e.metrics.adoptions.Add(float64(len(resume)))
	e.system(ctx, "SYSTEM_ZOMBIE_ADOPTED", "", map[string]any{"executors": dead, "workflows": resume})
	slog.InfoContext(ctx, "adopted the work of dead executors", "executors", dead, "workflows", len(resume))
	return nil
}

// transferOf parses a Transfer workflow ID: transfer/<id>[/retry-n].
func transferOf(id string) (uuid.UUID, bool) {
	rest, ok := strings.CutPrefix(id, "transfer/")
	if !ok {
		return uuid.UUID{}, false
	}
	t, err := uuid.Parse(strings.SplitN(rest, "/", 2)[0])
	return t, err == nil
}

// failAdopted ends a poison transfer FAILED and cancels its workflow.
func (e *Engine) failAdopted(ctx context.Context, tenant, transfer uuid.UUID, workflow string) error {
	msg, _ := json.Marshal(map[string]string{"class": classInternal, "message": fmt.Sprintf("adopted more than %d times", maxAdoptions)})
	if err := store.InTenant(ctx, e.worker, tenant, func(tx pgx.Tx) error {
		var job uuid.UUID
		if err := tx.QueryRow(ctx, `update veho.transfers set state = 'FAILED', last_error = $2, finished_at = now(), fence = fence + 1,
			lease_owner = null, lease_expires_at = null where id = $1 and state <> all($3) returning job_id`, transfer, msg, terminal).Scan(&job); err != nil {
			return err
		}
		return e.decide(ctx, tx, job)
	}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if err := dbos.CancelWorkflow(e.dbos, workflow); err != nil {
		return err
	}
	e.system(ctx, "SYSTEM_ZOMBIE_REPAIRED", tenant.String(), map[string]string{"transfer": transfer.String(), "reason": "too many adoptions"})
	return nil
}

// deadlines fails the jobs past their deadline and cancels their transfers,
// which stop at their next claim (§4.5).
func (e *Engine) deadlines(ctx context.Context) error {
	return e.each(ctx, "select tenant_id, job_id from veho.expired_jobs($1)", func(tenant, job uuid.UUID) error {
		var expired bool
		if err := store.InTenant(ctx, e.worker, tenant, func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `update veho.jobs set state = 'FAILED', last_error = '{"class": "Permanent", "message": "deadline passed"}',
				updated_at = now(), finished_at = now(), settled_at = now() where id = $1 and state in ('ACCEPTED', 'RUNNING') and deadline_at < now()`, job)
			if err != nil || tag.RowsAffected() == 0 {
				return err
			}
			expired = true
			_, err = tx.Exec(ctx, `update veho.transfers set state = 'CANCELLED', finished_at = now(), fence = fence + 1,
				lease_owner = null, lease_expires_at = null where job_id = $1 and state <> all($2)`, job, terminal)
			return err
		}); err != nil || !expired {
			return err
		}
		e.system(ctx, "SYSTEM_JOB_EXPIRED", tenant.String(), map[string]string{"job": job.String()})
		return nil
	})
}

// zombies repairs the quiet jobs whose workflow DBOS does not know (it is
// started again) or that ended without marking the job (it is decided
// again, with the transfers left marked FAILED) (§7).
func (e *Engine) zombies(ctx context.Context) error {
	return e.each(ctx, "select tenant_id, job_id from veho.quiet_jobs(make_interval(secs => $2), $1)", func(tenant, job uuid.UUID) error {
		ws, err := dbos.ListWorkflows(e.dbos, dbos.WithFilterWorkflowIDPrefix(JobWorkflowID(job)), dbos.WithFilterLoadInput(false), dbos.WithFilterLoadOutput(false))
		if err != nil {
			return err
		}
		if len(ws) == 0 {
			if _, err := dbos.Enqueue[string](e.dbos, QueueJobs, WorkflowJob, JobInput{Tenant: tenant, Job: job},
				dbos.WithEnqueueWorkflowID(JobWorkflowID(job)), dbos.WithEnqueueQueuePartitionKey(tenant.String())); err != nil {
				return err
			}
			e.system(ctx, "SYSTEM_ZOMBIE_RESTARTED", tenant.String(), map[string]string{"job": job.String()})
			return nil
		}
		for _, w := range ws {
			if !ended(w.Status) {
				return nil // a run is still going
			}
		}
		return e.repairEnded(ctx, tenant, job)
	}, stale.Seconds())
}

func ended(s dbos.WorkflowStatusType) bool {
	return s == dbos.WorkflowStatusSuccess || s == dbos.WorkflowStatusError || s == dbos.WorkflowStatusCancelled ||
		s == dbos.WorkflowStatusMaxRecoveryAttemptsExceeded
}

// repairEnded marks FAILED the transfers left by a job whose workflows all
// ended, and decides the job.
func (e *Engine) repairEnded(ctx context.Context, tenant, job uuid.UUID) error {
	msg, _ := json.Marshal(map[string]string{"class": classInternal, "message": "the workflow ended without marking the transfer"})
	if err := store.InTenant(ctx, e.worker, tenant, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `update veho.transfers set state = 'FAILED', last_error = $2, finished_at = now(), fence = fence + 1,
			lease_owner = null, lease_expires_at = null where job_id = $1 and state <> all($3)`, job, msg, terminal); err != nil {
			return err
		}
		return e.decide(ctx, tx, job)
	}); err != nil {
		return err
	}
	e.system(ctx, "SYSTEM_ZOMBIE_REPAIRED", tenant.String(), map[string]string{"job": job.String(), "reason": "workflow ended without marking"})
	return nil
}

// leases counts the transfers whose lease expired: a live executor renews
// its leases with its heartbeat, so such a lease belongs to a fenced or
// stuck node; adopt covers the dead ones. Nothing is released: a claim
// moves the fence when the owner changes (§4.2).
func (e *Engine) leases(ctx context.Context) error {
	var n int
	if err := e.worker.QueryRow(ctx, "select count(*) from veho.expired_transfers($1)", batch).Scan(&n); err != nil {
		return err
	}
	e.metrics.expiredLeases.Set(float64(n))
	return nil
}

// retain deletes the finished workflows, the settled jobs and the rows of
// executors gone, past their retention (veho-audit-housekeeping.md §3.2).
func (e *Engine) retain(ctx context.Context) error {
	counts := map[string]int{}
	if r := *e.cfg.Housekeeping.WorkflowRetention; r > 0 {
		ws, err := dbos.ListWorkflows(e.dbos, dbos.WithFilterStatus(dbos.WorkflowStatusSuccess, dbos.WorkflowStatusError, dbos.WorkflowStatusCancelled),
			dbos.WithFilterCompletedBefore(time.Now().Add(-r)), dbos.WithFilterLimit(batch), dbos.WithFilterLoadInput(false), dbos.WithFilterLoadOutput(false))
		if err != nil {
			return err
		}
		ids := make([]string, len(ws))
		for i, w := range ws {
			ids[i] = w.ID
		}
		if len(ids) > 0 {
			if err := dbos.DeleteWorkflows(e.dbos, ids); err != nil {
				return err
			}
		}
		counts["workflows"] = len(ids)
	}
	if r := *e.cfg.Housekeeping.JobsRetention; r > 0 {
		if err := e.each(ctx, "select tenant_id, job_id from veho.settled_jobs_before(now() - make_interval(secs => $2), $1)", func(tenant, job uuid.UUID) error {
			counts["jobs"]++
			return store.InTenant(ctx, e.worker, tenant, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, "delete from veho.jobs where id = $1", job)
				return err
			})
		}, r.Seconds()); err != nil {
			return err
		}
	}
	tag, err := e.worker.Exec(ctx, "delete from veho.executors where last_seen < now() - make_interval(secs => $1)", executorRetention.Seconds())
	if err != nil {
		return err
	}
	counts["executors"] = int(tag.RowsAffected())
	if counts["workflows"]+counts["jobs"]+counts["executors"] > 0 {
		e.system(ctx, "SYSTEM_RETENTION_PURGED", "", counts)
	}
	return nil
}

// each runs fn on the (tenant, job) rows of query, whose first argument is
// the batch size; the errors of fn are joined, so one row does not stop
// the others.
func (e *Engine) each(ctx context.Context, query string, fn func(tenant, job uuid.UUID) error, args ...any) error {
	rows, err := e.worker.Query(ctx, query, append([]any{batch}, args...)...)
	if err != nil {
		return err
	}
	type pair struct{ Tenant, Job uuid.UUID }
	pairs, err := pgx.CollectRows(rows, pgx.RowToStructByPos[pair])
	if err != nil {
		return err
	}
	var errs []error
	for _, p := range pairs {
		if err := fn(p.Tenant, p.Job); err != nil {
			errs = append(errs, fmt.Errorf("job %s: %w", p.Job, err))
		}
	}
	return errors.Join(errs...)
}

// ownQuiet is how long a PENDING workflow of this node must be unchanged, and
// not executing here, before recoverOwn resumes it: a workflow just dequeued
// has not registered yet.
const ownQuiet = time.Minute

// running registers the workflow of ctx as executing in this process until
// the returned function is called.
func (e *Engine) running(ctx dbos.Context) func() {
	id, err := dbos.GetWorkflowID(ctx)
	if err != nil {
		return func() {}
	}
	e.active.Store(id, struct{}{})
	return func() { e.active.Delete(id) }
}

// recoverOwn resumes this node's PENDING workflows that it no longer
// executes: an execution ends without its outcome recorded when the state
// store is unreachable (a failover), and the row then stays PENDING under a
// live executor, which adoption does not touch and DBOS recovers only at
// start (found by AC-5). It runs on every node, without the housekeeping
// lock, and not while fenced.
func (e *Engine) recoverOwn(ctx context.Context) (int, error) {
	if e.Fenced() {
		return 0, nil
	}
	pending, err := dbos.ListWorkflows(e.dbos, dbos.WithFilterExecutorIDs(e.executor), dbos.WithFilterStatus(dbos.WorkflowStatusPending),
		dbos.WithFilterLoadInput(false), dbos.WithFilterLoadOutput(false))
	if err != nil {
		return 0, err
	}
	var resume []string
	for _, w := range pending {
		if _, live := e.active.Load(w.ID); !live && time.Since(w.UpdatedAt) > ownQuiet {
			resume = append(resume, w.ID)
		}
	}
	if len(resume) == 0 {
		return 0, nil
	}
	if _, err := dbos.ResumeWorkflows[string](e.dbos, resume); err != nil {
		return 0, err
	}
	e.metrics.adoptions.Add(float64(len(resume)))
	e.system(ctx, "SYSTEM_ZOMBIE_ADOPTED", "", map[string]any{"executors": []string{e.executor}, "workflows": resume, "reason": "not executing here"})
	slog.InfoContext(ctx, "resumed this node's own workflows that no execution was running", "workflows", len(resume))
	return len(resume), nil
}

// rewrap re-wraps a batch of the DEKs left under an old KEK after a rotation
// and retires the versions no secret uses any more (backlog 24): what
// veho kek rewrap does, in the background.
func (e *Engine) rewrap(ctx context.Context) error {
	if e.secrets == nil {
		return nil
	}
	n, retired, err := e.secrets.Rewrap(ctx, batch)
	if err != nil {
		return err
	}
	if n > 0 || len(retired) > 0 {
		e.system(ctx, "KEK_REWRAP", "", map[string]any{"rewrapped": n, "retired": retired})
		slog.InfoContext(ctx, "re-wrapped data keys after a KEK rotation", "rewrapped", n, "retired", retired)
	}
	return nil
}
