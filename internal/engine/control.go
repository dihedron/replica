package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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
	ID             uuid.UUID
	State          string
	SourceEndpoint string
	SourcePath     string
	SourceSize     *int64
	SourceVersion  string
	Labels         map[string]string
	Copies         int
	Destinations   int
	CreatedAt      time.Time
	UpdatedAt      time.Time
	DeadlineAt     time.Time
	SatisfiedAt    *time.Time
	CompletedAt    *time.Time
	SettledAt      *time.Time
	LastError      json.RawMessage
	Transfers      []TransferStatus
}

// TransferStatus is one destination's progress.
type TransferStatus struct {
	ID         uuid.UUID
	Endpoint   string
	Path       string
	Position   map[string]string
	State      string
	BytesDone  int64
	BytesTotal *int64
	Attempt    int
	LastError  json.RawMessage
	FinishedAt *time.Time
}

const jobColumns = `j.id, j.state, e.name, j.source_path, (j.source_info->>'size')::bigint, coalesce(j.source_info->>'version', ''),
	j.labels, j.copies_done, (select count(*)::int from veho.transfers t where t.job_id = j.id), j.created_at, j.updated_at, j.deadline_at, j.satisfied_at, j.completed_at, j.settled_at, j.last_error`

func scanJob(row pgx.Row, s *JobStatus) error {
	return row.Scan(&s.ID, &s.State, &s.SourceEndpoint, &s.SourcePath, &s.SourceSize, &s.SourceVersion, &s.Labels, &s.Copies, &s.Destinations,
		&s.CreatedAt, &s.UpdatedAt, &s.DeadlineAt, &s.SatisfiedAt, &s.CompletedAt, &s.SettledAt, &s.LastError)
}

// Status returns a job and its transfers.
func (e *Engine) Status(ctx context.Context, tenant, job uuid.UUID) (JobStatus, error) {
	var s JobStatus
	err := store.InTenant(ctx, e.api, tenant, func(tx pgx.Tx) error {
		err := scanJob(tx.QueryRow(ctx, "select "+jobColumns+" from veho.jobs j join veho.endpoints e on e.id = j.source_endpoint_id where j.id = $1", job), &s)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `select t.id, e.name, t.dest_path, t.position, t.state, t.bytes_done, t.bytes_total, t.attempt, t.last_error, t.finished_at
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

// Filter selects jobs for ListJobs (FR-16).
type Filter struct {
	State          string
	Since          time.Time
	SourceEndpoint string
	Labels         map[string]string
	// Limit is at most 500; 0 is 100.
	Limit int
	// Cursor is the NextCursor of the previous page.
	Cursor string
}

// ListJobs returns a page of the tenant's jobs, newest first, without their
// transfers, and the cursor of the next page ("" at the end).
func (e *Engine) ListJobs(ctx context.Context, tenant uuid.UUID, f Filter) ([]JobStatus, string, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	limit = min(limit, 500)
	q := "select " + jobColumns + " from veho.jobs j join veho.endpoints e on e.id = j.source_endpoint_id where true"
	var args []any
	arg := func(v any) string { args = append(args, v); return fmt.Sprintf("$%d", len(args)) }
	if f.State != "" {
		q += " and j.state = " + arg(f.State)
	}
	if !f.Since.IsZero() {
		q += " and j.created_at >= " + arg(f.Since)
	}
	if f.SourceEndpoint != "" {
		q += " and e.name = " + arg(f.SourceEndpoint)
	}
	if len(f.Labels) > 0 {
		l, _ := json.Marshal(f.Labels)
		q += " and j.labels @> " + arg(l) + "::jsonb"
	}
	if f.Cursor != "" {
		at, id, err := parseCursor(f.Cursor)
		if err != nil {
			return nil, "", fmt.Errorf("engine: %w: cursor", ErrInvalid)
		}
		q += " and (j.created_at, j.id) < (" + arg(at) + ", " + arg(id) + ")"
	}
	q += " order by j.created_at desc, j.id desc limit " + arg(limit+1)
	var out []JobStatus
	err := store.InTenant(ctx, e.api, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var s JobStatus
			if err := scanJob(rows, &s); err != nil {
				return err
			}
			out = append(out, s)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, "", fmt.Errorf("engine: list: %w", err)
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		last := out[limit-1]
		next = last.CreatedAt.UTC().Format(time.RFC3339Nano) + "_" + last.ID.String()
	}
	return out, next, nil
}

func parseCursor(c string) (time.Time, uuid.UUID, error) {
	at, id, ok := strings.Cut(c, "_")
	if !ok {
		return time.Time{}, uuid.UUID{}, errors.New("malformed")
	}
	t, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return time.Time{}, uuid.UUID{}, err
	}
	u, err := uuid.Parse(id)
	return t, u, err
}
