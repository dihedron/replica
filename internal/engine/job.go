package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"
	"uuid"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/dihedron/veho/internal/audit"
	"github.com/dihedron/veho/internal/store"
	"github.com/dihedron/veho/pkg/plugin"
	"github.com/jackc/pgx/v5"
)

// resolve is ReplicateJob's first step (§4.5): Stat of the source, the part
// size of the job, its source info recorded; the job is RUNNING. A missing
// source is Permanent.
func (e *Engine) resolve(ctx context.Context, in JobInput) result {
	var (
		srcEP   uuid.UUID
		path    string
		state   string
		drivers []string
	)
	if err := store.InTenant(ctx, e.worker, in.Tenant, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, "select source_endpoint_id, source_path, state from veho.jobs where id = $1", in.Job).Scan(&srcEP, &path, &state); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `select distinct d.driver from veho.transfers t join veho.endpoints d on d.id = t.destination_endpoint_id
			where t.job_id = $1`, in.Job)
		if err != nil {
			return err
		}
		drivers, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	}); err != nil {
		return fail(err)
	}
	if state != "ACCEPTED" && state != "RUNNING" {
		return result{Done: true}
	}
	_, src, err := e.open(ctx, in.Tenant, srcEP)
	if err != nil {
		return fail(err)
	}
	info, err := src.Stat(ctx, plugin.Ref{Key: path})
	_ = src.Close()
	if err != nil {
		if errors.Is(err, plugin.ErrNotFound) {
			return fail(plugin.Wrapf(plugin.Permanent, "engine: the source object %q does not exist", path))
		}
		return fail(drv(fmt.Errorf("stat the source: %w", err)))
	}
	caps := make([]plugin.Capabilities, 0, len(drivers))
	for _, name := range drivers {
		d, err := e.driver(name)
		if err != nil {
			return fail(err)
		}
		caps = append(caps, d.Capabilities())
	}
	ps, err := partSize(info.Size, int64(e.cfg.Engine.PartSizeMiB)<<20, caps)
	if err != nil {
		return fail(err)
	}
	si, err := json.Marshal(sourceInfo{Size: info.Size, Version: info.Version, PartSize: ps})
	if err != nil {
		return fail(err)
	}
	return check(store.InTenant(ctx, e.worker, in.Tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `update veho.jobs set state = 'RUNNING', updated_at = now(), source_info = $2
			where id = $1 and state in ('ACCEPTED', 'RUNNING')`, in.Job, si)
		return err
	}))
}

// runSuffix is the suffix of the workflow IDs of run n.
func runSuffix(n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("/retry-%d", n)
}

// jobWorkflow is ReplicateJob (§4.5): resolve the source, start the
// transfers that are not terminal, wait for them, finalize. The job's state
// is set by the transfers as they end (§4.3).
func (e *Engine) jobWorkflow(ctx dbos.Context, in JobInput) (string, error) {
	unknown := 0
	for n := 0; ; n++ {
		r, err := runStep(ctx, "resolve", func(c context.Context) result { return e.resolve(c, in) })
		if err != nil {
			return "", err
		}
		if r.Done {
			return "done", nil
		}
		if !r.failed() {
			break
		}
		if r.Class == plugin.Unknown {
			unknown++
		}
		if r.Class != plugin.Transient && (r.Class != plugin.Unknown || unknown >= maxUnknown) {
			_, err := runStep(ctx, "fail", func(c context.Context) result { return e.failJob(c, in, r) })
			return "failed", err
		}
		if _, err := dbos.Sleep(ctx, backoff(in.Job.String(), n, 2*time.Second, 15*time.Minute)); err != nil {
			return "", err
		}
	}
	ids, err := runStep(ctx, "transfers", func(c context.Context) idsResult { return e.openTransfers(c, in) })
	if err != nil {
		return "", err
	}
	var handles []dbos.WorkflowHandle[string]
	for _, t := range ids.IDs {
		h, err := dbos.RunWorkflow(ctx, e.transferWorkflow, TransferInput{Tenant: in.Tenant, Job: in.Job, Transfer: t},
			dbos.WithWorkflowID(TransferWorkflowID(t)+runSuffix(in.Run)),
			dbos.WithQueue(e.queues[QueueTransfer]), dbos.WithQueuePartitionKey(in.Tenant.String()))
		if err != nil {
			return "", fmt.Errorf("engine: start transfer %s: %w", t, err)
		}
		handles = append(handles, h)
	}
	for _, h := range handles {
		// a transfer that ends in error or cancelled has its row set; the
		// job's state does not depend on the workflow's outcome
		if _, err := h.GetResult(); err != nil && ctx.Err() != nil {
			return "", err
		}
	}
	_, err = runStep(ctx, "finalize", func(c context.Context) result { return e.finalize(c, in) })
	return "done", err
}

type idsResult struct {
	result
	IDs []uuid.UUID `json:"ids"`
}

// openTransfers lists the job's transfers that are not terminal.
func (e *Engine) openTransfers(ctx context.Context, in JobInput) idsResult {
	var out idsResult
	err := store.InTenant(ctx, e.worker, in.Tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, "select id from veho.transfers where job_id = $1 and state <> all($2) order by id", in.Job, terminal)
		if err != nil {
			return err
		}
		out.IDs, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		return err
	})
	if err != nil {
		out.result = fail(err)
	}
	return out
}

// failJob ends a job whose source cannot be read: FAILED, every transfer
// CANCELLED.
func (e *Engine) failJob(ctx context.Context, in JobInput, r result) result {
	errJSON, _ := json.Marshal(map[string]string{"class": r.Class.String(), "message": r.Err})
	err := store.InTenant(ctx, e.worker, in.Tenant, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `update veho.jobs set state = 'FAILED', last_error = $2, updated_at = now(), finished_at = now(), settled_at = now()
			where id = $1 and state in ('ACCEPTED', 'RUNNING')`, in.Job, errJSON); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `update veho.transfers set state = 'CANCELLED', finished_at = now(), fence = fence + 1, lease_owner = null, lease_expires_at = null
			where job_id = $1 and state <> all($2)`, in.Job, terminal)
		return err
	})
	if err != nil {
		return fail(err)
	}
	e.settled(ctx, in)
	return result{Done: true}
}

// finalize records the job's settlement in the audit log.
func (e *Engine) finalize(ctx context.Context, in JobInput) result {
	e.settled(ctx, in)
	return result{}
}

// settled audits the job's outcome.
func (e *Engine) settled(ctx context.Context, in JobInput) {
	var state string
	var copies int
	if err := store.InTenant(ctx, e.worker, in.Tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "select state, copies_done from veho.jobs where id = $1", in.Job).Scan(&state, &copies)
	}); err != nil {
		slog.WarnContext(ctx, "cannot read the settled job", "tenant_id", in.Tenant.String(), "job_id", in.Job.String(), "error", err)
		return
	}
	e.audit.Emit(ctx, audit.Event{Action: "JOB_SETTLED", Outcome: audit.Success, TenantID: in.Tenant.String(), ResourceType: "job",
		ResourceID: in.Job.String(), JobID: in.Job.String(), WorkflowID: JobWorkflowID(in.Job) + runSuffix(in.Run),
		Details: map[string]any{"state": state, "copies": copies}})
}
