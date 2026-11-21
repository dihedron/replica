package api

import (
	"context"
	"log/slog"
	"strings"
	"uuid"

	"github.com/dihedron/veho/internal/audit"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
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
}

// public methods need no credential.
var public = map[string]bool{
	"/grpc.health.v1.Health/Check": true,
	"/grpc.health.v1.Health/Watch": true,
	"/grpc.health.v1.Health/List":  true,
}

type requestIDKey struct{}
type tokenKey struct{}

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
	token, ok := strings.CutPrefix(first(ctx, "authorization"), "Bearer ")
	if !ok {
		return nil, fail(pUnauthenticated, "a credential is required")
	}
	p, err := authenticate(ctx, s.api, token)
	if err != nil {
		if err != errBadKey {
			slog.ErrorContext(ctx, "cannot authenticate", "request_id", id, "error", err)
			return nil, fail(pUnavailable, "authentication is unavailable")
		}
		slog.WarnContext(ctx, "authentication refused", "request_id", id, "method", method)
		return nil, fail(pUnauthenticated, "invalid credentials")
	}
	need, known := roles[method]
	if !known || !p.may(need) {
		return nil, fail(pPermission, "not allowed")
	}
	tenant := ""
	if p.Tenant != nil {
		tenant = p.Tenant.String()
	}
	ctx = withPrincipal(ctx, p)
	ctx = context.WithValue(ctx, tokenKey{}, token)
	ctx = audit.WithRequest(ctx, audit.Request{Principal: p.Name + "@" + tenant, RequestID: id})
	return ctx, nil
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
	token, _ := ctx.Value(tokenKey{}).(string)
	p, err := authenticate(ctx, s.api, token)
	if err != nil || !p.may(role) {
		return fail(pUnauthenticated, "the credential is no longer valid")
	}
	return nil
}
