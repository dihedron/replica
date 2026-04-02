package workflow

import (
	"log/slog"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
)

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
