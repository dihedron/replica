package step // SimpleDelayedPrint is a simple workflow task that prints a message after a delay.

import (
	"fmt"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
)

func SimpleDelayedPrint(ctx dbos.DBOSContext, i int) (int, error) {
	dbos.Sleep(ctx, 5*time.Second)
	fmt.Printf("SimpleDelayedPrint task %d completed\n", i)
	return i, nil
}

/*
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

	// launch the DBOS Runtime and ensure graceful shutdown
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
*/
