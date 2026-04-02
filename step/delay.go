package step

import (
	"context"
	"fmt"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
)

// DelayedPrint is a step that prints a message one or more times after a delay.
func DelayedPrint(text string, delay time.Duration, iterations int) dbos.Step[Result[string]] {
	// The closure captures 'text', 'delay' and 'count' from the outer workflow scope
	return func(ctx context.Context) (Result[string], error) {
		if iterations <= 0 {
			iterations = 1
		}
		for i := range iterations {
			time.Sleep(delay)
			fmt.Printf("%d: %s\n", i, text)
		}
		return Result[string]{Data: fmt.Sprintf("Completed %d iterations of %s", iterations, text)}, nil
	}
}
