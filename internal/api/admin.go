package api

import (
	"context"
	"time"
	"uuid"

	vehov1 "github.com/dihedron/veho/api/veho/v1"
	"github.com/dihedron/veho/internal/engine"
	"github.com/dihedron/veho/internal/policy"
	"github.com/dihedron/veho/internal/secrets"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// The administration services (§5.2). Their roles are in the interceptors'
// table; each acts on the caller's tenant, except TenantService.

type endpoints struct {
	vehov1.UnimplementedEndpointServiceServer
	s *Server
}

type secretsService struct {
	vehov1.UnimplementedSecretServiceServer
	s *Server
}

type tenants struct {
	vehov1.UnimplementedTenantServiceServer
	s *Server
}

type admin struct {
	vehov1.UnimplementedAdminServiceServer
	s *Server
}

func toEndpoint(ep engine.Endpoint) (*vehov1.Endpoint, error) {
	st := &structpb.Struct{}
	if err := protojson.Unmarshal(ep.Config, st); err != nil {
		return nil, err
	}
	return &vehov1.Endpoint{Name: ep.Name, Driver: ep.Driver, Location: ep.Location, Config: st, CredentialRef: ep.CredentialRef,
		CreatedAt: timestamppb.New(ep.CreatedAt)}, nil
}

func fromEndpoint(r *vehov1.Endpoint) (engine.Endpoint, error) {
	cfg := []byte("{}")
	if r.GetConfig() != nil {
		var err error
		if cfg, err = protojson.Marshal(r.GetConfig()); err != nil {
			return engine.Endpoint{}, fail(pInvalid, "config: "+err.Error())
		}
	}
	return engine.Endpoint{Name: r.GetName(), Driver: r.GetDriver(), Location: r.GetLocation(), Config: cfg, CredentialRef: r.GetCredentialRef()}, nil
}

// CreateEndpoint implements EndpointService.
func (es endpoints) CreateEndpoint(ctx context.Context, r *vehov1.Endpoint) (*vehov1.Endpoint, error) {
	t, err := tenant(ctx)
	if err != nil {
		return nil, err
	}
	ep, err := fromEndpoint(r)
	if err != nil {
		return nil, err
	}
	out, err := es.s.engine.CreateEndpoint(ctx, t, ep)
	if err != nil {
		return nil, err
	}
	return toEndpoint(out)
}

// GetEndpoint implements EndpointService.
func (es endpoints) GetEndpoint(ctx context.Context, r *vehov1.EndpointName) (*vehov1.Endpoint, error) {
	t, err := tenant(ctx)
	if err != nil {
		return nil, err
	}
	out, err := es.s.engine.GetEndpoint(ctx, t, r.GetName())
	if err != nil {
		return nil, err
	}
	return toEndpoint(out)
}

// ListEndpoints implements EndpointService.
func (es endpoints) ListEndpoints(ctx context.Context, _ *vehov1.ListEndpointsRequest) (*vehov1.ListEndpointsResponse, error) {
	t, err := tenant(ctx)
	if err != nil {
		return nil, err
	}
	all, err := es.s.engine.ListEndpoints(ctx, t)
	if err != nil {
		return nil, err
	}
	out := &vehov1.ListEndpointsResponse{}
	for _, ep := range all {
		e, err := toEndpoint(ep)
		if err != nil {
			return nil, err
		}
		out.Endpoints = append(out.Endpoints, e)
	}
	return out, nil
}

// UpdateEndpoint implements EndpointService.
func (es endpoints) UpdateEndpoint(ctx context.Context, r *vehov1.Endpoint) (*vehov1.Endpoint, error) {
	t, err := tenant(ctx)
	if err != nil {
		return nil, err
	}
	ep, err := fromEndpoint(r)
	if err != nil {
		return nil, err
	}
	out, err := es.s.engine.UpdateEndpoint(ctx, t, ep)
	if err != nil {
		return nil, err
	}
	return toEndpoint(out)
}

// DeleteEndpoint implements EndpointService.
func (es endpoints) DeleteEndpoint(ctx context.Context, r *vehov1.EndpointName) (*vehov1.Empty, error) {
	t, err := tenant(ctx)
	if err != nil {
		return nil, err
	}
	return &vehov1.Empty{}, es.s.engine.DeleteEndpoint(ctx, t, r.GetName())
}

// TestEndpoint implements EndpointService.
func (es endpoints) TestEndpoint(ctx context.Context, r *vehov1.EndpointName) (*vehov1.EndpointTest, error) {
	t, err := tenant(ctx)
	if err != nil {
		return nil, err
	}
	p, err := es.s.engine.TestEndpoint(ctx, t, r.GetName())
	if err != nil {
		return nil, err
	}
	out := &vehov1.EndpointTest{Ok: p.OK, Step: p.Step, Message: p.Message}
	if !p.OK {
		out.Code = p.Class.String()
	}
	return out, nil
}

func toSecret(m secrets.Meta) *vehov1.Secret {
	return &vehov1.Secret{Name: m.Name, Version: int32(m.Version), KekVersion: int32(m.KEKVersion), //nolint:gosec // small
		CreatedAt: timestamppb.New(m.CreatedAt), UpdatedAt: timestamppb.New(m.UpdatedAt)}
}

// PutSecret implements SecretService.
func (ss secretsService) PutSecret(ctx context.Context, r *vehov1.PutSecretRequest) (*vehov1.Secret, error) {
	t, err := tenant(ctx)
	if err != nil {
		return nil, err
	}
	if reserved(r.GetName()) {
		return nil, fail(pInvalid, "secret names starting with "+engine.SecretPrefix+" are veho's own")
	}
	m, err := ss.s.secrets.Put(ctx, t, r.GetName(), r.GetValue())
	if err != nil {
		return nil, err
	}
	return toSecret(m), nil
}

// GetSecret implements SecretService: metadata only.
func (ss secretsService) GetSecret(ctx context.Context, r *vehov1.SecretName) (*vehov1.Secret, error) {
	t, err := tenant(ctx)
	if err != nil {
		return nil, err
	}
	if reserved(r.GetName()) {
		return nil, engine.ErrNotFound
	}
	m, err := ss.s.secrets.Show(ctx, t, r.GetName())
	if err != nil {
		return nil, err
	}
	return toSecret(m), nil
}

// ListSecrets implements SecretService.
func (ss secretsService) ListSecrets(ctx context.Context, _ *vehov1.ListSecretsRequest) (*vehov1.ListSecretsResponse, error) {
	t, err := tenant(ctx)
	if err != nil {
		return nil, err
	}
	all, err := ss.s.secrets.List(ctx, t)
	if err != nil {
		return nil, err
	}
	out := &vehov1.ListSecretsResponse{}
	for _, m := range all {
		if reserved(m.Name) {
			continue
		}
		out.Secrets = append(out.Secrets, toSecret(m))
	}
	return out, nil
}

// DeleteSecret implements SecretService.
func (ss secretsService) DeleteSecret(ctx context.Context, r *vehov1.SecretName) (*vehov1.Empty, error) {
	t, err := tenant(ctx)
	if err != nil {
		return nil, err
	}
	if reserved(r.GetName()) {
		return nil, engine.ErrNotFound
	}
	return &vehov1.Empty{}, ss.s.secrets.Delete(ctx, t, r.GetName())
}

func toTenant(t engine.Tenant) *vehov1.Tenant {
	return &vehov1.Tenant{Id: t.ID.String(), Name: t.Name, State: t.State, CreatedAt: timestamppb.New(t.CreatedAt),
		Quotas: &vehov1.Quotas{MaxDestinations: int32(t.Quotas.MaxDestinations), MaxActiveJobs: int32(t.Quotas.MaxActiveJobs), //nolint:gosec // small
			MinRetryInitial: duration(t.Quotas.MinRetryInitial), MaxRetryInterval: duration(t.Quotas.MaxRetryInterval)},
		Settings: &vehov1.TenantSettings{InlinePolicies: t.Settings.InlinePolicies}}
}

func tenantID(s string) (uuid.UUID, error) {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.UUID{}, engine.ErrNotFound
	}
	return id, nil
}

// CreateTenant implements TenantService.
func (ts tenants) CreateTenant(ctx context.Context, r *vehov1.CreateTenantRequest) (*vehov1.Tenant, error) {
	t, err := ts.s.engine.CreateTenant(ctx, r.GetName())
	if err != nil {
		return nil, err
	}
	return toTenant(t), nil
}

// GetTenant implements TenantService.
func (ts tenants) GetTenant(ctx context.Context, r *vehov1.TenantID) (*vehov1.Tenant, error) {
	id, err := tenantID(r.GetId())
	if err != nil {
		return nil, err
	}
	t, err := ts.s.engine.GetTenant(ctx, id)
	if err != nil {
		return nil, err
	}
	return toTenant(t), nil
}

// ListTenants implements TenantService.
func (ts tenants) ListTenants(ctx context.Context, _ *vehov1.ListTenantsRequest) (*vehov1.ListTenantsResponse, error) {
	all, err := ts.s.engine.ListTenants(ctx)
	if err != nil {
		return nil, err
	}
	out := &vehov1.ListTenantsResponse{}
	for _, t := range all {
		out.Tenants = append(out.Tenants, toTenant(t))
	}
	return out, nil
}

// UpdateTenant implements TenantService.
func (ts tenants) UpdateTenant(ctx context.Context, r *vehov1.UpdateTenantRequest) (*vehov1.Tenant, error) {
	id, err := tenantID(r.GetId())
	if err != nil {
		return nil, err
	}
	var state *string
	if r.GetState() != "" {
		s := r.GetState()
		state = &s
	}
	var q *engine.Quotas
	if r.GetQuotas() != nil {
		rq := r.GetQuotas()
		q = &engine.Quotas{MaxDestinations: int(rq.GetMaxDestinations()), MaxActiveJobs: int(rq.GetMaxActiveJobs()),
			MinRetryInitial: policy.Duration(rq.GetMinRetryInitial().AsDuration()), MaxRetryInterval: policy.Duration(rq.GetMaxRetryInterval().AsDuration())}
	}
	t, err := ts.s.engine.UpdateTenant(ctx, id, state, q)
	if err != nil {
		return nil, err
	}
	return toTenant(t), nil
}

// tenantRoles are the roles a tenant admin may give a key.
var tenantRoles = map[string]bool{RoleViewer: true, RoleOperator: true, RoleAdmin: true}

// CreateKey implements AdminService: the key is in this answer only.
func (as admin) CreateKey(ctx context.Context, r *vehov1.CreateKeyRequest) (*vehov1.CreatedKey, error) {
	t, err := tenant(ctx)
	if err != nil {
		return nil, err
	}
	if !tenantRoles[r.GetRole()] || r.GetName() == "" || r.GetExpiresIn() < 0 {
		return nil, fail(pInvalid, "a name, a role (viewer, operator, admin) and a non-negative expiry are required")
	}
	var expires *time.Time
	if r.GetExpiresIn() > 0 {
		e := time.Now().Add(time.Duration(r.GetExpiresIn()) * time.Second)
		expires = &e
	}
	token, id, sum := NewKey()
	if err := as.s.engine.CreateKey(ctx, t, r.GetName(), r.GetRole(), id, sum, expires); err != nil {
		return nil, err
	}
	return &vehov1.CreatedKey{Id: id, Key: token}, nil
}

// ListKeys implements AdminService: no secret.
func (as admin) ListKeys(ctx context.Context, _ *vehov1.ListKeysRequest) (*vehov1.ListKeysResponse, error) {
	t, err := tenant(ctx)
	if err != nil {
		return nil, err
	}
	all, err := as.s.engine.ListKeys(ctx, t)
	if err != nil {
		return nil, err
	}
	out := &vehov1.ListKeysResponse{}
	for _, k := range all {
		out.Keys = append(out.Keys, &vehov1.ApiKey{Id: k.ID, Principal: k.Principal, Role: k.Role, CreatedAt: timestamppb.New(k.CreatedAt),
			ExpiresAt: ts(k.ExpiresAt), RevokedAt: ts(k.RevokedAt), LastUsedAt: ts(k.LastUsedAt)})
	}
	return out, nil
}

// RevokeKey implements AdminService.
func (as admin) RevokeKey(ctx context.Context, r *vehov1.KeyID) (*vehov1.Empty, error) {
	t, err := tenant(ctx)
	if err != nil {
		return nil, err
	}
	return &vehov1.Empty{}, as.s.engine.RevokeKey(ctx, t, r.GetId())
}

func toEntitlement(e engine.Entitlement) *vehov1.Entitlement {
	return &vehov1.Entitlement{Id: e.ID.String(), Kind: e.Kind, Issuer: e.Issuer, Subject: e.Subject, Group: e.Group, Role: e.Role,
		CreatedAt: timestamppb.New(e.CreatedAt)}
}

// CreateEntitlement implements AdminService.
func (as admin) CreateEntitlement(ctx context.Context, r *vehov1.Entitlement) (*vehov1.Entitlement, error) {
	t, err := tenant(ctx)
	if err != nil {
		return nil, err
	}
	e, err := as.s.engine.CreateEntitlement(ctx, t, engine.Entitlement{Kind: r.GetKind(), Issuer: r.GetIssuer(), Subject: r.GetSubject(),
		Group: r.GetGroup(), Role: r.GetRole()})
	if err != nil {
		return nil, err
	}
	return toEntitlement(e), nil
}

// ListEntitlements implements AdminService.
func (as admin) ListEntitlements(ctx context.Context, _ *vehov1.ListEntitlementsRequest) (*vehov1.ListEntitlementsResponse, error) {
	t, err := tenant(ctx)
	if err != nil {
		return nil, err
	}
	all, err := as.s.engine.ListEntitlements(ctx, t)
	if err != nil {
		return nil, err
	}
	out := &vehov1.ListEntitlementsResponse{}
	for _, e := range all {
		out.Entitlements = append(out.Entitlements, toEntitlement(e))
	}
	return out, nil
}

// DeleteEntitlement implements AdminService.
func (as admin) DeleteEntitlement(ctx context.Context, r *vehov1.EntitlementID) (*vehov1.Empty, error) {
	t, err := tenant(ctx)
	if err != nil {
		return nil, err
	}
	id, err := uuid.Parse(r.GetId())
	if err != nil {
		return nil, engine.ErrNotFound
	}
	return &vehov1.Empty{}, as.s.engine.DeleteEntitlement(ctx, t, id)
}

// GetSettings implements AdminService.
func (as admin) GetSettings(ctx context.Context, _ *vehov1.GetSettingsRequest) (*vehov1.TenantSettings, error) {
	t, err := tenant(ctx)
	if err != nil {
		return nil, err
	}
	s, err := as.s.engine.Settings(ctx, t)
	if err != nil {
		return nil, err
	}
	return &vehov1.TenantSettings{InlinePolicies: s.InlinePolicies}, nil
}

// UpdateSettings implements AdminService.
func (as admin) UpdateSettings(ctx context.Context, r *vehov1.TenantSettings) (*vehov1.TenantSettings, error) {
	t, err := tenant(ctx)
	if err != nil {
		return nil, err
	}
	s, err := as.s.engine.SetSettings(ctx, t, engine.Settings{InlinePolicies: r.GetInlinePolicies()})
	if err != nil {
		return nil, err
	}
	return &vehov1.TenantSettings{InlinePolicies: s.InlinePolicies}, nil
}

// duration is a quota's duration, nil when unset (the default).
func duration(d policy.Duration) *durationpb.Duration {
	if d == 0 {
		return nil
	}
	return durationpb.New(time.Duration(d))
}
