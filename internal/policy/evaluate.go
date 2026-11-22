package policy

import (
	"fmt"

	"github.com/dihedron/veho/internal/topology"
)

// State is the state of a transfer (.specs/veho-job-lifecycle.md §2).
type State string

// The states of a transfer.
const (
	Pending   State = "PENDING"
	Copying   State = "COPYING"
	Verifying State = "VERIFYING"
	RetryWait State = "RETRY_WAIT"
	Succeeded State = "SUCCEEDED"
	Failed    State = "FAILED"
	Cancelled State = "CANCELLED"
)

// Terminal reports whether the transfer has ended.
func (s State) Terminal() bool { return s == Succeeded || s == Failed || s == Cancelled }

func (s State) valid() bool {
	switch s {
	case Pending, Copying, Verifying, RetryWait, Succeeded, Failed, Cancelled:
		return true
	}
	return false
}

// Transfer is what the evaluation needs to know of a transfer: its state and
// the position of its destination, as snapshotted at acceptance.
type Transfer struct {
	State    State
	Position topology.Position
}

// Outcome is the verdict on the completion rule.
type Outcome int

// The outcomes.
const (
	// Open: not met yet, and it still can be.
	Open Outcome = iota
	// Satisfied: the policy minimum is met.
	Satisfied
	// Unsatisfiable: it can no longer be met, even if every transfer that is
	// not terminal succeeds.
	Unsatisfiable
)

func (o Outcome) String() string {
	switch o {
	case Satisfied:
		return "satisfied"
	case Unsatisfiable:
		return "unsatisfiable"
	}
	return "open"
}

// Verdict is the result of Evaluate: the outcome and the milestones of
// .specs/veho-distribution-policy.md §5.1.
type Verdict struct {
	Outcome  Outcome
	Copies   int  // transfers SUCCEEDED
	Complete bool // every transfer SUCCEEDED (all N have a copy)
	Settled  bool // every transfer is terminal
	// ByLevel counts the verified copies per location, per level.
	ByLevel map[string]map[string]int
}

// Evaluate decides the completion rule over the transfers of a job. It is a
// pure function. A Completion.MinCopies of 0 means all the transfers.
//
// The rule is monotone (more copies never break a statement), so "can still be
// met" is exact: it holds iff it holds with every transfer that is not terminal
// succeeding.
func Evaluate(p Policy, ts []Transfer) (Verdict, error) {
	v := Verdict{ByLevel: map[string]map[string]int{}, Complete: len(ts) > 0, Settled: true}
	possible := make([]Transfer, 0, len(ts)) // succeeded or not terminal
	succeeded := make([]Transfer, 0, len(ts))
	for i, t := range ts {
		if !t.State.valid() {
			return Verdict{}, fmt.Errorf("policy: transfer %d has unknown state %q", i, t.State)
		}
		if t.State != Succeeded {
			v.Complete = false
		}
		if !t.State.Terminal() {
			v.Settled = false
		}
		if t.State == Succeeded || !t.State.Terminal() {
			possible = append(possible, t)
		}
		if t.State == Succeeded {
			succeeded = append(succeeded, t)
			for l, id := range t.Position {
				if v.ByLevel[l] == nil {
					v.ByLevel[l] = map[string]int{}
				}
				v.ByLevel[l][id]++
			}
		}
	}
	v.Copies = len(succeeded)
	min := p.Completion.MinCopies
	if min == 0 {
		min = len(ts)
	}
	switch {
	case meets(p.Completion, min, succeeded):
		v.Outcome = Satisfied
	case !meets(p.Completion, min, possible):
		v.Outcome = Unsatisfiable
	}
	return v, nil
}

// meets reports whether the transfers, taken as verified copies, satisfy the
// completion rule.
func meets(c Completion, min int, ts []Transfer) bool {
	if len(ts) < min {
		return false
	}
	for _, s := range c.Spread {
		seen := map[string]struct{}{}
		for _, t := range ts {
			if id, ok := t.Position[s.Level]; ok {
				seen[id] = struct{}{}
			}
		}
		if len(seen) < s.MinDistinct {
			return false
		}
	}
	return true
}
