package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"uuid"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/dihedron/veho/internal/audit"
	"github.com/dihedron/veho/internal/store"
	"github.com/jackc/pgx/v5"
)

// ErrState refuses an action the job's state does not allow (409).
var ErrState = errors.New("not allowed in the job's state")

// Cancel requests that a job stop (§4.6, FR-15): the transfers not running
// end CANCELLED at once, the others at their next batch; the job is
// CANCELLED when all are. Cancelling a cancelled job does nothing.
func (e *Engine) Cancel(ctx context.Context, tenant, job uuid.UUID) error {
	err := store.InTenant(ctx, e.api, tenant, func(tx pgx.Tx) error {
		var state string
		if err := tx.QueryRow(ctx, "select state from veho.jobs where id = $1 for update", job).Scan(&state); errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		switch state {
		case "CANCELLED":
			return nil
		case "SUCCEEDED", "FAILED":
			return fmt.Errorf("%w: %s", ErrState, state)
		}
		if _, err := tx.Exec(ctx, "update veho.jobs set cancel_requested = true, updated_at = now() where id = $1", job); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `update veho.transfers set state = 'CANCELLED', finished_at = now(), fence = fence + 1, lease_owner = null, lease_expires_at = null
			where job_id = $1 and state in ('PENDING', 'RETRY_WAIT')`, job); err != nil {
			return err
		}
		return e.decide(ctx, tx, job)
	})
	e.emitJob(ctx, "JOB_CANCEL", tenant, job, err, nil)
	if err != nil {
		return fmt.Errorf("engine: cancel: %w", err)
	}
	return nil
}

// Retry restarts the failed and cancelled transfers of a FAILED job as a
// new run of ReplicateJob, in one transaction (§4.6, FR-15).
func (e *Engine) Retry(ctx context.Context, tenant, job uuid.UUID) error {
	run := 0
	err := store.InTenant(ctx, e.api, tenant, func(tx pgx.Tx) error {
		var state string
		if err := tx.QueryRow(ctx, "select state from veho.jobs where id = $1 for update", job).Scan(&state); errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if state != "FAILED" {
			return fmt.Errorf("%w: %s", ErrState, state)
		}
		if err := tx.QueryRow(ctx, "select count(*) from dbos.workflow_status where workflow_uuid like $1", JobWorkflowID(job)+"%").Scan(&run); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `update veho.transfers set state = 'PENDING', attempt = 0, adoptions = 0, bytes_done = 0, resume_token = null,
			last_error = null, started_at = null, finished_at = null, fence = fence + 1, lease_owner = null, lease_expires_at = null
			where job_id = $1 and state in ('FAILED', 'CANCELLED')`, job); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `update veho.jobs set state = 'RUNNING', cancel_requested = false, last_error = null, source_info = source_info - 'digest',
			satisfied_at = null, completed_at = null, settled_at = null, finished_at = null, updated_at = now(),
			deadline_at = now() + (deadline_at - created_at) where id = $1`, job); err != nil {
			return err
		}
		_, err := dbos.Enqueue[string](e.dbos, QueueJobs, WorkflowJob, JobInput{Tenant: tenant, Job: job, Run: run},
			dbos.WithEnqueueWorkflowID(JobWorkflowID(job)+runSuffix(run)),
			dbos.WithEnqueueQueuePartitionKey(tenant.String()),
			dbos.WithEnqueueTransaction(tx))
		return err
	})
	e.emitJob(ctx, "JOB_RETRY", tenant, job, err, map[string]int{"run": run})
	if err != nil {
		return fmt.Errorf("engine: retry: %w", err)
	}
	return nil
}

func (e *Engine) emitJob(ctx context.Context, action string, tenant, job uuid.UUID, err error, details any) {
	ev := audit.Event{Action: action, Outcome: audit.Success, TenantID: tenant.String(), ResourceType: "job", ResourceID: job.String(), JobID: job.String(), Details: details}
	if err != nil {
		ev.Outcome, ev.Details = audit.Failed, map[string]string{"error": err.Error()}
	}
	e.audit.Emit(ctx, ev)
}

// JobStatus is what Status shows (FR-14).
type JobStatus struct {
	ID          uuid.UUID
	State       string
	Copies      int
	SatisfiedAt *time.Time
	CompletedAt *time.Time
	SettledAt   *time.Time
	DeadlineAt  time.Time
	LastError   json.RawMessage
	Transfers   []TransferStatus
}

// TransferStatus is one destination's progress.
type TransferStatus struct {
	ID         uuid.UUID
	Endpoint   string
	Path       string
	State      string
	BytesDone  int64
	BytesTotal *int64
	Attempt    int
	LastError  json.RawMessage
}

// Status returns a job and its transfers.
func (e *Engine) Status(ctx context.Context, tenant, job uuid.UUID) (JobStatus, error) {
	s := JobStatus{ID: job}
	err := store.InTenant(ctx, e.api, tenant, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `select state, copies_done, satisfied_at, completed_at, settled_at, deadline_at, last_error
			from veho.jobs where id = $1`, job).Scan(&s.State, &s.Copies, &s.SatisfiedAt, &s.CompletedAt, &s.SettledAt, &s.DeadlineAt, &s.LastError); errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `select t.id, e.name, t.dest_path, t.state, t.bytes_done, t.bytes_total, t.attempt, t.last_error
			from veho.transfers t join veho.endpoints e on e.id = t.destination_endpoint_id where t.job_id = $1 order by e.name`, job)
		if err != nil {
			return err
		}
		s.Transfers, err = pgx.CollectRows(rows, pgx.RowToStructByPos[TransferStatus])
		return err
	})
	if err != nil {
		return JobStatus{}, fmt.Errorf("engine: status: %w", err)
	}
	return s, nil
}
