package engine

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
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
	// Labels are the client's own (veho-api-server.md §5).
	Labels map[string]string
	// PolicyRef names a stored policy instead of Policy (§5.1).
	PolicyRef *PolicyRef
}

// PolicyRef names a version of a named policy; Version 0 is the latest.
type PolicyRef struct {
	Name    string `json:"name"`
	Version int    `json:"version,omitempty"`
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
	// the backoff's bounds (FR-12); a tenant's own bounds must keep the
	// policy defaults (2s, 15m) valid
	defaultMinRetryInitial  = time.Second
	defaultMaxRetryInterval = time.Hour
	policyRetryInitial      = 2 * time.Second
	policyRetryMaxInterval  = 15 * time.Minute
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
	// ErrIdempotencyReuse: the key was used for another request (409).
	ErrIdempotencyReuse = errors.New("idempotency key used for another request")
	// ErrInvalid: a request the engine refuses before the policy (400).
	ErrInvalid = errors.New("invalid request")
)

// Label limits (veho-api-server.md §5).
const (
	maxLabels     = 16
	maxLabelKey   = 63
	maxLabelValue = 255
)

type quotas struct {
	MaxDestinations  int             `json:"max_destinations"`
	MaxActiveJobs    int             `json:"max_active_jobs"`
	MinRetryInitial  policy.Duration `json:"min_retry_initial"`
	MaxRetryInterval policy.Duration `json:"max_retry_interval"`
}

// env is the policy environment of the tenant's quotas.
func (q quotas) env(topo *topology.Topology, resolve func(string) (topology.Position, bool)) policy.Env {
	return policy.Env{Topology: topo, Resolve: resolve,
		MaxDestinations:  min(cmpOr(q.MaxDestinations, defaultMaxDestinations), platformMaxDestinations),
		MinRetryInitial:  cmp.Or(time.Duration(q.MinRetryInitial), defaultMinRetryInitial),
		MaxRetryInterval: cmp.Or(time.Duration(q.MaxRetryInterval), defaultMaxRetryInterval)}
}

// JobWorkflowID is the workflow ID of a job's first ReplicateJob.
func JobWorkflowID(job uuid.UUID) string { return "job/" + job.String() }

// Submit accepts a job: the job, its transfers and its workflow are committed
// together or not at all (INV-1). It returns the job and whether it is new.
// A policy the tenant may not submit fails with a *policy.Error.
func (e *Engine) Submit(ctx context.Context, tenant uuid.UUID, r Request) (uuid.UUID, bool, error) {
	var p policy.Policy
	if (r.PolicyRef == nil) == (len(r.Policy) == 0) {
		return uuid.UUID{}, false, fmt.Errorf("%w: give a policy or a policy reference", ErrInvalid)
	}
	if r.PolicyRef == nil {
		var err error
		if p, err = policy.Parse(r.Policy); err != nil {
			return uuid.UUID{}, false, &policy.Error{Code: policy.CodeInvalidPolicy, Message: err.Error()}
		}
	}
	if err := checkLabels(r.Labels); err != nil {
		return uuid.UUID{}, false, err
	}
	sum, err := requestHash(r, p)
	if err != nil {
		return uuid.UUID{}, false, err
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
			if found, err := sameRequest(ctx, tx, r.IdempotencyKey, sum, &job); found || err != nil {
				return err
			}
		}
		var ref *uuid.UUID
		if r.PolicyRef != nil {
			pv, err := loadPolicy(ctx, tx, r.PolicyRef.Name, r.PolicyRef.Version)
			if err != nil {
				return err
			}
			if p, err = policy.Parse(pv.Spec); err != nil {
				return &policy.Error{Code: policy.CodeInvalidPolicy, Message: err.Error()}
			}
			ref = &pv.ID
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
		env := q.env(topo, func(name string) (topology.Position, bool) {
			ep, ok := eps.get(name)
			return ep.position, ok
		})
		env.Source = policy.Ref{Endpoint: r.SourceEndpoint, Path: r.SourcePath}
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
		labels, err := json.Marshal(cmpLabels(r.Labels))
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `insert into veho.jobs (id, tenant_id, idempotency_key, source_endpoint_id, source_path, policy, deadline_at, labels, request_sha256, policy_ref)
			values ($1, veho.current_tenant(), $2, $3, $4, $5, now() + $6 * interval '1 second', $7, $8, $9)
			on conflict (tenant_id, idempotency_key) do nothing`,
			job, key, src.id, r.SourcePath, spec, time.Duration(p.Deadline).Seconds(), labels, sum, ref)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 { // a concurrent submission of the same key won
			_, err := sameRequest(ctx, tx, r.IdempotencyKey, sum, &job)
			return err
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
		if !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrQuota) && !errors.Is(err, ErrSuspended) && !errors.Is(err, ErrIdempotencyReuse) {
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

// sameRequest finds the job of an idempotency key: found, it sets *job, or
// fails with ErrIdempotencyReuse when the key served another request.
func sameRequest(ctx context.Context, tx pgx.Tx, key string, sum []byte, job *uuid.UUID) (bool, error) {
	var prev []byte
	err := tx.QueryRow(ctx, "select id, request_sha256 from veho.jobs where idempotency_key = $1", key).Scan(job, &prev)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	case err != nil:
		return false, err
	case !bytes.Equal(prev, sum):
		return true, ErrIdempotencyReuse
	}
	return true, nil
}

// requestHash is the SHA-256 of the request's canonical form: the source,
// the normalised inline policy or the policy reference, and the labels, encoded by encoding/json, which
// sorts map keys.
func requestHash(r Request, p policy.Policy) ([]byte, error) {
	b, err := json.Marshal(struct {
		Endpoint, Path string
		Policy         policy.Policy
		Ref            *PolicyRef
		Labels         map[string]string
	}{r.SourceEndpoint, r.SourcePath, p, r.PolicyRef, cmpLabels(r.Labels)})
	if err != nil {
		return nil, err
	}
	s := sha256.Sum256(b)
	return s[:], nil
}

func cmpLabels(l map[string]string) map[string]string {
	if l == nil {
		return map[string]string{}
	}
	return l
}

func checkLabels(l map[string]string) error {
	if len(l) > maxLabels {
		return fmt.Errorf("%w: at most %d labels", ErrInvalid, maxLabels)
	}
	for k, v := range l {
		if k == "" || len(k) > maxLabelKey || len(v) > maxLabelValue {
			return fmt.Errorf("%w: label %q: keys of 1 to %d characters, values of at most %d", ErrInvalid, k, maxLabelKey, maxLabelValue)
		}
	}
	return nil
}
