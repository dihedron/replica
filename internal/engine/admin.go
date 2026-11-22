package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"uuid"

	"github.com/dihedron/veho/internal/audit"
	"github.com/dihedron/veho/internal/policy"
	"github.com/dihedron/veho/internal/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Tenants and the administration of a tenant (veho-api-server.md §5.2).

// Tenant is a tenant as the platform administrator sees it.
type Tenant struct {
	ID        uuid.UUID
	Name      string
	State     string
	Quotas    Quotas
	Settings  Settings
	CreatedAt time.Time
}

// Quotas are a tenant's limits; 0 is the default.
type Quotas struct {
	MaxDestinations int `json:"max_destinations,omitempty"`
	MaxActiveJobs   int `json:"max_active_jobs,omitempty"`
	// MinRetryInitial and MaxRetryInterval bound a policy's backoff (FR-12):
	// at most 2s and at least 15m, so that the policy defaults stay valid.
	MinRetryInitial  policy.Duration `json:"min_retry_initial,omitempty"`
	MaxRetryInterval policy.Duration `json:"max_retry_interval,omitempty"`
}

// Settings are a tenant's own choices.
type Settings struct {
	InlinePolicies string `json:"inline_policies,omitempty"`
}

func (e *Engine) emitAdmin(ctx context.Context, action string, tenant *uuid.UUID, resource, name string, err error, details any) {
	ev := audit.Event{Action: action, Outcome: audit.Success, ResourceType: resource, ResourceName: name, Details: details}
	if tenant != nil {
		ev.TenantID = tenant.String()
	}
	if err != nil {
		ev.Outcome, ev.Details = audit.Failed, map[string]string{"error": err.Error()}
	}
	e.audit.Emit(ctx, ev)
}

// CreateTenant creates a tenant.
func (e *Engine) CreateTenant(ctx context.Context, name string) (Tenant, error) {
	var id uuid.UUID
	err := e.api.QueryRow(ctx, "select veho.create_tenant($1)", name).Scan(&id)
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == "23505" {
		err = fmt.Errorf("%w: tenant %q exists", ErrConflict, name)
	} else if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == "23514" {
		err = fmt.Errorf("%w: tenant name %q", ErrInvalid, name)
	}
	e.emitAdmin(ctx, "TENANT_CREATE", &id, "tenant", name, err, nil)
	if err != nil {
		return Tenant{}, fmt.Errorf("engine: create tenant: %w", err)
	}
	return e.GetTenant(ctx, id)
}

// ListTenants returns every tenant.
func (e *Engine) ListTenants(ctx context.Context) ([]Tenant, error) {
	rows, err := e.api.Query(ctx, "select id, name, state, quotas, settings, created_at from veho.list_tenants()")
	if err != nil {
		return nil, fmt.Errorf("engine: list tenants: %w", err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Tenant])
	if err != nil {
		return nil, fmt.Errorf("engine: list tenants: %w", err)
	}
	return out, nil
}

// GetTenant returns a tenant.
func (e *Engine) GetTenant(ctx context.Context, id uuid.UUID) (Tenant, error) {
	var t Tenant
	err := store.InTenant(ctx, e.api, id, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, "select id, name, state, quotas, settings, created_at from veho.tenants where id = veho.current_tenant()")
		if err != nil {
			return err
		}
		t, err = pgx.CollectExactlyOneRow(rows, pgx.RowToStructByPos[Tenant])
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	})
	if err != nil {
		return Tenant{}, fmt.Errorf("engine: get tenant: %w", err)
	}
	return t, nil
}

// UpdateTenant sets a tenant's state and quotas; nil leaves them.
func (e *Engine) UpdateTenant(ctx context.Context, id uuid.UUID, state *string, q *Quotas) (Tenant, error) {
	if state != nil && *state != "active" && *state != "suspended" {
		return Tenant{}, fmt.Errorf("engine: update tenant: %w: state %q: want active or suspended", ErrInvalid, *state)
	}
	if q != nil && (q.MaxDestinations < 0 || q.MaxActiveJobs < 0 || q.MaxDestinations > platformMaxDestinations ||
		q.MinRetryInitial < 0 || time.Duration(q.MinRetryInitial) > policyRetryInitial ||
		q.MaxRetryInterval < 0 || (q.MaxRetryInterval > 0 && time.Duration(q.MaxRetryInterval) < policyRetryMaxInterval)) {
		return Tenant{}, fmt.Errorf("engine: update tenant: %w: quotas %+v", ErrInvalid, *q)
	}
	var quotas []byte
	if q != nil {
		quotas, _ = json.Marshal(q)
	}
	rows, err := e.api.Query(ctx, "select id, name, state, quotas, settings, created_at from veho.update_tenant($1, $2, $3)", id, state, quotas)
	var t Tenant
	if err == nil {
		t, err = pgx.CollectExactlyOneRow(rows, pgx.RowToStructByPos[Tenant])
		if errors.Is(err, pgx.ErrNoRows) {
			err = ErrNotFound
		}
	}
	e.emitAdmin(ctx, "TENANT_UPDATE", &id, "tenant", t.Name, err, map[string]any{"state": state, "quotas": q})
	if err != nil {
		return Tenant{}, fmt.Errorf("engine: update tenant: %w", err)
	}
	return t, nil
}

// Settings returns the tenant's settings.
func (e *Engine) Settings(ctx context.Context, tenant uuid.UUID) (Settings, error) {
	t, err := e.GetTenant(ctx, tenant)
	return t.Settings, err
}

// SetSettings replaces the tenant's settings.
func (e *Engine) SetSettings(ctx context.Context, tenant uuid.UUID, s Settings) (Settings, error) {
	switch s.InlinePolicies {
	case "", InlineAll, InlineGranted, InlineNone:
	default:
		return Settings{}, fmt.Errorf("engine: settings: %w: inline_policies %q: want all, granted or none", ErrInvalid, s.InlinePolicies)
	}
	b, _ := json.Marshal(s)
	var out Settings
	err := store.InTenant(ctx, e.api, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "select veho.set_tenant_settings($1)", b).Scan(&out)
	})
	e.emitAdmin(ctx, "TENANT_SETTINGS", &tenant, "tenant", "", err, s)
	if err != nil {
		return Settings{}, fmt.Errorf("engine: settings: %w", err)
	}
	return out, nil
}

// APIKey is what an administrator sees of a key: never its secret.
type APIKey struct {
	ID         string
	Principal  string
	Role       string
	CreatedAt  time.Time
	ExpiresAt  *time.Time
	RevokedAt  *time.Time
	LastUsedAt *time.Time
}

// CreateKey adds a principal of the tenant and its key, whose secret the
// caller made and only the hash of which is stored.
func (e *Engine) CreateKey(ctx context.Context, tenant uuid.UUID, name, role, id string, sum []byte, expires *time.Time) error {
	err := store.InTenant(ctx, e.api, tenant, func(tx pgx.Tx) error {
		var p uuid.UUID
		err := tx.QueryRow(ctx, "insert into veho.principals (tenant_id, name, role) values (veho.current_tenant(), $1, $2) returning id", name, role).Scan(&p)
		if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && (pgErr.Code == "23505" || pgErr.Code == "23514") {
			return fmt.Errorf("%w: principal %q with role %q", ErrInvalid, name, role)
		} else if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, "insert into veho.api_keys (id, principal_id, tenant_id, sha256, expires_at) values ($1, $2, veho.current_tenant(), $3, $4)", id, p, sum, expires)
		return err
	})
	e.emitAdmin(ctx, "APIKEY_CREATE", &tenant, "api_key", id, err, map[string]string{"principal": name, "role": role})
	if err != nil {
		return fmt.Errorf("engine: create key: %w", err)
	}
	return nil
}

// ListKeys returns the tenant's keys.
func (e *Engine) ListKeys(ctx context.Context, tenant uuid.UUID) ([]APIKey, error) {
	var out []APIKey
	err := store.InTenant(ctx, e.api, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `select k.id, 'key:' || p.name, p.role, k.created_at, k.expires_at, k.revoked_at, k.last_used_at
			from veho.api_keys k join veho.principals p on p.id = k.principal_id order by k.created_at`)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, pgx.RowToStructByPos[APIKey])
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("engine: list keys: %w", err)
	}
	return out, nil
}

// RevokeKey revokes a key at once.
func (e *Engine) RevokeKey(ctx context.Context, tenant uuid.UUID, id string) error {
	err := store.InTenant(ctx, e.api, tenant, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, "update veho.api_keys set revoked_at = coalesce(revoked_at, now()) where id = $1", id)
		if err == nil && tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return err
	})
	e.emitAdmin(ctx, "APIKEY_REVOKE", &tenant, "api_key", id, err, nil)
	if err != nil {
		return fmt.Errorf("engine: revoke key: %w", err)
	}
	return nil
}

// Entitlement gives an identity a role in the tenant (§4.4).
type Entitlement struct {
	ID        uuid.UUID
	Kind      string
	Issuer    string
	Subject   string
	Group     string
	Role      string
	CreatedAt time.Time
}

// CreateEntitlement adds an entitlement of the tenant.
func (e *Engine) CreateEntitlement(ctx context.Context, tenant uuid.UUID, en Entitlement) (Entitlement, error) {
	var out Entitlement
	err := store.InTenant(ctx, e.api, tenant, func(tx pgx.Tx) error {
		if (en.Subject == "") == (en.Group == "") || en.Role == "platform-admin" {
			return fmt.Errorf("%w: an entitlement has a subject or a group, and a role of the tenant", ErrInvalid)
		}
		rows, err := tx.Query(ctx, `insert into veho.entitlements (tenant_id, kind, issuer, subject, "group", role)
			values (veho.current_tenant(), $1, $2, nullif($3, ''), nullif($4, ''), $5)
			returning id, kind, issuer, coalesce(subject, ''), coalesce("group", ''), role, created_at`,
			en.Kind, en.Issuer, en.Subject, en.Group, en.Role)
		if err != nil {
			return err
		}
		out, err = pgx.CollectExactlyOneRow(rows, pgx.RowToStructByPos[Entitlement])
		if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == "23514" {
			return fmt.Errorf("%w: entitlement %+v", ErrInvalid, en)
		}
		return err
	})
	e.emitAdmin(ctx, "ENTITLEMENT_CREATE", &tenant, "entitlement", out.ID.String(), err, map[string]string{"kind": en.Kind,
		"issuer": en.Issuer, "subject": en.Subject, "group": en.Group, "role": en.Role})
	if err != nil {
		return Entitlement{}, fmt.Errorf("engine: create entitlement: %w", err)
	}
	return out, nil
}

// ListEntitlements returns the tenant's entitlements.
func (e *Engine) ListEntitlements(ctx context.Context, tenant uuid.UUID) ([]Entitlement, error) {
	var out []Entitlement
	err := store.InTenant(ctx, e.api, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `select id, kind, issuer, coalesce(subject, ''), coalesce("group", ''), role, created_at
			from veho.entitlements order by created_at`)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, pgx.RowToStructByPos[Entitlement])
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("engine: list entitlements: %w", err)
	}
	return out, nil
}

// DeleteEntitlement removes an entitlement.
func (e *Engine) DeleteEntitlement(ctx context.Context, tenant, id uuid.UUID) error {
	err := store.InTenant(ctx, e.api, tenant, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, "delete from veho.entitlements where id = $1", id)
		if err == nil && tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return err
	})
	e.emitAdmin(ctx, "ENTITLEMENT_DELETE", &tenant, "entitlement", id.String(), err, nil)
	if err != nil {
		return fmt.Errorf("engine: delete entitlement: %w", err)
	}
	return nil
}
