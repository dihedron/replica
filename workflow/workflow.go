package workflow

import (
	"fmt"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
)

// Workflows orchestrate steps. They take a special dbos.DBOSContext.
func MainWorkflow(ctx dbos.DBOSContext, input string) (string, error) {
	fmt.Printf("Starting workflow with input: %s\n", input)

	// Execute the step durably. We can configure automatic retries here.
	result, err := dbos.RunAsStep(
		ctx,
		FetchURL("https://api.example.com/data"),
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

	return fmt.Sprintf("Workflow finished successfully. Data length: %d", len(result.Data)), nil
}
