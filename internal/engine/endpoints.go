package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"uuid"

	"github.com/dihedron/veho/internal/audit"
	"github.com/dihedron/veho/internal/secrets"
	"github.com/dihedron/veho/internal/store"
	"github.com/dihedron/veho/pkg/plugin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Endpoints of a tenant (veho-api-server.md §5.2).

var validEndpointName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// Endpoint is a tenant's storage endpoint.
type Endpoint struct {
	Name          string
	Driver        string
	Location      string
	Config        json.RawMessage
	CredentialRef string
	CreatedAt     time.Time
}

// check refuses an endpoint whose driver, location, configuration or
// reference is wrong, inside tx.
func (e *Engine) checkEndpoint(ctx context.Context, tx pgx.Tx, ep Endpoint) error {
	if !validEndpointName.MatchString(ep.Name) {
		return fmt.Errorf("%w: endpoint name %q: want %s", ErrInvalid, ep.Name, validEndpointName)
	}
	if _, ok := e.drivers[ep.Driver]; !ok {
		return fmt.Errorf("%w: no driver %q on this node", ErrInvalid, ep.Driver)
	}
	if len(bytes.TrimSpace(ep.Config)) == 0 || bytes.TrimSpace(ep.Config)[0] != '{' {
		return fmt.Errorf("%w: the configuration must be a JSON object", ErrInvalid)
	}
	topo, err := LoadTopology(ctx, tx)
	if err != nil {
		return err
	}
	if _, err := topo.Position(ep.Location); err != nil {
		return fmt.Errorf("%w: location %q: %w", ErrInvalid, ep.Location, err)
	}
	if ep.CredentialRef != "" {
		name, ok := strings.CutPrefix(ep.CredentialRef, secrets.RefPrefix)
		if !ok {
			return fmt.Errorf("%w: credential_ref %q: want %s<name>", ErrInvalid, ep.CredentialRef, secrets.RefPrefix)
		}
		if strings.HasPrefix(name, SecretPrefix) {
			return fmt.Errorf("%w: credential_ref %q: secrets starting with %s are veho's own", ErrInvalid, ep.CredentialRef, SecretPrefix)
		}
		var n int
		if err := tx.QueryRow(ctx, "select count(*) from veho.secrets where name = $1", name).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("secret %q: %w", name, ErrNotFound)
		}
	}
	return nil
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

const endpointColumns = "name, driver, location_id, config, coalesce(credential_ref, ''), created_at"

// CreateEndpoint adds an endpoint.
func (e *Engine) CreateEndpoint(ctx context.Context, tenant uuid.UUID, ep Endpoint) (Endpoint, error) {
	var out Endpoint
	err := store.InTenant(ctx, e.api, tenant, func(tx pgx.Tx) error {
		if err := e.checkEndpoint(ctx, tx, ep); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `insert into veho.endpoints (tenant_id, name, driver, location_id, config, credential_ref)
			values (veho.current_tenant(), $1, $2, $3, $4, $5) returning `+endpointColumns,
			ep.Name, ep.Driver, ep.Location, ep.Config, nullable(ep.CredentialRef))
		if err != nil {
			return err
		}
		out, err = pgx.CollectExactlyOneRow(rows, pgx.RowToStructByPos[Endpoint])
		if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == "23505" {
			return fmt.Errorf("%w: endpoint %q exists", ErrConflict, ep.Name)
		}
		return err
	})
	e.emitEndpoint(ctx, "ENDPOINT_CREATE", tenant, ep.Name, err)
	if err != nil {
		return Endpoint{}, fmt.Errorf("engine: create endpoint: %w", err)
	}
	return out, nil
}

// ErrConflict is a resource that exists already (409).
var ErrConflict = errors.New("already exists")

// GetEndpoint returns an endpoint.
func (e *Engine) GetEndpoint(ctx context.Context, tenant uuid.UUID, name string) (Endpoint, error) {
	var out Endpoint
	err := store.InTenant(ctx, e.api, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, "select "+endpointColumns+" from veho.endpoints where name = $1", name)
		if err != nil {
			return err
		}
		out, err = pgx.CollectExactlyOneRow(rows, pgx.RowToStructByPos[Endpoint])
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("endpoint %q: %w", name, ErrNotFound)
		}
		return err
	})
	if err != nil {
		return Endpoint{}, fmt.Errorf("engine: get endpoint: %w", err)
	}
	return out, nil
}

// ListEndpoints returns the tenant's endpoints, by name.
func (e *Engine) ListEndpoints(ctx context.Context, tenant uuid.UUID) ([]Endpoint, error) {
	var out []Endpoint
	err := store.InTenant(ctx, e.api, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, "select "+endpointColumns+" from veho.endpoints order by name")
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, pgx.RowToStructByPos[Endpoint])
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("engine: list endpoints: %w", err)
	}
	return out, nil
}

// UpdateEndpoint changes an endpoint's location, configuration and
// reference; its driver stays.
func (e *Engine) UpdateEndpoint(ctx context.Context, tenant uuid.UUID, ep Endpoint) (Endpoint, error) {
	var out Endpoint
	err := store.InTenant(ctx, e.api, tenant, func(tx pgx.Tx) error {
		var driver string
		if err := tx.QueryRow(ctx, "select driver from veho.endpoints where name = $1 for update", ep.Name).Scan(&driver); errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("endpoint %q: %w", ep.Name, ErrNotFound)
		} else if err != nil {
			return err
		}
		if ep.Driver != "" && ep.Driver != driver {
			return fmt.Errorf("%w: the driver of an endpoint does not change", ErrInvalid)
		}
		ep.Driver = driver
		if err := e.checkEndpoint(ctx, tx, ep); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `update veho.endpoints set location_id = $2, config = $3, credential_ref = $4 where name = $1
			returning `+endpointColumns, ep.Name, ep.Location, ep.Config, nullable(ep.CredentialRef))
		if err != nil {
			return err
		}
		out, err = pgx.CollectExactlyOneRow(rows, pgx.RowToStructByPos[Endpoint])
		return err
	})
	e.emitEndpoint(ctx, "ENDPOINT_UPDATE", tenant, ep.Name, err)
	if err != nil {
		return Endpoint{}, fmt.Errorf("engine: update endpoint: %w", err)
	}
	return out, nil
}

// DeleteEndpoint removes an endpoint no job references.
func (e *Engine) DeleteEndpoint(ctx context.Context, tenant uuid.UUID, name string) error {
	err := store.InTenant(ctx, e.api, tenant, func(tx pgx.Tx) error {
		var used bool
		if err := tx.QueryRow(ctx, `select exists (select 1 from veho.jobs j join veho.endpoints e on e.id = j.source_endpoint_id where e.name = $1)
			or exists (select 1 from veho.transfers t join veho.endpoints e on e.id = t.destination_endpoint_id where e.name = $1)`, name).Scan(&used); err != nil {
			return err
		}
		if used {
			return ErrInUse
		}
		tag, err := tx.Exec(ctx, "delete from veho.endpoints where name = $1", name)
		if err == nil && tag.RowsAffected() == 0 {
			return fmt.Errorf("endpoint %q: %w", name, ErrNotFound)
		}
		return err
	})
	e.emitEndpoint(ctx, "ENDPOINT_DELETE", tenant, name, err)
	if err != nil {
		return fmt.Errorf("engine: delete endpoint: %w", err)
	}
	return nil
}

// Probe is the outcome of TestEndpoint: OK, or the step that failed.
type Probe struct {
	OK      bool
	Step    string
	Class   plugin.Class
	Message string
}

// TestEndpoint probes an endpoint with its credentials (§5.2): open, a
// Stat of a random key, which must be "not found", and for a sink a
// zero-byte upload committed and deleted.
func (e *Engine) TestEndpoint(ctx context.Context, tenant uuid.UUID, name string) (Probe, error) {
	var id uuid.UUID
	if err := store.InTenant(ctx, e.api, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "select id from veho.endpoints where name = $1", name).Scan(&id)
	}); errors.Is(err, pgx.ErrNoRows) {
		return Probe{}, fmt.Errorf("engine: test endpoint: endpoint %q: %w", name, ErrNotFound)
	} else if err != nil {
		return Probe{}, fmt.Errorf("engine: test endpoint: %w", err)
	}
	failed := func(step string, err error) (Probe, error) {
		r := fail(err)
		return Probe{Step: step, Class: r.Class, Message: r.Err}, nil
	}
	d, c, err := e.open(ctx, tenant, id)
	if err != nil {
		return failed("open", err)
	}
	defer func() { _ = c.Close() }()
	ref := plugin.Ref{Key: ".veho-probe-" + uuid.New().String()}
	if _, err := c.Stat(ctx, ref); !errors.Is(err, plugin.ErrNotFound) {
		if err == nil {
			err = plugin.Wrapf(plugin.Permanent, "a random key exists")
		}
		return failed("stat", drv(err))
	}
	if d.Capabilities().Sink {
		// an empty object is one empty part 0, as the engine writes it
		up, err := c.Begin(ctx, ref, plugin.ObjectInfo{}, plugin.PutOptions{})
		var part plugin.PartResult
		if err == nil {
			part, err = up.WritePart(ctx, 0, bytes.NewReader(nil), 0)
		}
		if err == nil {
			_, err = up.Commit(ctx, []plugin.PartResult{part})
		}
		if err != nil {
			return failed("write", drv(err))
		}
		if err := c.Delete(ctx, ref); err != nil {
			return failed("delete", drv(err))
		}
	}
	return Probe{OK: true}, nil
}

func (e *Engine) emitEndpoint(ctx context.Context, action string, tenant uuid.UUID, name string, err error) {
	ev := audit.Event{Action: action, Outcome: audit.Success, TenantID: tenant.String(), ResourceType: "endpoint", ResourceName: name}
	if err != nil {
		ev.Outcome, ev.Details = audit.Failed, map[string]string{"error": err.Error()}
	}
	e.audit.Emit(ctx, ev)
}
