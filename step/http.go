package step

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/dihedron/replica/workflow"
)

func FetchFromURL(ctx dbos.DBOSContext, url string, opts ...dbos.StepOption) ([]byte, error) {
	slog.Debug("Starting FetchFromURL", "url", url, "options", opts)
	result, err := dbos.RunAsStep(
		ctx,
		func(ctx context.Context) (workflow.Result[[]byte], error) {
			slog.Debug("Fetching from URL", "url", url)
			resp, err := http.Get(url)

			// SCENARIO A: Transient Network Error: return error to trigger retry
			if err != nil {
				slog.Error("Error sending GET request", "url", url, "error", err)
				return workflow.Result[[]byte]{}, fmt.Errorf("network failure: %w", err)
			}
			defer resp.Body.Close()

			slog.Debug("GET request successfully submitted", "url", url)

			// SCENARIO B: Transient Server Error (5xx): return error to trigger retry
			if resp.StatusCode >= 500 {
				slog.Error("internal server error", "url", url, "code", resp.StatusCode, "status", resp.Status)
				return workflow.Result[[]byte]{}, fmt.Errorf("temporary server error (HTTP %d)", resp.StatusCode)
			}

			// SCENARIO C: Fatal Client Error (4xx): return nil tostop retrying, pass real error in Result
			if resp.StatusCode >= 400 && resp.StatusCode < 500 {
				slog.Error("invalid resource requested", "url", url, "code", resp.StatusCode, "status", resp.Status)
				return workflow.Result[[]byte]{
					Value: nil,
					Error: fmt.Errorf("invalid request: HTTP %d", resp.StatusCode),
				}, nil
			}
			slog.Debug("response successfully received")
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				slog.Error("error reading response data", "url", url, "error", err)
				return workflow.Result[[]byte]{}, fmt.Errorf("error reading response data (%v)", err)
			}

			slog.Debug("all data successfully read", "count", len(body), "data", body)
			return workflow.Result[[]byte]{Value: body}, nil
		},
		opts...,
	)

	// SCENARIOS A & B: retries exhausted
	if result.IsTransientError(err) {
		// run compensation logic here
		return nil, fmt.Errorf("workflow aborted: external API is completely down: %w", err)
	}

	if result.IsFatalError(err) {
		// business logic error
		return nil, fmt.Errorf("workflow aborted due to client error: %v", result.Error)
	}

	return result.Value, nil
}

// // FetchFromURL retrieves the data at the given URL by running a GET request.
// func FetchFromURL(url string) dbos.Step[Result[[]byte]] {
// 	// the closure captures the 'url' from the defining function
// 	return func(ctx context.Context) (Result[[]byte], error) {
// 		resp, err := http.Get(url)

// 		// SCENARIO A: Transient Network Error: return error to trigger retry
// 		if err != nil {
// 			slog.Error("error sending GET request", "url", url, "error", err)
// 			returretrieves the data at the given URL by running a GET request.
// func FetchFromURL(url string) dbos.Step[Result[[]byte]] {
// 	// the closure captures the 'url' from the defining function
// 	return func(ctx context.Context) (Result[[]byte], error) {
// 		resp, err := http.Get(url)

// 		// SCENARIO A: Transient Network Error: return error to trigger retry
// 		if err != nil {
// 			slog.Error("error sending GET request", "url", url, "error", err)
// 			return Result[[]byte]{}, fmt.Errorf("network failure: %w", err)
// 		}
// 		defer resp.Body.Close()

// 		slog.Debug("GET request successfully submitted", "url", url)

// 		// SCENARIO B: Transient Server Error (5xx): return error to trigger retry
// 		if resp.StatusCode >= 500 {
// 			slog.Error("internal server error", "url", url, "code", resp.StatusCode, "status", resp.Status)
// 			return Result[[]byte]{}, fmt.Errorf("temporary server error (HTTP %d)", resp.StatusCode)
// 		}

// 		// SCENARIO C: Fatal Client Error (4xx): return nil tostop retrying, pass real error in Result
// 		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
// 			slog.Error("invalid resource requested", "url", url, "code", resp.StatusCode, "status", resp.Status)
// 			return Result[[]byte]{
// 				Data:  nil,
// 				Error: fmt.Errorf("invalid request: HTTP %d", resp.StatusCode),
// 			}, nil
// 		}c which
// 		slog.Debug("response successfully received")
// 		body, err := io.ReadAll(resp.Body)
// 		if err != nil {
// 			slog.Error("error reading response data", "url", url, "error", err)
// 			return Result[[]byte]{}, fmt.Errorf("error reading response data (%v)", err)
// 		}

// 		slog.Debug("all data successfully read", "count", len(body), "data", body)
// 		return Result[[]byte]{Data: body}, nil
// 	}
// }
//n Result[[]byte]{}, fmt.Errorf("network failure: %w", err)
// 		}
// 		defer resp.Body.Close()

// 		slog.Debug("GET request successfully submitted", "url", url)

// 		// SCENARIO B: Transient Server Error (5xx): return error to trigger retry
// 		if resp.StatusCode >= 500 {
// 			slog.Error("internal server error", "url", url, "code", resp.StatusCode, "status", resp.Status)
// 			return Result[[]byte]{}, fmt.Errorf("temporary server error (HTTP %d)", resp.StatusCode)
// 		}

// 		// SCENARIO C: Fatal Client Error (4xx): return nil tostop retrying, pass real error in Result
// 		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
// 			slog.Error("invalid resource requested", "url", url, "code", resp.StatusCode, "status", resp.Status)
// 			return Result[[]byte]{
// 				Data:  nil,
// 				Error: fmt.Errorf("invalid request: HTTP %d", resp.StatusCode),
// 			}, nil
// 		}
// 		slog.Debug("response successfully received")
// 		body, err := io.ReadAll(resp.Body)
// 		if err != nil {
// 			slog.Error("error reading response data", "url", url, "error", err)
// 			return Result[[]byte]{}, fmt.Errorf("error reading response data (%v)", err)
// 		}

// 		slog.Debug("all data successfully read", "count", len(body), "data", body)
// 		return Result[[]byte]{Data: body}, nil
// 	}
// }
