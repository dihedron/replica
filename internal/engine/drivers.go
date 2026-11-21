package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"uuid"

	"github.com/dihedron/veho/internal/store"
	"github.com/dihedron/veho/pkg/plugin"
	"github.com/jackc/pgx/v5"
)

// driver returns the driver registered under name (§6).
func (e *Engine) driver(name string) (plugin.Driver, error) {
	d, ok := e.drivers[name]
	if !ok {
		return nil, plugin.Wrapf(plugin.Permanent, "engine: no driver %q on this node", name)
	}
	return d, nil
}

// open opens a client of the tenant's endpoint for one step: the endpoint's
// credentials are fetched now and zeroed before open returns (SEC-10); the
// caller closes the client.
func (e *Engine) open(ctx context.Context, tenant, endpoint uuid.UUID) (plugin.Driver, plugin.Client, error) {
	var (
		name string
		cfg  []byte
		ref  *string
	)
	if err := store.InTenant(ctx, e.worker, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "select driver, config, credential_ref from veho.endpoints where id = $1", endpoint).Scan(&name, &cfg, &ref)
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, plugin.Wrapf(plugin.Permanent, "engine: endpoint %s is gone", endpoint)
		}
		return nil, nil, fmt.Errorf("engine: endpoint %s: %w", endpoint, err)
	}
	d, err := e.driver(name)
	if err != nil {
		return nil, nil, err
	}
	var creds plugin.Credentials
	if ref != nil && *ref != "" {
		secret, err := e.secrets.Fetch(ctx, tenant, *ref)
		if err != nil {
			return nil, nil, err // ErrUnavailable: Transient
		}
		err = json.Unmarshal(secret.Bytes(), &creds)
		secret.Zero()
		if err != nil {
			return nil, nil, plugin.Wrapf(plugin.Permanent, "engine: the secret of endpoint %s is not a JSON object of strings", endpoint)
		}
	}
	c, err := d.Open(ctx, cfg, creds)
	clear(creds) // the strings themselves cannot be zeroed (veho-secrets.md §3)
	if err != nil {
		return nil, nil, drv(fmt.Errorf("engine: open endpoint %s: %w", endpoint, err))
	}
	return d, c, nil
}
