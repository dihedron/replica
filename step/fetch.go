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

type FetchTask struct {
	Username string
	Password string
	URLs     []string
}

func Fetch(ctx context.Context, url string, username string, password string) (workflow.Result[[]byte], error) {
	slog.Debug("Fetching from URL", "url", url, "username", username, "password", password)
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
}

// Workflows orchestrate steps. They take a special dbos.DBOSContext.
func FetchWorkflow(ctx dbos.DBOSContext, tasks FetchTask) (string, error) {
	slog.Debug("Starting fetch workflow", "username", tasks.Username, "password", tasks.Password)

	for _, url := range tasks.URLs {
		// fetch URL
		result, err := dbos.RunAsStep(
			ctx,
			func(c context.Context) (workflow.Result[[]byte], error) {
				return Fetch(c, url, tasks.Username, tasks.Password)
			},
			dbos.WithStepName(fmt.Sprintf("STEP: Fetch URL %s", url)),
		)
		// SCENARIOS A & B: retries exhausted
		if result.IsTransientError(err) {
			// run compensation logic here
			return "", fmt.Errorf("workflow aborted: external API is completely down: %w", err)
		}

		if result.IsFatalError(err) {
			// business logic error
			return "", fmt.Errorf("workflow aborted due to client error: %v", result.Error)
		}
	}
	return "all URLS fetched", nil
}
