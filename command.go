package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/dihedron/replica/workflow"
)

type Command struct {
	DatabaseURL string `short:"d" long:"database-url" description:"The URL of the database holding durable execution data." optional:"yes" env:"REPLICA_DATABASE_URL" default:"postgres://postgres:dbos@localhost:5432/postgres"`
	Application string `short:"a" long:"application-name" description:"The name of the application workflow." optional:"yes" env:"REPLICA_APPLICATION" default:"my-durable-app"`
}

// Execute executes the command.
func (cmd *Command) Execute(args []string) error {

	ctx, err := dbos.NewDBOSContext(context.Background(), dbos.Config{
		AppName:     cmd.Application,
		DatabaseURL: cmd.DatabaseURL,
	})
	if err != nil {
		slog.Error("Failed to initialize DBOS context", "error", err)
		return err
	}

	// register Workflows before launching the runtime
	dbos.RegisterWorkflow(ctx, workflow.MainWorkflow)

	// launch the DBOS Runtime and esnure graceful shutdown
	if err := dbos.Launch(ctx); err != nil {
		slog.Error("Failed to launch DBOS runtime", "error", err)
		return err
	}
	defer dbos.Shutdown(ctx, 5*time.Second)

	// execute the Workflow
	fmt.Println("Triggering durable workflow execution...")

	// RunWorkflow returns a handle you can use to check status or get results
	handle, err := dbos.RunWorkflow(ctx, workflow.MainWorkflow, "Initialization Payload")
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
