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
	"github.com/dihedron/veho/internal/policy"
	"github.com/dihedron/veho/internal/store"
	"github.com/dihedron/veho/internal/topology"
	"github.com/jackc/pgx/v5"
)

// Request is a job to accept (§3).
type Request struct {
	// IdempotencyKey makes a repeated submission return the first job (FR-2);
	// empty, every submission is a new job.
	IdempotencyKey string
	// SourceEndpoint and SourcePath name the source object (INV-6).
	SourceEndpoint string
	SourcePath     string
	// Policy is the policy document (veho-distribution-policy.md).
	Policy []byte
}

// JobInput is the input of ReplicateJob: IDs only (SEC-7, INV-5).
type JobInput struct {
	Tenant uuid.UUID `json:"tenant"`
	Job    uuid.UUID `json:"job"`
	// Run numbers the runs of the job: 0, then one per Retry (§4.6).
	Run int `json:"run,omitempty"`
}

// Quota defaults and the platform's ceiling (veho-distribution-policy.md §4).
const (
	defaultMaxDestinations  = 16
	platformMaxDestinations = 32
	defaultMaxActiveJobs    = 1000
)

// Errors of Submit, for the API's status codes.
var (
	// ErrNotFound: the source endpoint, or the tenant, does not exist for
	// the caller (INV-4).
	ErrNotFound = errors.New("not found")
	// ErrQuota: the tenant has max_active_jobs jobs running (429).
	ErrQuota = errors.New("quota exceeded")
	// ErrSuspended: the tenant is suspended.
	ErrSuspended = errors.New("tenant suspended")
)

type quotas struct {
	MaxDestinations int `json:"max_destinations"`
	MaxActiveJobs   int `json:"max_active_jobs"`
}

// JobWorkflowID is the workflow ID of a job's first ReplicateJob.
func JobWorkflowID(job uuid.UUID) string { return "job/" + job.String() }

// Submit accepts a job: the job, its transfers and its workflow are committed
// together or not at all (INV-1). It returns the job and whether it is new.
// A policy the tenant may not submit fails with a *policy.Error.
func (e *Engine) Submit(ctx context.Context, tenant uuid.UUID, r Request) (uuid.UUID, bool, error) {
	p, err := policy.Parse(r.Policy)
	if err != nil {
		return uuid.UUID{}, false, &policy.Error{Code: policy.CodeInvalidPolicy, Message: err.Error()}
	}
	var (
		job     uuid.UUID
		created bool
	)
	err = store.InTenant(ctx, e.api, tenant, func(tx pgx.Tx) error {
		var state string
		var q quotas
		if err := tx.QueryRow(ctx, "select state, quotas from veho.tenants where id = veho.current_tenant()").Scan(&state, &q); errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if state != "active" {
			return ErrSuspended
		}
		if r.IdempotencyKey != "" {
			if err := tx.QueryRow(ctx, "select id from veho.jobs where idempotency_key = $1", r.IdempotencyKey).Scan(&job); err == nil {
				return nil
			} else if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		topo, err := LoadTopology(ctx, tx)
		if err != nil {
			return err
		}
		eps := endpoints{tx: tx, ctx: ctx, topo: topo}
		src, ok := eps.get(r.SourceEndpoint)
		if !ok {
			if eps.err != nil {
				return eps.err
			}
			return fmt.Errorf("source endpoint %q: %w", r.SourceEndpoint, ErrNotFound)
		}
		env := policy.Env{
			Topology: topo,
			Resolve: func(name string) (topology.Position, bool) {
				ep, ok := eps.get(name)
				return ep.position, ok
			},
			Source:          policy.Ref{Endpoint: r.SourceEndpoint, Path: r.SourcePath},
			MaxDestinations: min(cmpOr(q.MaxDestinations, defaultMaxDestinations), platformMaxDestinations),
		}
		if err := p.Validate(env); err != nil {
			if eps.err != nil {
				return eps.err
			}
			return err
		}
		// shortcut: the count is not serialised with concurrent submissions,
		// so a burst can pass the quota by a few jobs; lock per tenant if
		// that ever matters
		var active int
		if err := tx.QueryRow(ctx, "select count(*) from veho.jobs where state in ('ACCEPTED', 'RUNNING')").Scan(&active); err != nil {
			return err
		}
		if active >= cmpOr(q.MaxActiveJobs, defaultMaxActiveJobs) {
			return ErrQuota
		}
		spec, err := json.Marshal(p)
		if err != nil {
			return err
		}
		job = uuid.NewV7()
		var key *string
		if r.IdempotencyKey != "" {
			key = &r.IdempotencyKey
		}
		tag, err := tx.Exec(ctx, `insert into veho.jobs (id, tenant_id, idempotency_key, source_endpoint_id, source_path, policy, deadline_at)
			values ($1, veho.current_tenant(), $2, $3, $4, $5, now() + $6 * interval '1 second')
			on conflict (tenant_id, idempotency_key) do nothing`,
			job, key, src.id, r.SourcePath, spec, time.Duration(p.Deadline).Seconds())
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 { // a concurrent submission of the same key won
			return tx.QueryRow(ctx, "select id from veho.jobs where idempotency_key = $1", r.IdempotencyKey).Scan(&job)
		}
		for _, d := range p.Destinations {
			ep, _ := eps.get(d.Endpoint)
			pos, err := json.Marshal(ep.position)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `insert into veho.transfers (id, tenant_id, job_id, destination_endpoint_id, position, dest_path)
				values ($1, veho.current_tenant(), $2, $3, $4, $5)`, uuid.NewV7(), job, ep.id, pos, d.Path); err != nil {
				return err
			}
		}
		if e.beforeCommit != nil {
			if err := e.beforeCommit(); err != nil {
				return err
			}
		}
		if _, err := dbos.Enqueue[string](e.dbos, QueueJobs, WorkflowJob, JobInput{Tenant: tenant, Job: job},
			dbos.WithEnqueueWorkflowID(JobWorkflowID(job)),
			dbos.WithEnqueueQueuePartitionKey(tenant.String()),
			dbos.WithEnqueueTransaction(tx)); err != nil {
			return fmt.Errorf("enqueue: %w", err)
		}
		created = true
		return nil
	})
	if err != nil {
		if perr, ok := errors.AsType[*policy.Error](err); ok {
			return uuid.UUID{}, false, perr
		}
		if !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrQuota) && !errors.Is(err, ErrSuspended) {
			slog.ErrorContext(ctx, "cannot accept the job", "tenant_id", tenant.String(), "error", err)
		}
		e.audit.Emit(ctx, audit.Event{Action: "JOB_SUBMIT", Outcome: audit.Failed, TenantID: tenant.String(), ResourceType: "job",
			Details: map[string]string{"error": err.Error()}})
		return uuid.UUID{}, false, fmt.Errorf("engine: submit: %w", err)
	}
	if created {
		e.audit.Emit(ctx, audit.Event{Action: "JOB_SUBMIT", Outcome: audit.Success, TenantID: tenant.String(), ResourceType: "job",
			ResourceID: job.String(), JobID: job.String(), WorkflowID: JobWorkflowID(job),
			Details: map[string]any{"source_endpoint": r.SourceEndpoint, "destinations": len(p.Destinations)}})
	}
	return job, created, nil
}

func cmpOr(v, def int) int {
	if v > 0 {
		return v
	}
	return def
}

// endpoint is an endpoint of the tenant, with its position.
type endpoint struct {
	id       uuid.UUID
	position topology.Position
}

// endpoints resolves the tenant's endpoints by name, once each; a database
// error is kept in err and reported by the caller.
type endpoints struct {
	ctx   context.Context
	tx    pgx.Tx
	topo  *topology.Topology
	cache map[string]*endpoint
	err   error
}

func (s *endpoints) get(name string) (endpoint, bool) {
	if s.cache == nil {
		s.cache = map[string]*endpoint{}
	}
	if ep, ok := s.cache[name]; ok {
		return derefOr(ep), ep != nil
	}
	var (
		id  uuid.UUID
		loc string
	)
	err := s.tx.QueryRow(s.ctx, "select id, location_id from veho.endpoints where name = $1", name).Scan(&id, &loc)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) && s.err == nil {
			s.err = err
		}
		s.cache[name] = nil
		return endpoint{}, false
	}
	pos, err := s.topo.Position(loc)
	if err != nil {
		s.err = err
		s.cache[name] = nil
		return endpoint{}, false
	}
	s.cache[name] = &endpoint{id: id, position: pos}
	return *s.cache[name], true
}

func derefOr(ep *endpoint) endpoint {
	if ep == nil {
		return endpoint{}
	}
	return *ep
}
