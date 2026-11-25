package policy

import (
	"fmt"
	"time"

	"github.com/dihedron/veho/internal/topology"
)

// Codes of the errors Validate returns; they are the stable codes of the API
// (.specs/veho-distribution-policy.md §4).
const (
	CodeInvalidPolicy        = "invalid_policy"
	CodeTooManyDestinations  = "too_many_destinations"
	CodeUnknownEndpoint      = "unknown_endpoint"
	CodeDuplicateDestination = "duplicate_destination"
	CodeDestinationIsSource  = "destination_is_source"
	CodeUnsatisfiable        = "unsatisfiable_policy"
	// CodeInvalidEndpoint: the source cannot be read, or a destination
	// written, by its driver on this node (veho-sources.md §2, decision 1).
	CodeInvalidEndpoint = "invalid_endpoint"
)

// Error is a policy the API refuses (HTTP 400, with its Code).
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func refuse(code, format string, args ...any) error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Ref is an object on an endpoint.
type Ref struct {
	Endpoint string
	Path     string
}

// Env is what a policy is validated against.
type Env struct {
	Topology *topology.Topology
	// Resolve returns the position of a destination endpoint of the submitting
	// tenant; an endpoint that does not exist and one that belongs to another
	// tenant are both reported as not found (INV-4).
	Resolve func(endpoint string) (topology.Position, bool)
	// Source is the source of the job; no destination may be the same object.
	Source Ref
	// MaxDestinations is the tenant's limit on N.
	MaxDestinations int
	// MinRetryInitial and MaxRetryInterval are the tenant's bounds on the
	// backoff (FR-12): retry.initial at least the first, retry.max_interval
	// at most the second; 0 is no bound.
	MinRetryInitial, MaxRetryInterval time.Duration
}

// Validate checks the policy, normalized by Parse or Normalize, and returns
// the first problem as an *Error.
func (p Policy) Validate(env Env) error {
	n := len(p.Destinations)
	switch {
	case n < 1:
		return refuse(CodeInvalidPolicy, "at least 1 destination is required")
	case env.MaxDestinations > 0 && n > env.MaxDestinations:
		return refuse(CodeTooManyDestinations, "%d destinations, the limit is %d", n, env.MaxDestinations)
	}
	seen := map[Destination]bool{}
	positions := make([]topology.Position, 0, n)
	for i, d := range p.Destinations {
		if d.Endpoint == "" || d.Path == "" {
			return refuse(CodeInvalidPolicy, "destination %d needs an endpoint and a path", i)
		}
		pos, ok := env.Resolve(d.Endpoint)
		if !ok {
			return refuse(CodeUnknownEndpoint, "destination %d: unknown endpoint %q", i, d.Endpoint)
		}
		if seen[d] {
			return refuse(CodeDuplicateDestination, "destination %d repeats %q on %q", i, d.Path, d.Endpoint)
		}
		seen[d] = true
		if d.Endpoint == env.Source.Endpoint && d.Path == env.Source.Path {
			return refuse(CodeDestinationIsSource, "destination %d is the source", i)
		}
		positions = append(positions, pos)
	}
	if err := p.validateCompletion(env, positions); err != nil {
		return err
	}
	return p.validateOptions(env)
}

func (p Policy) validateCompletion(env Env, positions []topology.Position) error {
	n, min := len(p.Destinations), p.Completion.MinCopies
	if min < 1 || min > n {
		return refuse(CodeInvalidPolicy, "min_copies must be between 1 and %d, got %d", n, min)
	}
	levels := map[string]bool{}
	for _, s := range p.Completion.Spread {
		if !env.Topology.HasLevel(s.Level) {
			return refuse(CodeInvalidPolicy, "spread names an unknown level %q", s.Level)
		}
		if levels[s.Level] {
			return refuse(CodeInvalidPolicy, "spread names level %q twice", s.Level)
		}
		levels[s.Level] = true
		if s.MinDistinct < 1 {
			return refuse(CodeInvalidPolicy, "spread on %q: min_distinct must be at least 1", s.Level)
		}
		if s.MinDistinct > min {
			return refuse(CodeUnsatisfiable, "spread on %q asks for %d distinct locations with only %d copies", s.Level, s.MinDistinct, min)
		}
		distinct := map[string]struct{}{}
		for _, pos := range positions {
			if id, ok := pos[s.Level]; ok {
				distinct[id] = struct{}{}
			}
		}
		if s.MinDistinct > len(distinct) {
			return refuse(CodeUnsatisfiable, "spread on %q asks for %d distinct locations, the destinations span %d", s.Level, s.MinDistinct, len(distinct))
		}
	}
	return nil
}

func (p Policy) validateOptions(env Env) error {
	switch p.OnSatisfied {
	case OnSatisfiedComplete, OnSatisfiedStop:
	default:
		return refuse(CodeInvalidPolicy, "on_satisfied must be %q or %q", OnSatisfiedComplete, OnSatisfiedStop)
	}
	switch p.Overwrite {
	case "fail", "replace", "skip":
	default:
		return refuse(CodeInvalidPolicy, "overwrite must be fail, replace or skip")
	}
	if p.Verify != "sha256" {
		return refuse(CodeInvalidPolicy, "verify must be sha256")
	}
	if p.Deadline <= 0 || p.Retry.Initial <= 0 || p.Retry.MaxInterval <= 0 || p.Retry.Initial > p.Retry.MaxInterval {
		return refuse(CodeInvalidPolicy, "deadline and retry durations must be positive, and retry.initial no larger than retry.max_interval")
	}
	if env.MinRetryInitial > 0 && time.Duration(p.Retry.Initial) < env.MinRetryInitial {
		return refuse(CodeInvalidPolicy, "retry.initial %v is below the tenant's minimum %v", time.Duration(p.Retry.Initial), env.MinRetryInitial)
	}
	if env.MaxRetryInterval > 0 && time.Duration(p.Retry.MaxInterval) > env.MaxRetryInterval {
		return refuse(CodeInvalidPolicy, "retry.max_interval %v is above the tenant's maximum %v", time.Duration(p.Retry.MaxInterval), env.MaxRetryInterval)
	}
	return nil
}
