package api

import (
	"context"
	"fmt"
	"testing"
	"time"

	vehov1 "github.com/dihedron/veho/api/veho/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// call runs a handler that waits for the state store until its context ends,
// as a node cut off from the database does, through the deadline interceptor.
func call(ctx context.Context, req any) error {
	s := &Server{}
	_, err := s.deadline(ctx, req, &grpc.UnaryServerInfo{}, func(ctx context.Context, _ any) (any, error) {
		<-ctx.Done()
		return nil, toStatus(ctx, fmt.Errorf("engine: submit: %w", ctx.Err()))
	})
	return err
}

// AC-6: a call the state store does not answer within the server's own limit
// is unavailable (503, retryable), not left to the client's deadline; a
// shorter client deadline still answers DeadlineExceeded.
func TestServerTimeout(t *testing.T) {
	defer func(d time.Duration) { defaultCall = d }(defaultCall)
	defaultCall = 50 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := call(ctx, &vehov1.CreateJobRequest{}); status.Code(err) != codes.Unavailable {
		t.Errorf("server limit: %v, want Unavailable", err)
	}

	short, cancel2 := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel2()
	if err := call(short, &vehov1.CreateJobRequest{}); status.Code(err) == codes.Unavailable {
		t.Errorf("client deadline: %v, want the client's own deadline", err)
	}

	// a long poll gets its wait on top of the limit
	start := time.Now()
	_ = call(ctx, &vehov1.GetJobRequest{Wait: 1})
	if d := time.Since(start); d < time.Second {
		t.Errorf("GetJob with wait 1s cut after %v", d)
	}
}
