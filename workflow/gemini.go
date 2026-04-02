package workflow // SimpleDelayedPrint is a simple workflow task that prints a message after a delay.

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/dihedron/replica/step"
)

func SimpleDelayedPrint(ctx dbos.DBOSContext, i int) (int, error) {
	dbos.Sleep(ctx, 5*time.Second)
	fmt.Printf("SimpleDelayedPrint task %d completed\n", i)
	return i, nil
}

func ExecuteGeminiWorkflow(dburl string, appl string, args []string) error {
	ctx, err := dbos.NewDBOSContext(context.Background(), dbos.Config{
		AppName:     appl,
		DatabaseURL: dburl,
	})
	if err != nil {
		slog.Error("Failed to initialize DBOS context", "error", err)
		return err
	}

	// register Workflows before launching the runtime
	dbos.RegisterWorkflow(ctx, GeminiWorkflow)

	// launch the DBOS Runtime and esnure graceful shutdown
	if err := dbos.Launch(ctx); err != nil {
		slog.Error("Failed to launch DBOS runtime", "error", err)
		return err
	}
	defer dbos.Shutdown(ctx, 5*time.Second)

	// execute the Workflow
	fmt.Println("Triggering durable workflow execution...")

	// RunWorkflow returns a handle you can use to check status or get results
	handle, err := dbos.RunWorkflow(ctx, GeminiWorkflow, "Initialization Payload")
	if err != nil {
		slog.Error("Failed to start workflow", "error", err)
		return err
	}

	// wait for the workflow to complete and fetch the result
	finalResult, err := handle.GetResult()
	if err != nil {
		slog.Error("Workflow execution failed", "error", err)
		return err
	}

	fmt.Printf("Result: %v\n", finalResult)
	return nil
}

// Workflows orchestrate steps. They take a special dbos.DBOSContext.
func GeminiWorkflow(ctx dbos.DBOSContext, input string) (string, error) {
	fmt.Printf("Starting workflow with input: %s\n", input)

	//
	// STEP 1
	//
	result, err := dbos.RunAsStep(
		ctx,
		step.FetchFromURL("https://www.google.com"),
		dbos.WithStepName("fetchData"),
		dbos.WithStepMaxRetries(3),
	)

	// SCENARIOS A & B: retries exhausted
	if step.IsRetryableError(result, err) {
		// run compensation logic here
		return "", fmt.Errorf("workflow aborted: external API is completely down: %w", err)
	}

	if step.IsFatalError(result, err) {
		// business logic error
		return "", fmt.Errorf("workflow aborted due to client error: %v", result.Error)
	}

	//
	// STEP 2
	//
	result2, err := dbos.RunAsStep(
		ctx,
		step.SendWelcomeEmail("developer", "developer@example.com"),
		dbos.WithStepName("sendEmail"),
		dbos.WithStepMaxRetries(3),
	)

	// SCENARIOS A & B: retries exhausted
	if step.IsRetryableError(result2, err) {
		// run compensation logic here
		return "", fmt.Errorf("workflow aborted: external SMTP server completely down: %w", err)
	}

	if step.IsFatalError(result2, err) {
		// business logic error
		return "", fmt.Errorf("workflow aborted due to email client error: %v", result.Error)
	}

	return fmt.Sprintf("Workflow finished successfully. Data length: %d", len(result.Data)), nil
}
