package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"
	"uuid"

	vehov1 "github.com/dihedron/veho/api/veho/v1"
	"github.com/dihedron/veho/internal/engine"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// tenant is the caller's tenant; a platform administrator has none and
// cannot act on jobs.
func tenant(ctx context.Context) (uuid.UUID, error) {
	p := principal(ctx)
	if p.Tenant == nil {
		return uuid.UUID{}, fail(pPermission, "jobs belong to tenants")
	}
	return *p.Tenant, nil
}

// jobID parses a job ID; a malformed one is as missing as another tenant's.
func jobID(s string) (uuid.UUID, error) {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.UUID{}, engine.ErrNotFound
	}
	return id, nil
}

// accepted makes the REST answer 202 with the job's Location.
func accepted(ctx context.Context, id string) {
	_ = grpc.SetHeader(ctx, metadata.Pairs("x-http-code", "202", "location", "/v1/jobs/"+id))
}

// CreateJob implements JobService.
func (s *Server) CreateJob(ctx context.Context, r *vehov1.CreateJobRequest) (*vehov1.Job, error) {
	t, err := tenant(ctx)
	if err != nil {
		return nil, err
	}
	if r.GetSource().GetEndpoint() == "" || r.GetSource().GetPath() == "" || (r.GetPolicy() == nil) == (r.GetPolicyRef() == nil) {
		return nil, fail(pInvalid, "source.endpoint, source.path and one of policy and policy_ref are required")
	}
	var (
		pol []byte
		ref *engine.PolicyRef
	)
	if pr := r.GetPolicyRef(); pr != nil {
		// submitting under a policy needs submit on it (SEC-21)
		if err := s.pol.need(ctx, pr.GetName(), engine.PermSubmit); err != nil {
			return nil, err
		}
		ref = &engine.PolicyRef{Name: pr.GetName(), Version: int(pr.GetVersion())}
	} else {
		if err := s.pol.mayInline(ctx); err != nil {
			return nil, err
		}
		var err error
		if pol, err = protojson.Marshal(r.GetPolicy()); err != nil {
			return nil, fail(pInvalid, "policy: "+err.Error())
		}
	}
	key := r.GetIdempotencyKey()
	if key == "" {
		key = first(ctx, "idempotency-key")
	}
	id, _, err := s.engine.Submit(ctx, t, engine.Request{IdempotencyKey: key, SourceEndpoint: r.GetSource().GetEndpoint(),
		SourcePath: r.GetSource().GetPath(), Policy: pol, PolicyRef: ref, Labels: r.GetLabels()})
	if err != nil {
		return nil, err
	}
	accepted(ctx, id.String())
	return s.job(ctx, t, id)
}

func (s *Server) job(ctx context.Context, t, id uuid.UUID) (*vehov1.Job, error) {
	st, err := s.engine.Status(ctx, t, id)
	if err != nil {
		return nil, err
	}
	return toJob(st), nil
}

// etag is the hash of a job's view.
func etag(j *vehov1.Job) string {
	b, _ := proto.MarshalOptions{Deterministic: true}.Marshal(j)
	s := sha256.Sum256(b)
	return `"` + hex.EncodeToString(s[:12]) + `"`
}

// GetJob implements JobService, with the REST long poll (§5).
func (s *Server) GetJob(ctx context.Context, r *vehov1.GetJobRequest) (*vehov1.Job, error) {
	t, err := tenant(ctx)
	if err != nil {
		return nil, err
	}
	id, err := jobID(r.GetId())
	if err != nil {
		return nil, err
	}
	wait := min(time.Duration(r.GetWait())*time.Second, maxWait)
	until := time.Now().Add(wait)
	known := first(ctx, "if-none-match")
	for {
		j, err := s.job(ctx, t, id)
		if err != nil {
			return nil, err
		}
		tag := etag(j)
		if known != tag || wait <= 0 {
			_ = grpc.SetHeader(ctx, metadata.Pairs("etag", tag))
			return j, nil
		}
		if time.Now().After(until) {
			_ = grpc.SetHeader(ctx, metadata.Pairs("etag", tag))
			return nil, fail(pNotModified, "not modified")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(watchInterval):
		}
	}
}

// ListTransfers implements JobService.
func (s *Server) ListTransfers(ctx context.Context, r *vehov1.JobRef) (*vehov1.ListTransfersResponse, error) {
	j, err := s.GetJob(ctx, &vehov1.GetJobRequest{Id: r.GetId()})
	if err != nil {
		return nil, err
	}
	return &vehov1.ListTransfersResponse{Transfers: j.GetTransfers()}, nil
}

// ListJobs implements JobService.
func (s *Server) ListJobs(ctx context.Context, r *vehov1.ListJobsRequest) (*vehov1.ListJobsResponse, error) {
	t, err := tenant(ctx)
	if err != nil {
		return nil, err
	}
	f := engine.Filter{State: r.GetState(), SourceEndpoint: r.GetSourceEndpoint(), Labels: r.GetLabel(), Limit: int(r.GetLimit()), Cursor: r.GetCursor()}
	if r.GetSince() != nil {
		f.Since = r.GetSince().AsTime()
	}
	jobs, next, err := s.engine.ListJobs(ctx, t, f)
	if err != nil {
		return nil, err
	}
	out := &vehov1.ListJobsResponse{NextCursor: next}
	for _, j := range jobs {
		out.Jobs = append(out.Jobs, toJob(j))
	}
	return out, nil
}

// CancelJob implements JobService.
func (s *Server) CancelJob(ctx context.Context, r *vehov1.JobRef) (*vehov1.Job, error) {
	return s.control(ctx, r, s.engine.Cancel)
}

// RetryJob implements JobService.
func (s *Server) RetryJob(ctx context.Context, r *vehov1.JobRef) (*vehov1.Job, error) {
	return s.control(ctx, r, s.engine.Retry)
}

func (s *Server) control(ctx context.Context, r *vehov1.JobRef, fn func(context.Context, uuid.UUID, uuid.UUID) error) (*vehov1.Job, error) {
	t, err := tenant(ctx)
	if err != nil {
		return nil, err
	}
	id, err := jobID(r.GetId())
	if err != nil {
		return nil, err
	}
	if err := fn(ctx, t, id); err != nil {
		return nil, err
	}
	accepted(ctx, id.String())
	return s.job(ctx, t, id)
}

// WatchJob implements JobService: the job each time it changes, until it
// settles; the caller is re-authorised every minute (API-11).
func (s *Server) WatchJob(r *vehov1.JobRef, ss grpc.ServerStreamingServer[vehov1.Job]) error {
	ctx := ss.Context()
	if s.watches.Add(1) > s.max {
		s.watches.Add(-1)
		return fail(pWatches, "too many watches on this node")
	}
	defer s.watches.Add(-1)
	t, err := tenant(ctx)
	if err != nil {
		return err
	}
	id, err := jobID(r.GetId())
	if err != nil {
		return err
	}
	sent, checked := "", time.Now()
	for {
		if time.Since(checked) > s.reauth {
			if err := s.stillAllowed(ctx, roles["/veho.v1.JobService/WatchJob"]); err != nil {
				return err
			}
			checked = time.Now()
		}
		j, err := s.job(ctx, t, id)
		if err != nil {
			return err
		}
		if tag := etag(j); tag != sent {
			if err := ss.Send(j); err != nil {
				return err
			}
			sent = tag
		}
		if j.GetSettledAt() != nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(watchInterval):
		}
	}
}

func ts(t *time.Time) *timestamppb.Timestamp {
	if t == nil {
		return nil
	}
	return timestamppb.New(*t)
}

// toProblem turns a last_error ({class, message}) into a Problem.
func toProblem(raw json.RawMessage) *vehov1.Problem {
	var e struct{ Class, Message string }
	if len(raw) == 0 || json.Unmarshal(raw, &e) != nil || (e.Class == "" && e.Message == "") {
		return nil
	}
	return &vehov1.Problem{Code: strings.ToLower(e.Class), Message: e.Message}
}

func toJob(s engine.JobStatus) *vehov1.Job {
	j := &vehov1.Job{
		Id: s.ID.String(), State: s.State, Labels: s.Labels,
		Source:    &vehov1.Source{Endpoint: s.SourceEndpoint, Path: s.SourcePath, Version: s.SourceVersion},
		CreatedAt: timestamppb.New(s.CreatedAt), UpdatedAt: timestamppb.New(s.UpdatedAt), DeadlineAt: timestamppb.New(s.DeadlineAt),
		SatisfiedAt: ts(s.SatisfiedAt), CompletedAt: ts(s.CompletedAt), SettledAt: ts(s.SettledAt),
		Copies:    &vehov1.Copies{Done: int32(s.Copies), Total: int32(s.Destinations)}, //nolint:gosec // small counts
		LastError: toProblem(s.LastError),
	}
	if s.SourceSize != nil {
		j.Source.Size = *s.SourceSize
	}
	if s.PolicyName != nil && s.PolicyVersion != nil {
		j.PolicyRef = &vehov1.PolicyRef{Name: *s.PolicyName, Version: int32(*s.PolicyVersion)} //nolint:gosec // small
	}
	var done, total int64
	for _, t := range s.Transfers {
		tr := &vehov1.Transfer{Id: t.ID.String(), Endpoint: t.Endpoint, Path: t.Path, Position: t.Position, State: t.State,
			BytesDone: t.BytesDone, Attempt: int32(t.Attempt), LastError: toProblem(t.LastError), FinishedAt: ts(t.FinishedAt)} //nolint:gosec // small counts
		if t.BytesTotal != nil {
			tr.BytesTotal = *t.BytesTotal
		}
		done, total = done+tr.BytesDone, total+tr.BytesTotal
		j.Transfers = append(j.Transfers, tr)
	}
	if total > 0 {
		j.Progress = float64(done) / float64(total)
	}
	return j
}
