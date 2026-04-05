package workflow

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
)

// ctx is the global DBOS execution context.
var (
	ctx     dbos.DBOSContext
	started bool
)

// Setup established the DBOS operating context; it must be called
// prior to any other DBOS-related operation.
func Setup(application string, dburl string) error {
	var err error
	slog.Debug("Setting up DBOS context")
	ctx, err = dbos.NewDBOSContext(context.Background(), dbos.Config{
		AppName:     application,
		DatabaseURL: dburl,
	})
	if err != nil {
		slog.Error("Failed to initialize DBOS context", "error", err)
		return err
	}
	slog.Info("DBOS context successfully established")
	return nil
}

// StartRuntime launches the DBOS runtime; it should be launched
// after registering the workflows.
func StartRuntime() error {
	slog.Debug("Launching the DBOS runtime...")
	if err := dbos.Launch(ctx); err != nil {
		slog.Error("Failed to launch DBOS runtime", "error", err)
		return err
	}
	slog.Info("DBOS runtime successfully launched")
	started = true
	return nil
}

// Cleanup cleans up the DBOS runtime; it should be called
// prior to the application exiting.
func Cleanup(timeout time.Duration) {
	slog.Debug("Shutting down DBOS runtime...")
	dbos.Shutdown(ctx, timeout)
	slog.Info("DBOS runtime successfully stopped")
}

// Workflow is a wrapper around a DBOS workflow.
type Workflow[I any, O any] struct {
	workflow dbos.Workflow[I, O]
	options  []dbos.WorkflowRegistrationOption
}

// New creates a new workflow and registers it with
// the DBOS runtime.
func New[I any, O any](workflow dbos.Workflow[I, O], opts ...dbos.WorkflowRegistrationOption) (*Workflow[I, O], error) {
	if started {
		slog.Error("Runtime already started: you should register new workflows BEFORE starting it!!!")
		return nil, fmt.Errorf("runtime already started")
	}
	slog.Debug("Registering workflow...") // register Workflows before launching the runtime
	wf := &Workflow[I, O]{
		workflow: workflow,
		options:  opts,
	}
	dbos.RegisterWorkflow(ctx, wf.workflow, wf.options...)
	slog.Debug("Workflow successfully registered")
	return wf, nil
}

// Run runs the workflow.
func (w *Workflow[I, O]) Run(input I) (O, error) {
	if !started {
		slog.Error("Runtime not started: you should start it before running any workflow!!!")
		return *new(O), fmt.Errorf("runtime not started")
	}
	slog.Debug("Running workflow", "workflow", w.workflow, "input", input, "options", w.options)
	r, err := w.workflow(ctx, input)
	if err != nil {
		slog.Error("workflow failed", "error", err)
		return r, err
	}
	slog.Info("workflow completed successfully", "result", r)
	return r, nil
}

// Result is a wrapper around the result of a step; it provides a way to
// distinguish between transient and fatal errors.
type Result[T any] struct {
	Error error
	Value T
}

// IsTransientError returns whether the Step error is due to a temporary
// condition which could be overcome by resubmitting the step later but
// needs to be retried.
func (r *Result[T]) IsTransientError(err error) bool {
	return err != nil && r.Error == nil
}

// IsFatalError returns whether the Step error is a permanent
// failure which cannot be overcome by resubmitting the step.
func (r *Result[T]) IsFatalError(err error) bool {
	return err == nil && r.Error != nil
}

/*
func Enqueue[T any](ctx dbos.DBOSContext, queue dbos.WorkflowQueue, workflows []func(dbos.DBOSContext, T) (T, error), input T) ([]T, error) {
	slog.Debug("Enqueuing workflows", "queue", queue.Name, "count", len(workflows))
	handles := make([]dbos.WorkflowHandle[T], len(workflows))
	for i, workflow := range workflows {
		slog.Debug("Enqueuing workflow", "workflow", workflow, "input", input)
		handle, err := dbos.RunWorkflow(ctx, workflow, input, dbos.WithQueue(queue.Name))
		if err != nil {
			return nil, err
		}
		handles[i] = handle
	}
	results := make([]T, len(workflows))
	for i, handle := range handles {
		slog.Debug("Waiting for workflow to complete", "handle", handle)
		result, err := handle.GetResult()
		if err != nil {
			return nil, err
		}
		results[i] = result
	}
	slog.Debug("Successfully completed workflows", "count", len(results))
	return results, nil
}
*/
