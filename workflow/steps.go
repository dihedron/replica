package workflow

import (
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
)

// FetchFromURL retrieves the data at the given URL by running a GET request.
func FetchURL(url string) dbos.Step[Result[[]byte]] {
	// the closure captures the 'url' from the defining function
	return func(ctx context.Context) (Result[[]byte], error) {
		resp, err := http.Get(url)

		// SCENARIO A: Transient Network Error: return error to trigger retry
		if err != nil {
			return Result[[]byte]{}, fmt.Errorf("network failure: %w", err)
		}
		defer resp.Body.Close()

		// SCENARIO B: Transient Server Error (5xx): return error to trigger retry
		if resp.StatusCode >= 500 {
			return Result[[]byte]{}, fmt.Errorf("temporary server error (HTTP %d)", resp.StatusCode)
		}

		// SCENARIO C: Fatal Client Error (4xx): return nil tostop retrying, pass real error in Result
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			return Result[[]byte]{
				Data:  nil,
				Error: fmt.Errorf("invalid request: HTTP %d", resp.StatusCode),
			}, nil
		}

		body, _ := io.ReadAll(resp.Body)
		return Result[[]byte]{Data: body}, nil
	}
}

func SendWelcomeEmail(name string, email string) dbos.Step[Result[string]] {
	// The closure captures 'name' and 'email' from the outer workflow scope
	return func(ctx context.Context) (Result[string], error) {
		// ... simulate an external API call ...
		fmt.Printf("Sending email to %s (ID: %s)...\n", email, name)
		return Result[string]{Data: "Email sent successfully"}, nil
	}
}
