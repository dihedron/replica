package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/dihedron/replica/step"
)

type Command struct {
	DatabaseURL string `short:"d" long:"database-url" description:"The URL of the database holding durable execution data." optional:"yes" env:"REPLICA_DATABASE_URL" default:"postgres://postgres:dbos@localhost:5432/postgres"`
	Application string `short:"a" long:"application-name" description:"The name of the application workflow." optional:"yes" env:"REPLICA_APPLICATION" default:"my-durable-app"`
}

// Execute executes the command.
func (cmd *Command) Execute(args []string) error {

	name := "fetch workflow"
	workflow := step.FetchWorkflow
	input := step.FetchTask{Username: "[USER]", Password: "[PASSWORD]", URLs: []string{"https://www.google.com", "https://www.ansa.it", "https://www.repubblica.it"}}

	// name := "user registration saga"
	// workflow := step.UserRegistrationSaga
	// input := "[EMAIL_ADDRESS]"

	slog.Debug("starting workflow", "name", name)

	// point DBOS to your Postgres connection string
	dbURL := os.Getenv("DBOS_SYSTEM_DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://postgres:dbos@localhost:5432/postgres" // Default for `dbos postgres start`
	}
	runtime, err := dbos.NewDBOSContext(context.Background(), dbos.Config{
		AppName:     name,
		DatabaseURL: dbURL,
	})
	if err != nil {
		slog.Error("Failed to initialize DBOS context", "error", err)
		return err
	}

	// ensure graceful shutdown
	defer runtime.Shutdown(5 * time.Second)
	slog.Info("DBOS Context created successfully")

	// register workflows
	dbos.RegisterWorkflow(runtime, workflow)

	// Launch the DBOS Runtime
	if err := runtime.Launch(); err != nil {
		slog.Error("Failed to launch DBOS runtime", "error", err)
		return err
	}
	slog.Info("DBOS Runtime launched successfully")

	// 6. Execute the Workflow
	slog.Info("Triggering durable workflow execution...")

	// RunWorkflow returns a handle you can use to check status or get results
	handle, err := dbos.RunWorkflow(runtime, workflow, input)
	if err != nil {
		slog.Error("Failed to start workflow", "error", err)
		return err
	}

	// Wait for the workflow to complete and fetch the result
	finalResult, err := handle.GetResult()
	if err != nil {
		slog.Error("Workflow execution failed", "error", err)
		return err
	}
	fmt.Printf("result: %s\n", finalResult)

	slog.Info("Workflow completed successfully", "result", finalResult)
	return nil
}
