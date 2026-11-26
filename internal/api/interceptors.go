package api

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"log/slog"
	"net"
	"strings"
	"uuid"

	"github.com/dihedron/veho/internal/audit"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
)

// roles is the role each method needs (§3); a method missing here is
// refused, so that a new method cannot be served unprotected (T-36).
var roles = map[string]string{
	"/veho.v1.JobService/CreateJob":     RoleOperator,
	"/veho.v1.JobService/GetJob":        RoleViewer,
	"/veho.v1.JobService/ListTransfers": RoleViewer,
	"/veho.v1.JobService/ListJobs":      RoleViewer,
	"/veho.v1.JobService/CancelJob":     RoleOperator,
	"/veho.v1.JobService/RetryJob":      RoleOperator,
	"/veho.v1.JobService/WatchJob":      RoleViewer,
	// the handlers check the grants (§5.1)
	"/veho.v1.PolicyService/CreatePolicy":   RoleViewer,
	"/veho.v1.PolicyService/GetPolicy":      RoleViewer,
	"/veho.v1.PolicyService/ListPolicies":   RoleViewer,
	"/veho.v1.PolicyService/DeletePolicy":   RoleViewer,
	"/veho.v1.PolicyService/ValidatePolicy": RoleViewer,
	"/veho.v1.PolicyService/GetGrants":      RoleViewer,
	"/veho.v1.PolicyService/SetGrants":      RoleViewer,

	"/veho.v1.EndpointService/CreateEndpoint": RoleAdmin,
	"/veho.v1.EndpointService/GetEndpoint":    RoleViewer,
	"/veho.v1.EndpointService/ListEndpoints":  RoleViewer,
	"/veho.v1.EndpointService/UpdateEndpoint": RoleAdmin,
	"/veho.v1.EndpointService/DeleteEndpoint": RoleAdmin,
	"/veho.v1.EndpointService/TestEndpoint":   RoleAdmin,
	// NC-5: what TestEndpoint may see
	"/veho.v1.EndpointService/CheckEndpoint":      RoleAdmin,
	"/veho.v1.EndpointService/ListEndpointChecks": RoleAdmin,
	"/veho.v1.EndpointService/GetEndpointCheck":   RoleAdmin,

	"/veho.v1.SecretService/PutSecret":    RoleAdmin,
	"/veho.v1.SecretService/GetSecret":    RoleAdmin,
	"/veho.v1.SecretService/ListSecrets":  RoleAdmin,
	"/veho.v1.SecretService/DeleteSecret": RoleAdmin,

	"/veho.v1.WebhookService/CreateWebhook":       RoleAdmin,
	"/veho.v1.WebhookService/GetWebhook":          RoleAdmin,
	"/veho.v1.WebhookService/ListWebhooks":        RoleAdmin,
	"/veho.v1.WebhookService/UpdateWebhook":       RoleAdmin,
	"/veho.v1.WebhookService/RotateWebhookSecret": RoleAdmin,
	"/veho.v1.WebhookService/DeleteWebhook":       RoleAdmin,
	"/veho.v1.WebhookService/TestWebhook":         RoleAdmin,

	"/veho.v1.TenantService/CreateTenant": RolePlatformAdmin,
	"/veho.v1.TenantService/GetTenant":    RolePlatformAdmin,
	"/veho.v1.TenantService/ListTenants":  RolePlatformAdmin,
	"/veho.v1.TenantService/UpdateTenant": RolePlatformAdmin,

	"/veho.v1.AdminService/CreateServiceAccount":  roleScopeAdmin,
	"/veho.v1.AdminService/ListServiceAccounts":   roleScopeAdmin,
	"/veho.v1.AdminService/GetServiceAccount":     roleScopeAdmin,
	"/veho.v1.AdminService/DisableServiceAccount": roleScopeAdmin,
	"/veho.v1.AdminService/EnableServiceAccount":  roleScopeAdmin,
	"/veho.v1.AdminService/DeleteServiceAccount":  roleScopeAdmin,
	"/veho.v1.AdminService/CreateKey":             roleScopeAdmin,
	"/veho.v1.AdminService/ListKeys":              roleScopeAdmin,
	"/veho.v1.AdminService/RevokeKey":             roleScopeAdmin,
	"/veho.v1.AdminService/CreateEntitlement":     RoleAdmin,
	"/veho.v1.AdminService/ListEntitlements":      RoleAdmin,
	"/veho.v1.AdminService/DeleteEntitlement":     RoleAdmin,
	"/veho.v1.AdminService/GetSettings":           RoleAdmin,
	"/veho.v1.AdminService/UpdateSettings":        RoleAdmin,
}

// public methods need no credential.
var public = map[string]bool{
	"/grpc.health.v1.Health/Check": true,
	"/grpc.health.v1.Health/Watch": true,
	"/grpc.health.v1.Health/List":  true,
}

type requestIDKey struct{}
type checkKey struct{}

// peerChainMD carries a REST client's certificate chain from the gateway to
// the gRPC server, which believes it only from the in-process pipe (§4.4).
const peerChainMD = "x-veho-peer-chain"

// tenantMD names the tenant of a call (veho-authentication.md §4).
const tenantMD = "x-veho-tenant"

// credential picks the caller's credential (§4.4): a client certificate,
// else the bearer token (an API key, a JWT-SVID or an OIDC token). It
// returns the check that turns it into a principal, which a stream runs
// again to re-authorise its caller.
func (s *Server) credential(ctx context.Context) (func(context.Context) (Principal, error), error) {
	tenant := first(ctx, tenantMD)
	if chain := peerChain(ctx); len(chain) > 0 && s.ids != nil && s.ids.x509 != nil {
		return func(ctx context.Context) (Principal, error) {
			id, err := s.ids.svid(chain)
			if err != nil {
				return Principal{}, err
			}
			return entitle(ctx, s.api, id, tenant)
		}, nil
	}
	token, ok := strings.CutPrefix(first(ctx, "authorization"), "Bearer ")
	switch {
	case !ok:
		return nil, fail(pUnauthenticated, "a credential is required")
	case strings.HasPrefix(token, keyPrefix):
		return func(ctx context.Context) (Principal, error) { return authenticate(ctx, s.api, token, tenant) }, nil
	case s.ids == nil:
		return nil, fail(pUnauthenticated, "invalid credentials")
	}
	return func(ctx context.Context) (Principal, error) {
		id, err := s.ids.bearer(ctx, token)
		if err != nil {
			return Principal{}, err
		}
		return entitle(ctx, s.api, id, tenant)
	}, nil
}

// clientAddr is the client's address: the TCP peer's, or for REST the last
// X-Forwarded-For entry, which the gateway appends and only the pipe from
// it carries (§5.2).
func clientAddr(ctx context.Context) string {
	p, ok := peer.FromContext(ctx)
	if !ok || p.Addr == nil {
		return ""
	}
	if p.Addr.Network() == "bufconn" {
		md, _ := metadata.FromIncomingContext(ctx)
		if v := md.Get("x-forwarded-for"); len(v) > 0 {
			parts := strings.Split(v[len(v)-1], ",")
			return strings.TrimSpace(parts[len(parts)-1])
		}
		return "gateway"
	}
	host, _, err := net.SplitHostPort(p.Addr.String())
	if err != nil {
		return p.Addr.String()
	}
	return host
}

// peerChain is the client's certificate chain: the TLS connection's for a
// gRPC call, the gateway's metadata for a REST one.
func peerChain(ctx context.Context) []*x509.Certificate {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return nil
	}
	if p.Addr != nil && p.Addr.Network() == "bufconn" {
		var chain []*x509.Certificate
		for _, v := range strings.Split(first(ctx, peerChainMD), ",") {
			der, err := base64.StdEncoding.DecodeString(v)
			if err != nil {
				return nil
			}
			c, err := x509.ParseCertificate(der)
			if err != nil {
				return nil
			}
			chain = append(chain, c)
		}
		return chain
	}
	if info, ok := p.AuthInfo.(credentials.TLSInfo); ok {
		return info.State.PeerCertificates
	}
	return nil
}

func requestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// first is the first value of a metadata key.
func first(ctx context.Context, key string) string {
	md, _ := metadata.FromIncomingContext(ctx)
	if v := md.Get(key); len(v) > 0 {
		return v[0]
	}
	return ""
}

// admit runs the chain of §3 for a call of method: request ID,
// authentication, authorisation, audit context. It returns the context of
// the handler, or the error to return.
func (s *Server) admit(ctx context.Context, method string) (context.Context, error) {
	id := first(ctx, "x-request-id")
	if _, err := uuid.Parse(id); err != nil {
		id = uuid.New().String()
	}
	ctx = context.WithValue(ctx, requestIDKey{}, id)
	_ = grpc.SetHeader(ctx, metadata.Pairs("x-request-id", id))
	if public[method] {
		return ctx, nil
	}
	addr := clientAddr(ctx)
	if s.failures.Empty("ip:"+addr, s.failRate) {
		return nil, fail(pRateLimited, "too many refused credentials from this address")
	}
	check, err := s.credential(ctx)
	if err != nil {
		s.failures.Allow("ip:"+addr, s.failRate)
		s.refused.add(refusal{address: addr, kind: "none", reason: "no usable credential"})
		return nil, err
	}
	p, err := check(ctx)
	if err != nil {
		return nil, s.refuse(ctx, id, method, addr, p, err)
	}
	need, known := roles[method]
	if !known || !p.may(need) {
		return nil, fail(pPermission, "not allowed")
	}
	if p.Tenant != nil && !s.calls.Allow("t:"+p.Tenant.String(), s.tenantRate) {
		return nil, fail(pRateLimited, "the tenant's rate limit")
	}
	tenant := ""
	if p.Tenant != nil {
		tenant = p.Tenant.String()
	}
	if !s.calls.Allow("p:"+tenant+"/"+p.Name, s.principalRate) {
		return nil, fail(pRateLimited, "the principal's rate limit")
	}
	ctx = withPrincipal(ctx, p)
	ctx = context.WithValue(ctx, checkKey{}, check)
	ctx = audit.WithRequest(ctx, audit.Request{Principal: p.Name + "@" + tenant, Groups: p.Groups, RequestID: id})
	return ctx, nil
}

// credentialKind names the credential that credential picked, for the audit.
func (s *Server) credentialKind(ctx context.Context) string {
	token, ok := strings.CutPrefix(first(ctx, "authorization"), "Bearer ")
	switch {
	case s.ids != nil && s.ids.x509 != nil && len(peerChain(ctx)) > 0:
		return "certificate"
	case ok && strings.HasPrefix(token, keyPrefix):
		return "api_key"
	}
	return "jwt"
}

// refuse is the answer to a credential that check refused: p has a name only
// when the identity was verified.
func (s *Server) refuse(ctx context.Context, id, method, addr string, p Principal, err error) error {
	var answer error
	switch {
	case errors.Is(err, errTenantRequired): // a mistake, not an attempt
		return fail(pTenantRequired, err.Error())
	case errors.Is(err, errNotEntitled):
		answer = fail(pNotEntitled, err.Error())
	case errors.Is(err, errBadKey):
		answer = fail(pUnauthenticated, "invalid credentials")
	default:
		slog.ErrorContext(ctx, "cannot authenticate", "request_id", id, "error", err)
		return fail(pUnavailable, "authentication is unavailable")
	}
	s.failures.Allow("ip:"+addr, s.failRate)
	s.refused.add(refusal{address: addr, kind: s.credentialKind(ctx), principal: p.Name, reason: err.Error()})
	slog.WarnContext(ctx, "authentication refused", "request_id", id, "method", method, "principal", p.Name, "reason", err.Error())
	return answer
}

func (s *Server) unary(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
	ctx, err := s.admit(ctx, info.FullMethod)
	if err != nil {
		return nil, err
	}
	resp, err := h(ctx, req)
	return resp, toStatus(ctx, err)
}

type stream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s stream) Context() context.Context { return s.ctx }

func (s *Server) stream(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, h grpc.StreamHandler) error {
	ctx, err := s.admit(ss.Context(), info.FullMethod)
	if err != nil {
		return err
	}
	return toStatus(ctx, h(srv, stream{ServerStream: ss, ctx: ctx}))
}

// stillAllowed re-authenticates the caller of a stream (API-11): a key
// revoked meanwhile ends it.
func (s *Server) stillAllowed(ctx context.Context, role string) error {
	check, _ := ctx.Value(checkKey{}).(func(context.Context) (Principal, error))
	if check == nil {
		return fail(pUnauthenticated, "the credential is no longer valid")
	}
	p, err := check(ctx)
	if err != nil || !p.may(role) {
		return fail(pUnauthenticated, "the credential is no longer valid")
	}
	return nil
}
