package workflow

import (
	"fmt"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
)

// Workflows orchestrate steps. They take a special dbos.DBOSContext.
func MainWorkflow(ctx dbos.DBOSContext, input string) (string, error) {
	fmt.Printf("Starting workflow with input: %s\n", input)

	//
	// STEP 1
	//
	result, err := dbos.RunAsStep(
		ctx,
		FetchURL("https://www.google.com"),
		dbos.WithStepName("fetchData"),
		dbos.WithStepMaxRetries(3),
	)

	// SCENARIOS A & B: retries exhausted
	if IsRetryableError(result, err) {
		// run compensation logic here
		return "", fmt.Errorf("workflow aborted: external API is completely down: %w", err)
	}

	if IsFatalError(result, err) {
		// business logic error
		return "", fmt.Errorf("workflow aborted due to client error: %v", result.Error)
	}

	//
	// STEP 2
	//
	result2, err := dbos.RunAsStep(
		ctx,
		SendWelcomeEmail("developer", "developer@example.com"),
		dbos.WithStepName("sendEmail"),
		dbos.WithStepMaxRetries(3),
	)

	// SCENARIOS A & B: retries exhausted
	if IsRetryableError(result2, err) {
		// run compensation logic here
		return "", fmt.Errorf("workflow aborted: external SMTP server completely down: %w", err)
	}

	if IsFatalError(result2, err) {
		// business logic error
		return "", fmt.Errorf("workflow aborted due to email client error: %v", result.Error)
	}

	return fmt.Sprintf("Workflow finished successfully. Data length: %d", len(result.Data)), nil
}
