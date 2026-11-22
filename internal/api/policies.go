package api

import (
	"context"
	"errors"

	vehov1 "github.com/dihedron/veho/api/veho/v1"
	"github.com/dihedron/veho/internal/engine"
	"github.com/dihedron/veho/internal/policy"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// policies is PolicyService (§5.1): every method checks the caller's
// grants itself; a tenant admin may do everything.
type policies struct {
	vehov1.UnimplementedPolicyServiceServer
	s *Server
}

// allowed is true when the caller is an admin or holds one of perms on the
// policy name ("": the tenant-wide grants).
func (ps policies) allowed(ctx context.Context, name string, perms ...string) (bool, error) {
	p := principal(ctx)
	if p.may(RoleAdmin) {
		return true, nil
	}
	t, err := tenant(ctx)
	if err != nil {
		return false, err
	}
	return ps.s.engine.Permitted(ctx, t, name, p.grantees(), perms...)
}

// reads are the permissions that let a caller see a policy.
var reads = []string{engine.PermRead, engine.PermSubmit, engine.PermManage}

// need fails with NotFound, as a missing policy, unless allowed (INV-4).
func (ps policies) need(ctx context.Context, name string, perms ...string) error {
	ok, err := ps.allowed(ctx, name, perms...)
	if err != nil {
		return err
	}
	if !ok {
		if seen, _ := ps.allowed(ctx, name, reads...); seen {
			return fail(pPermission, "not allowed on this policy")
		}
		return engine.ErrNotFound
	}
	return nil
}

func toPolicy(pv engine.PolicyVersion) (*vehov1.Policy, error) {
	st := &structpb.Struct{}
	if err := protojson.Unmarshal(pv.Spec, st); err != nil {
		return nil, err
	}
	return &vehov1.Policy{Name: pv.Name, Version: int32(pv.Version), Policy: st, CreatedAt: timestamppb.New(pv.CreatedAt)}, nil //nolint:gosec // small
}

func spec(st *structpb.Struct) ([]byte, error) {
	if st == nil {
		return nil, fail(pInvalid, "policy is required")
	}
	b, err := protojson.Marshal(st)
	if err != nil {
		return nil, fail(pInvalid, "policy: "+err.Error())
	}
	return b, nil
}

// CreatePolicy implements PolicyService: a new name needs an admin, a new
// version manage.
func (ps policies) CreatePolicy(ctx context.Context, r *vehov1.CreatePolicyRequest) (*vehov1.Policy, error) {
	t, err := tenant(ctx)
	if err != nil {
		return nil, err
	}
	if !principal(ctx).may(RoleAdmin) {
		if _, err := ps.s.engine.GetPolicy(ctx, t, r.GetName(), 0); err != nil {
			return nil, fail(pPermission, "only an admin creates a policy")
		}
		if err := ps.need(ctx, r.GetName(), engine.PermManage); err != nil {
			return nil, err
		}
	}
	b, err := spec(r.GetPolicy())
	if err != nil {
		return nil, err
	}
	pv, err := ps.s.engine.CreatePolicy(ctx, t, r.GetName(), b)
	if err != nil {
		return nil, err
	}
	return toPolicy(pv)
}

// GetPolicy implements PolicyService.
func (ps policies) GetPolicy(ctx context.Context, r *vehov1.GetPolicyRequest) (*vehov1.Policy, error) {
	t, err := tenant(ctx)
	if err != nil {
		return nil, err
	}
	if err := ps.need(ctx, r.GetName(), reads...); err != nil {
		return nil, err
	}
	pv, err := ps.s.engine.GetPolicy(ctx, t, r.GetName(), int(r.GetVersion()))
	if err != nil {
		return nil, err
	}
	return toPolicy(pv)
}

// ListPolicies implements PolicyService: the policies the caller sees.
func (ps policies) ListPolicies(ctx context.Context, _ *vehov1.ListPoliciesRequest) (*vehov1.ListPoliciesResponse, error) {
	t, err := tenant(ctx)
	if err != nil {
		return nil, err
	}
	all, err := ps.s.engine.ListPolicies(ctx, t)
	if err != nil {
		return nil, err
	}
	out := &vehov1.ListPoliciesResponse{}
	for _, pv := range all {
		if ok, err := ps.allowed(ctx, pv.Name, reads...); err != nil {
			return nil, err
		} else if !ok {
			continue
		}
		p, err := toPolicy(pv)
		if err != nil {
			return nil, err
		}
		out.Policies = append(out.Policies, p)
	}
	return out, nil
}

// DeletePolicy implements PolicyService.
func (ps policies) DeletePolicy(ctx context.Context, r *vehov1.PolicyName) (*vehov1.Empty, error) {
	t, err := tenant(ctx)
	if err != nil {
		return nil, err
	}
	if err := ps.need(ctx, r.GetName(), engine.PermManage); err != nil {
		return nil, err
	}
	return &vehov1.Empty{}, ps.s.engine.DeletePolicy(ctx, t, r.GetName())
}

// ValidatePolicy implements PolicyService.
func (ps policies) ValidatePolicy(ctx context.Context, r *vehov1.ValidatePolicyRequest) (*vehov1.ValidatePolicyResponse, error) {
	t, err := tenant(ctx)
	if err != nil {
		return nil, err
	}
	b, err := spec(r.GetPolicy())
	if err != nil {
		return nil, err
	}
	err = ps.s.engine.ValidatePolicy(ctx, t, b)
	if pe, ok := errors.AsType[*policy.Error](err); ok {
		return &vehov1.ValidatePolicyResponse{Code: pe.Code, Message: pe.Message}, nil
	}
	if err != nil {
		return nil, err
	}
	return &vehov1.ValidatePolicyResponse{Valid: true}, nil
}

// GetGrants implements PolicyService.
func (ps policies) GetGrants(ctx context.Context, r *vehov1.PolicyName) (*vehov1.Grants, error) {
	t, err := tenant(ctx)
	if err != nil {
		return nil, err
	}
	if err := ps.need(ctx, r.GetName(), engine.PermManage); err != nil {
		return nil, err
	}
	gs, err := ps.s.engine.Grants(ctx, t, r.GetName())
	if err != nil {
		return nil, err
	}
	out := &vehov1.Grants{Name: r.GetName()}
	for _, g := range gs {
		out.Grants = append(out.Grants, &vehov1.Grant{Grantee: g.Grantee, Permission: g.Permission})
	}
	return out, nil
}

// SetGrants implements PolicyService.
func (ps policies) SetGrants(ctx context.Context, r *vehov1.SetGrantsRequest) (*vehov1.Grants, error) {
	t, err := tenant(ctx)
	if err != nil {
		return nil, err
	}
	if err := ps.need(ctx, r.GetName(), engine.PermManage); err != nil {
		return nil, err
	}
	gs := make([]engine.Grant, len(r.GetGrants()))
	for i, g := range r.GetGrants() {
		if g.GetPermission() == engine.PermSubmitInline {
			return nil, fail(pInvalid, "submit_inline is a grant of the tenant, not of a policy")
		}
		gs[i] = engine.Grant{Grantee: g.GetGrantee(), Permission: g.GetPermission()}
	}
	if err := ps.s.engine.SetGrants(ctx, t, r.GetName(), gs); err != nil {
		return nil, err
	}
	return ps.GetGrants(ctx, &vehov1.PolicyName{Name: r.GetName()})
}

// mayInline enforces the tenant's inline_policies (§5.1).
func (ps policies) mayInline(ctx context.Context) error {
	t, err := tenant(ctx)
	if err != nil {
		return err
	}
	mode, err := ps.s.engine.InlinePolicies(ctx, t)
	if err != nil {
		return err
	}
	switch mode {
	case engine.InlineAll:
		return nil
	case engine.InlineGranted:
		if ok, err := ps.allowed(ctx, "", engine.PermSubmitInline); err != nil || ok {
			return err
		}
	}
	return fail(pInlineRefused, "the tenant does not accept this principal's inline policies")
}
