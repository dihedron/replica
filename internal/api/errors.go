package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/dihedron/veho/internal/engine"
	"github.com/dihedron/veho/internal/policy"
	"github.com/dihedron/veho/internal/secrets"
	"github.com/dihedron/veho/internal/store"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// problem is an error the API returns (§6).
type problem struct {
	code      codes.Code
	http      int
	reason    string
	retryable bool
}

// classify maps a back end's error to its problem (§6).
func classify(err error) (problem, string) {
	if pe, ok := errors.AsType[*policy.Error](err); ok {
		return problem{codes.InvalidArgument, http.StatusBadRequest, pe.Code, false}, pe.Message
	}
	for _, c := range backend {
		if errors.Is(err, c.target) {
			return c.p, c.target.Error()
		}
	}
	if store.Unavailable(err) {
		return pUnavailable, "the state store is unavailable: try again, or another node"
	}
	return pInternal, "internal error"
}

// backend is the table of §6.
var backend = []struct {
	target error
	p      problem
}{
	{engine.ErrNotFound, problem{codes.NotFound, http.StatusNotFound, "not_found", false}},
	{secrets.ErrNotFound, problem{codes.NotFound, http.StatusNotFound, "not_found", false}},
	{engine.ErrInvalid, problem{codes.InvalidArgument, http.StatusBadRequest, "invalid_request", false}},
	{engine.ErrIdempotencyReuse, problem{codes.AlreadyExists, http.StatusConflict, "idempotency_key_reuse", false}},
	{engine.ErrState, problem{codes.FailedPrecondition, http.StatusConflict, "invalid_state", false}},
	{secrets.ErrInUse, problem{codes.FailedPrecondition, http.StatusConflict, "in_use", false}},
	{engine.ErrInUse, problem{codes.FailedPrecondition, http.StatusConflict, "in_use", false}},
	{engine.ErrConflict, problem{codes.AlreadyExists, http.StatusConflict, "already_exists", false}},
	{secrets.ErrInvalid, problem{codes.InvalidArgument, http.StatusBadRequest, "invalid_request", false}},
	{engine.ErrQuota, problem{codes.ResourceExhausted, http.StatusTooManyRequests, "quota_exceeded", true}},
	{engine.ErrSuspended, problem{codes.PermissionDenied, http.StatusForbidden, "tenant_suspended", false}},
	{engine.ErrFenced, pUnavailable},
}

// httpByReason gives each reason its HTTP status, where the gateway's
// mapping of the gRPC code differs (FailedPrecondition is 409, not 400).
var httpByReason = func() map[string]int {
	m := map[string]int{}
	for _, c := range backend {
		m[c.p.reason] = c.p.http
	}
	for _, p := range []problem{pUnauthenticated, pPermission, pInvalid, pWatches, pNotModified, pUnavailable, pInternal, pInlineRefused, pRateLimited,
		pTenantRequired, pNotEntitled} {
		m[p.reason] = p.http
	}
	return m
}()

// fail builds the status of a problem, with its ErrorInfo (API-12).
func fail(p problem, msg string) error {
	st := status.New(p.code, msg)
	retry := "false"
	if p.retryable {
		retry = "true"
	}
	if d, err := st.WithDetails(&errdetails.ErrorInfo{Reason: p.reason, Domain: "veho", Metadata: map[string]string{"retryable": retry}}); err == nil {
		return d.Err()
	}
	return st.Err()
}

// The problems the interceptors raise themselves.
var (
	pUnauthenticated = problem{codes.Unauthenticated, http.StatusUnauthorized, "unauthenticated", false}
	pPermission      = problem{codes.PermissionDenied, http.StatusForbidden, "permission_denied", false}
	pInvalid         = problem{codes.InvalidArgument, http.StatusBadRequest, "invalid_request", false}
	pWatches         = problem{codes.ResourceExhausted, http.StatusTooManyRequests, "too_many_watches", true}
	pNotModified     = problem{codes.Aborted, http.StatusNotModified, "not_modified", false}
	pUnavailable     = problem{codes.Unavailable, http.StatusServiceUnavailable, "unavailable", true}
	pInternal        = problem{codes.Internal, http.StatusInternalServerError, "internal", true}
	pInlineRefused   = problem{codes.PermissionDenied, http.StatusForbidden, "inline_policy_refused", false}
	pRateLimited     = problem{codes.ResourceExhausted, http.StatusTooManyRequests, "rate_limited", true}
	pTenantRequired  = problem{codes.InvalidArgument, http.StatusBadRequest, "tenant_required", false}
	pNotEntitled     = problem{codes.PermissionDenied, http.StatusForbidden, "not_entitled", false}
)

// toStatus is the status of a handler's error: already a status, it stays;
// otherwise it is classified, and an internal one logged, not returned
// (T-06).
func toStatus(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if _, ok := status.FromError(err); ok {
		return err
	}
	p, msg := classify(err)
	if errors.Is(context.Cause(ctx), errServerTimeout) {
		p, msg = pUnavailable, errServerTimeout.Error()
	}
	if p.code == codes.Internal {
		slog.ErrorContext(ctx, "request failed", "request_id", requestID(ctx), "error", err)
		msg = "internal error (request " + requestID(ctx) + ")"
	}
	return fail(p, msg)
}

// problemJSON writes a gRPC status as RFC 9457 problem+json (API-3), and
// not_modified as a bare 304.
func problemJSON(ctx context.Context, _ *runtime.ServeMux, _ runtime.Marshaler, w http.ResponseWriter, r *http.Request, err error) {
	st := status.Convert(err)
	code, reason, retryable := runtime.HTTPStatusFromCode(st.Code()), "", false
	for _, d := range st.Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok {
			reason, retryable = info.Reason, info.Metadata["retryable"] == "true"
		}
	}
	if c, ok := httpByReason[reason]; ok {
		code = c
	}
	if md, ok := runtime.ServerMetadataFromContext(ctx); ok {
		for _, v := range md.HeaderMD.Get("x-request-id") {
			w.Header().Set("X-Request-Id", v)
		}
	}
	if code == http.StatusNotModified {
		w.WriteHeader(code)
		return
	}
	if code == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", "1")
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type": "about:blank", "title": http.StatusText(code), "status": code, "detail": st.Message(),
		"code": reason, "retryable": retryable, "instance": r.URL.Path,
	})
}
