package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"
	"uuid"

	"github.com/dihedron/veho/internal/audit"
	"github.com/dihedron/veho/internal/policy"
	"github.com/dihedron/veho/internal/store"
	"github.com/dihedron/veho/internal/topology"
	"github.com/jackc/pgx/v5"
)

// Named policies and their grants (veho-api-server.md §5.1, SEC-21).

// ErrInUse refuses to delete a policy a job that is not terminal uses.
var ErrInUse = errors.New("used by a job in progress")

var validPolicyName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// The permissions of a grant; submit_inline has no policy.
const (
	PermSubmit       = "submit"
	PermRead         = "read"
	PermManage       = "manage"
	PermSubmitInline = "submit_inline"
)

// The tenant's inline_policies setting.
const (
	InlineAll     = "all"
	InlineGranted = "granted"
	InlineNone    = "none"
)

// PolicyVersion is a stored version of a named policy.
type PolicyVersion struct {
	ID        uuid.UUID
	Name      string
	Version   int
	Spec      json.RawMessage
	CreatedAt time.Time
}

// Grant gives a grantee a permission on a policy.
type Grant struct {
	Grantee    string
	Permission string
}

// loadPolicy reads a version of a name, the latest for version 0.
func loadPolicy(ctx context.Context, tx pgx.Tx, name string, version int) (PolicyVersion, error) {
	rows, err := tx.Query(ctx, `select id, name, version, spec, created_at from veho.policies
		where name = $1 and deleted_at is null and ($2 = 0 or version = $2) order by version desc limit 1`, name, version)
	if err != nil {
		return PolicyVersion{}, err
	}
	pv, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByPos[PolicyVersion])
	if errors.Is(err, pgx.ErrNoRows) {
		return PolicyVersion{}, fmt.Errorf("policy %q: %w", name, ErrNotFound)
	}
	return pv, err
}

// validate checks a policy against the tenant's endpoints and topology, as
// a submission would, with no source.
func validate(ctx context.Context, tx pgx.Tx, p policy.Policy) error {
	var q quotas
	if err := tx.QueryRow(ctx, "select quotas from veho.tenants where id = veho.current_tenant()").Scan(&q); err != nil {
		return err
	}
	topo, err := LoadTopology(ctx, tx)
	if err != nil {
		return err
	}
	eps := endpoints{tx: tx, ctx: ctx, topo: topo}
	env := q.env(topo, func(name string) (topology.Position, bool) {
		ep, ok := eps.get(name)
		return ep.position, ok
	})
	if err := p.Validate(env); err != nil {
		if eps.err != nil {
			return eps.err
		}
		return err
	}
	return nil
}

func parsePolicy(spec []byte) (policy.Policy, error) {
	p, err := policy.Parse(spec)
	if err != nil {
		return policy.Policy{}, &policy.Error{Code: policy.CodeInvalidPolicy, Message: err.Error()}
	}
	return p, nil
}

// ValidatePolicy checks a policy document and stores nothing.
func (e *Engine) ValidatePolicy(ctx context.Context, tenant uuid.UUID, spec []byte) error {
	p, err := parsePolicy(spec)
	if err != nil {
		return err
	}
	return store.InTenant(ctx, e.api, tenant, func(tx pgx.Tx) error { return validate(ctx, tx, p) })
}

// CreatePolicy stores version 1 of name, or its next version.
func (e *Engine) CreatePolicy(ctx context.Context, tenant uuid.UUID, name string, spec []byte) (PolicyVersion, error) {
	if !validPolicyName.MatchString(name) {
		return PolicyVersion{}, fmt.Errorf("%w: policy name %q: want %s", ErrInvalid, name, validPolicyName)
	}
	p, err := parsePolicy(spec)
	if err != nil {
		return PolicyVersion{}, err
	}
	var pv PolicyVersion
	err = store.InTenant(ctx, e.api, tenant, func(tx pgx.Tx) error {
		if err := validate(ctx, tx, p); err != nil {
			return err
		}
		norm, err := json.Marshal(p)
		if err != nil {
			return err
		}
		// one version at a time per name
		if _, err := tx.Exec(ctx, "select pg_advisory_xact_lock(hashtext('veho.policy/' || $1 || '/' || $2))", tenant.String(), name); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `insert into veho.policies (tenant_id, name, version, spec)
			values (veho.current_tenant(), $1, (select coalesce(max(version), 0) + 1 from veho.policies where name = $1), $2)
			returning id, name, version, spec, created_at`, name, norm)
		if err != nil {
			return err
		}
		pv, err = pgx.CollectExactlyOneRow(rows, pgx.RowToStructByPos[PolicyVersion])
		return err
	})
	e.emitPolicy(ctx, "POLICY_CREATE", tenant, name, err, map[string]int{"version": pv.Version})
	if err != nil {
		return PolicyVersion{}, fmt.Errorf("engine: create policy: %w", err)
	}
	return pv, nil
}

// GetPolicy returns a version of name, the latest for version 0.
func (e *Engine) GetPolicy(ctx context.Context, tenant uuid.UUID, name string, version int) (PolicyVersion, error) {
	var pv PolicyVersion
	err := store.InTenant(ctx, e.api, tenant, func(tx pgx.Tx) error {
		var err error
		pv, err = loadPolicy(ctx, tx, name, version)
		return err
	})
	if err != nil {
		return PolicyVersion{}, fmt.Errorf("engine: get policy: %w", err)
	}
	return pv, nil
}

// ListPolicies returns the latest version of each policy, by name.
func (e *Engine) ListPolicies(ctx context.Context, tenant uuid.UUID) ([]PolicyVersion, error) {
	var out []PolicyVersion
	err := store.InTenant(ctx, e.api, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `select distinct on (name) id, name, version, spec, created_at from veho.policies
			where deleted_at is null order by name, version desc`)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, pgx.RowToStructByPos[PolicyVersion])
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("engine: list policies: %w", err)
	}
	return out, nil
}

// DeletePolicy hides every version of name, unless a job that is not
// terminal uses one; the versions stay for the finished jobs.
func (e *Engine) DeletePolicy(ctx context.Context, tenant uuid.UUID, name string) error {
	err := store.InTenant(ctx, e.api, tenant, func(tx pgx.Tx) error {
		var used bool
		if err := tx.QueryRow(ctx, `select exists (select 1 from veho.jobs j join veho.policies p on p.id = j.policy_ref
			where p.name = $1 and j.state in ('ACCEPTED', 'RUNNING'))`, name).Scan(&used); err != nil {
			return err
		}
		if used {
			return ErrInUse
		}
		tag, err := tx.Exec(ctx, "update veho.policies set deleted_at = now() where name = $1 and deleted_at is null", name)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		_, err = tx.Exec(ctx, "delete from veho.policy_grants where policy = $1", name)
		return err
	})
	e.emitPolicy(ctx, "POLICY_DELETE", tenant, name, err, nil)
	if err != nil {
		return fmt.Errorf("engine: delete policy: %w", err)
	}
	return nil
}

// Grants returns the grants of a policy.
func (e *Engine) Grants(ctx context.Context, tenant uuid.UUID, name string) ([]Grant, error) {
	var out []Grant
	err := store.InTenant(ctx, e.api, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, "select grantee, permission from veho.policy_grants where policy = $1 order by grantee, permission", name)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, pgx.RowToStructByPos[Grant])
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("engine: grants: %w", err)
	}
	return out, nil
}

// SetGrants replaces the grants of a policy (submit, read, manage); with
// name "", the tenant-wide grants (submit_inline).
func (e *Engine) SetGrants(ctx context.Context, tenant uuid.UUID, name string, grants []Grant) error {
	err := store.InTenant(ctx, e.api, tenant, func(tx pgx.Tx) error {
		var pol *string
		if name != "" {
			if _, err := loadPolicy(ctx, tx, name, 0); err != nil {
				return err
			}
			pol = &name
		}
		for _, g := range grants {
			if (g.Permission == PermSubmitInline) != (name == "") || !validGrantee.MatchString(g.Grantee) {
				return fmt.Errorf("%w: grant %s to %q", ErrInvalid, g.Permission, g.Grantee)
			}
		}
		if _, err := tx.Exec(ctx, "delete from veho.policy_grants where policy is not distinct from $1", pol); err != nil {
			return err
		}
		for _, g := range grants {
			if _, err := tx.Exec(ctx, `insert into veho.policy_grants (tenant_id, policy, grantee, permission)
				values (veho.current_tenant(), $1, $2, $3) on conflict do nothing`, pol, g.Grantee, g.Permission); err != nil {
				return err
			}
		}
		return nil
	})
	e.emitPolicy(ctx, "POLICY_GRANTS", tenant, name, err, map[string]int{"grants": len(grants)})
	if err != nil {
		return fmt.Errorf("engine: set grants: %w", err)
	}
	return nil
}

var validGrantee = regexp.MustCompile(`^(key|oidc|spiffe|group):.+$`)

// Permitted is true when one of grantees holds one of perms on the policy
// (name "": the tenant-wide grants).
func (e *Engine) Permitted(ctx context.Context, tenant uuid.UUID, name string, grantees []string, perms ...string) (bool, error) {
	var ok bool
	err := store.InTenant(ctx, e.api, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `select exists (select 1 from veho.policy_grants
			where policy is not distinct from nullif($1, '') and grantee = any($2) and permission = any($3))`, name, grantees, perms).Scan(&ok)
	})
	if err != nil {
		return false, fmt.Errorf("engine: permitted: %w", err)
	}
	return ok, nil
}

// InlinePolicies is the tenant's inline_policies setting (default all).
func (e *Engine) InlinePolicies(ctx context.Context, tenant uuid.UUID) (string, error) {
	var mode string
	err := store.InTenant(ctx, e.api, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "select coalesce(settings->>'inline_policies', $1) from veho.tenants where id = veho.current_tenant()", InlineAll).Scan(&mode)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return mode, err
}

func (e *Engine) emitPolicy(ctx context.Context, action string, tenant uuid.UUID, name string, err error, details any) {
	ev := audit.Event{Action: action, Outcome: audit.Success, TenantID: tenant.String(), ResourceType: "policy", ResourceName: name, Details: details}
	if err != nil {
		ev.Outcome, ev.Details = audit.Failed, map[string]string{"error": err.Error()}
	}
	e.audit.Emit(ctx, ev)
}
