package main

import (
	"errors"
	"fmt"
	"testing"

	"github.com/dihedron/veho/internal/config"
	"github.com/dihedron/veho/internal/store"
)

// STO-C1 to C5: a failure refuses the start unless its check's severity is
// warn; the schema version always refuses.
func TestCheckSeverities(t *testing.T) {
	failures := []error{
		store.ErrNotPrimary, fmt.Errorf("%w (local)", store.ErrSyncCommit), store.ErrNoQuorum,
		fmt.Errorf("%w: 3s", store.ErrClock), fmt.Errorf("STO-C4: %w", store.ErrSchema),
	}
	all := config.Checks{Primary: "refuse", SynchronousCommit: "refuse", Quorum: "refuse", Clock: "refuse"}
	if err := applyChecks(failures, all); err == nil || len(errors.Unwrap(err).(interface{ Unwrap() []error }).Unwrap()) != 5 {
		t.Errorf("all refuse: %v", err)
	}
	warn := config.Checks{Primary: "warn", SynchronousCommit: "warn", Quorum: "warn", Clock: "warn"}
	err := applyChecks(failures, warn)
	if err == nil || !errors.Is(err, store.ErrSchema) || errors.Is(err, store.ErrClock) || errors.Is(err, store.ErrNotPrimary) {
		t.Errorf("all warn: %v, want only the schema", err)
	}
	if err := applyChecks(failures[:4], warn); err != nil {
		t.Errorf("warnings refused the start: %v", err)
	}
}
