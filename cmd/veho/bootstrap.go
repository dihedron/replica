package main

import (
	"context"
	"errors"
	"fmt"
	"time"
	"uuid"

	"github.com/dihedron/veho/internal/api"
	"github.com/jackc/pgx/v5"
)

// TenantCommand is "veho tenant" (.specs/veho-api-server.md §4.3): the
// operator's bootstrap of tenants, as veho_migrator.
type TenantCommand struct {
	Create TenantCreateCommand `command:"create" description:"Create a tenant"`
}

// TenantCreateCommand is "veho tenant create <name>".
type TenantCreateCommand struct {
	dbOptions
	auditOptions
	Args struct {
		Name string `positional-arg-name:"name" required:"true"`
	} `positional-args:"true"`
}

// Execute creates the tenant and prints its ID.
func (cmd *TenantCreateCommand) Execute([]string) error {
	return audited(cmd.dbOptions, cmd.auditOptions, "tenant create", "TENANT_CREATE", func(ctx context.Context, conn *pgx.Conn) (any, error) {
		var id uuid.UUID
		if err := conn.QueryRow(ctx, "select veho.create_tenant($1)", cmd.Args.Name).Scan(&id); err != nil {
			return nil, err
		}
		fmt.Println(id)
		return map[string]string{"tenant": id.String(), "name": cmd.Args.Name}, nil
	})
}

// APIKeyCommand is "veho apikey".
type APIKeyCommand struct {
	Create APIKeyCreateCommand `command:"create" description:"Create a principal and its API key; the key is printed once"`
}

// APIKeyCreateCommand is "veho apikey create".
type APIKeyCreateCommand struct {
	dbOptions
	auditOptions
	Tenant        string        `long:"tenant" description:"The tenant's name (not with --platform-admin)"`
	Role          string        `long:"role" choice:"viewer" choice:"operator" choice:"admin" description:"The principal's role in the tenant"`
	PlatformAdmin bool          `long:"platform-admin" description:"A platform administrator, of no tenant"`
	Name          string        `long:"name" required:"true" description:"The principal's name"`
	Expires       time.Duration `long:"expires" description:"Expiry of the key from now; 0 never expires"`
}

// Execute creates the principal and the key, and prints the key: it is
// never shown again, nor logged (C7-4).
func (cmd *APIKeyCreateCommand) Execute([]string) error {
	if cmd.PlatformAdmin == (cmd.Tenant != "") || cmd.PlatformAdmin == (cmd.Role != "") {
		return errors.New("apikey create: give --tenant and --role, or --platform-admin")
	}
	role := cmd.Role
	if cmd.PlatformAdmin {
		role = api.RolePlatformAdmin
	}
	token, id, sum := api.NewKey()
	err := audited(cmd.dbOptions, cmd.auditOptions, "apikey create", "APIKEY_CREATE", func(ctx context.Context, conn *pgx.Conn) (any, error) {
		return map[string]string{"key": id, "name": cmd.Name, "role": role, "tenant": cmd.Tenant},
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
				var principal uuid.UUID
				if err := tx.QueryRow(ctx, "insert into veho.principals (tenant_id, name, role) values ($1, $2, $3) returning id",
					tenant, cmd.Name, role).Scan(&principal); err != nil {
					return err
				}
				var expires *time.Time
				if cmd.Expires > 0 {
					e := time.Now().Add(cmd.Expires)
					expires = &e
				}
				_, err := tx.Exec(ctx, "insert into veho.api_keys (id, principal_id, tenant_id, sha256, expires_at) values ($1, $2, $3, $4, $5)",
					id, principal, tenant, sum, expires)
				return err
			})
	})
	if err != nil {
		return err
	}
	fmt.Println(token)
	return nil
}

// EntitlementCommand is "veho entitlement".
type EntitlementCommand struct {
	Create EntitlementCreateCommand `command:"create" description:"Entitle an OIDC or SPIFFE identity, or a group, to a role in a tenant"`
}

// EntitlementCreateCommand is "veho entitlement create" (.specs/veho-api-server.md
// §4.4).
type EntitlementCreateCommand struct {
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
func (cmd *EntitlementCreateCommand) Execute([]string) error {
	if (cmd.Subject == "") == (cmd.Group == "") {
		return errors.New("entitlement create: give --subject or --group")
	}
	if cmd.PlatformAdmin == (cmd.Tenant != "") || cmd.PlatformAdmin == (cmd.Role != "") {
		return errors.New("entitlement create: give --tenant and --role, or --platform-admin")
	}
	role := cmd.Role
	if cmd.PlatformAdmin {
		role = api.RolePlatformAdmin
	}
	return audited(cmd.dbOptions, cmd.auditOptions, "entitlement create", "ENTITLEMENT_CREATE", func(ctx context.Context, conn *pgx.Conn) (any, error) {
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
