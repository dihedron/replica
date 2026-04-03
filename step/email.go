package step

import (
	"context"
	"fmt"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/dihedron/replica/workflow"
)

func SendWelcomeEmail(ctx dbos.DBOSContext, name string, email string, opts ...dbos.StepOption) (string, error) {
	result, err := dbos.RunAsStep(
		ctx,

		// The closure captures 'name' and 'email' from the outer workflow scope
		func(ctx context.Context) (workflow.Result[string], error) {
			// ... simulate an external API call ...
			fmt.Printf("Sending email to %s (ID: %s)...\n", email, name)
			return workflow.Result[string]{Value: "Email sent successfully"}, nil
		},
		opts...,
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

	return result.Value, nil
}
