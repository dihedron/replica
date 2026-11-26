package api

import (
	"context"
	"fmt"
	"slices"
	"sort"

	vehov1 "github.com/dihedron/veho/api/veho/v1"
	"github.com/jackc/pgx/v5"
)

// authService is AuthService: how a client signs in.
type authService struct {
	vehov1.UnimplementedAuthServiceServer
	s *Server
}

// GetConfig implements AuthService: the issuers that offer the CLI's login,
// by name. Public, so it says nothing that is not in the issuers' own
// discovery documents.
func (a authService) GetConfig(context.Context, *vehov1.GetConfigRequest) (*vehov1.AuthConfig, error) {
	out := &vehov1.AuthConfig{}
	if a.s.ids == nil {
		return out, nil
	}
	for _, i := range a.s.ids.oidc {
		if c := i.conf; c.CLIClientID != "" {
			out.Issuers = append(out.Issuers, &vehov1.AuthIssuer{Name: c.Name, Issuer: c.Issuer, CliClientId: c.CLIClientID, Scopes: slices.Clone(c.Scopes)})
		}
	}
	sort.Slice(out.Issuers, func(i, j int) bool { return out.Issuers[i].GetName() < out.Issuers[j].GetName() })
	return out, nil
}

// identityService is IdentityService: who calls.
type identityService struct {
	vehov1.UnimplementedIdentityServiceServer
	s *Server
}

// WhoAmI implements IdentityService: an OIDC identity's roles in every
// tenant; an API key's or a SPIFFE identity's in the tenant of the call.
func (is identityService) WhoAmI(ctx context.Context, _ *vehov1.WhoAmIRequest) (*vehov1.Identity, error) {
	p := principal(ctx)
	out := &vehov1.Identity{Name: p.Name, Kind: p.id.kind, Issuer: p.id.issuer, Subject: p.id.subject, Groups: p.id.groups}
	if p.id.kind != "oidc" {
		out.Tenants = []*vehov1.TenantRole{{Tenant: p.TenantName, Role: p.Role}}
		return out, nil
	}
	rows, err := is.s.api.Query(ctx, "select tenant, role from veho.entitlements_of($1, $2, $3, $4)", p.id.kind, p.id.issuer, p.id.subject, p.id.groups)
	if err != nil {
		return nil, fmt.Errorf("api: whoami: %w", err)
	}
	type entitlement struct{ Tenant, Role string }
	all, err := pgx.CollectRows(rows, pgx.RowToStructByPos[entitlement])
	if err != nil {
		return nil, fmt.Errorf("api: whoami: %w", err)
	}
	for _, e := range all { // ordered by tenant: keep each tenant's highest role
		if n := len(out.Tenants); n > 0 && out.Tenants[n-1].GetTenant() == e.Tenant {
			if rank[e.Role] > rank[out.Tenants[n-1].GetRole()] {
				out.Tenants[n-1].Role = e.Role
			}
			continue
		}
		out.Tenants = append(out.Tenants, &vehov1.TenantRole{Tenant: e.Tenant, Role: e.Role})
	}
	return out, nil
}
