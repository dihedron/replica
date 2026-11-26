package main

import (
	"context"
	"errors"
	"fmt"
	"time"
	"uuid"

	"github.com/dihedron/veho/internal/api"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// BootstrapCommand is "veho bootstrap" (.specs/veho-cli.md §1): what the
// first administrators need, written in the database as veho_migrator; the
// API does the same afterwards.
type BootstrapCommand struct {
	Tenant      BootstrapTenantCommand      `command:"tenant" description:"Create a tenant"`
	APIKey      BootstrapAPIKeyCommand      `command:"apikey" description:"Create an API key of a service account, and the account if new; the key is printed once"`
	Entitlement BootstrapEntitlementCommand `command:"entitlement" description:"Entitle an OIDC or SPIFFE identity, or a group, to a role in a tenant"`
}

// BootstrapTenantCommand is "veho bootstrap tenant <name>".
type BootstrapTenantCommand struct {
	dbOptions
	auditOptions
	Args struct {
		Name string `positional-arg-name:"name" required:"true"`
	} `positional-args:"true"`
}

// Execute creates the tenant and prints its ID.
func (cmd *BootstrapTenantCommand) Execute([]string) error {
	return audited(cmd.dbOptions, cmd.auditOptions, "bootstrap tenant", "TENANT_CREATE", func(ctx context.Context, conn *pgx.Conn) (any, error) {
		var id uuid.UUID
		if err := conn.QueryRow(ctx, "select veho.create_tenant($1)", cmd.Args.Name).Scan(&id); err != nil {
			return nil, err
		}
		fmt.Println(id)
		return map[string]string{"tenant": id.String(), "name": cmd.Args.Name}, nil
	})
}

// bootstrapKeyLifetime is the lifetime of a bootstrap key, the default and
// the maximum: bootstrap reads no node's configuration, so api.keys does
// not apply to it.
const bootstrapKeyLifetime = 90 * 24 * time.Hour

// BootstrapAPIKeyCommand is "veho bootstrap apikey".
type BootstrapAPIKeyCommand struct {
	dbOptions
	auditOptions
	Tenant        string        `long:"tenant" description:"The tenant's name (not with --platform-admin)"`
	Role          string        `long:"role" choice:"viewer" choice:"operator" choice:"admin" description:"The service account's role in the tenant"`
	PlatformAdmin bool          `long:"platform-admin" description:"A service account of the platform's administrators, of no tenant"`
	Name          string        `long:"name" required:"true" description:"The service account's name; a new key of an existing account rotates it"`
	Expires       time.Duration `long:"expires" description:"The key's lifetime, at most and by default 90 days (2160h)"`
}

// Execute creates the service account if it is new, then its key, and
// prints the key: it is never shown again, nor logged (C7-4). An existing
// account must have the role asked for.
func (cmd *BootstrapAPIKeyCommand) Execute([]string) error {
	if cmd.PlatformAdmin == (cmd.Tenant != "") || cmd.PlatformAdmin == (cmd.Role != "") {
		return errors.New("bootstrap apikey: give --tenant and --role, or --platform-admin")
	}
	if cmd.Expires == 0 {
		cmd.Expires = bootstrapKeyLifetime
	}
	if cmd.Expires < 0 || cmd.Expires > bootstrapKeyLifetime {
		return fmt.Errorf("bootstrap apikey: --expires must be positive and at most %s", bootstrapKeyLifetime)
	}
	role := cmd.Role
	if cmd.PlatformAdmin {
		role = api.RolePlatformAdmin
	}
	token, id, sum := api.NewKey()
	err := audited(cmd.dbOptions, cmd.auditOptions, "bootstrap apikey", "APIKEY_CREATE", func(ctx context.Context, conn *pgx.Conn) (any, error) {
		return map[string]string{"key": id, "service_account": cmd.Name, "role": role, "tenant": cmd.Tenant},
			pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
				var tenant *uuid.UUID
				if cmd.Tenant != "" {
					var t uuid.UUID
					if err := tx.QueryRow(ctx, "select id from veho.tenants where name = $1", cmd.Tenant).Scan(&t); errors.Is(err, pgx.ErrNoRows) {
						return fmt.Errorf("no tenant %q", cmd.Tenant)
					} else if err != nil {
						return err
					}
					tenant = &t
				}
				var (
					account uuid.UUID
					has     string
				)
				if _, err := tx.Exec(ctx, "insert into veho.service_accounts (tenant_id, name, role) values ($1, $2, $3) on conflict do nothing",
					tenant, cmd.Name, role); err != nil {
					return err
				}
				if err := tx.QueryRow(ctx, "select id, role from veho.service_accounts where tenant_id is not distinct from $1 and name = $2",
					tenant, cmd.Name).Scan(&account, &has); err != nil {
					return err
				}
				if has != role {
					return fmt.Errorf("service account %q has the role %s, not %s", cmd.Name, has, role)
				}
				_, err := tx.Exec(ctx, "insert into veho.api_keys (id, service_account_id, tenant_id, sha256, expires_at) values ($1, $2, $3, $4, $5)",
					id, account, tenant, sum, time.Now().Add(cmd.Expires))
				if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == "VHK03" {
					return fmt.Errorf("service account %q has 3 active keys: revoke one first", cmd.Name)
				}
				return err
			})
	})
	if err != nil {
		return err
	}
	fmt.Println(token)
	return nil
}

// BootstrapEntitlementCommand is "veho bootstrap entitlement"
// (.specs/veho-api-server.md §4.4).
type BootstrapEntitlementCommand struct {
	dbOptions
	auditOptions
	Kind          string `long:"kind" required:"true" choice:"oidc" choice:"spiffe" description:"The kind of identity"`
	Issuer        string `long:"issuer" required:"true" description:"The OIDC issuer URL, or the SPIFFE trust domain"`
	Subject       string `long:"subject" description:"One identity: the OIDC sub, or the SPIFFE ID"`
	Group         string `long:"group" description:"The members of an OIDC group"`
	Tenant        string `long:"tenant" description:"The tenant's name (not with --platform-admin)"`
	Role          string `long:"role" choice:"viewer" choice:"operator" choice:"admin" description:"The role in the tenant"`
	PlatformAdmin bool   `long:"platform-admin" description:"A platform administrator, of no tenant"`
}

// Execute creates the entitlement and prints its ID.
func (cmd *BootstrapEntitlementCommand) Execute([]string) error {
	if (cmd.Subject == "") == (cmd.Group == "") {
		return errors.New("bootstrap entitlement: give --subject or --group")
	}
	if cmd.PlatformAdmin == (cmd.Tenant != "") || cmd.PlatformAdmin == (cmd.Role != "") {
		return errors.New("bootstrap entitlement: give --tenant and --role, or --platform-admin")
	}
	role := cmd.Role
	if cmd.PlatformAdmin {
		role = api.RolePlatformAdmin
	}
	return audited(cmd.dbOptions, cmd.auditOptions, "bootstrap entitlement", "ENTITLEMENT_CREATE", func(ctx context.Context, conn *pgx.Conn) (any, error) {
		var id uuid.UUID
		err := conn.QueryRow(ctx, `insert into veho.entitlements (tenant_id, kind, issuer, subject, "group", role)
			values ((select id from veho.tenants where name = nullif($1, '')), $2, $3, nullif($4, ''), nullif($5, ''), $6) returning id`,
			cmd.Tenant, cmd.Kind, cmd.Issuer, cmd.Subject, cmd.Group, role).Scan(&id)
		if err != nil {
			return nil, err
		}
		fmt.Println(id)
		return map[string]string{"entitlement": id.String(), "kind": cmd.Kind, "issuer": cmd.Issuer, "subject": cmd.Subject,
			"group": cmd.Group, "tenant": cmd.Tenant, "role": role}, nil
	})
}
