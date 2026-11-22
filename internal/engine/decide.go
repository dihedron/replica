package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"uuid"

	"github.com/dihedron/veho/internal/policy"
	"github.com/dihedron/veho/internal/topology"
	"github.com/jackc/pgx/v5"
)

// decide evaluates the job's policy in the caller's transaction, which ends
// one of its transfers (§4.3, veho-distribution-policy.md §5.2): it locks
// the job, sets its state and milestones, and cancels the transfers left
// when the job is decided and nobody waits for them.
func (e *Engine) decide(ctx context.Context, tx pgx.Tx, job uuid.UUID) error {
	var (
		state  string
		spec   []byte
		cancel bool
	)
	if err := tx.QueryRow(ctx, "select state, policy, cancel_requested from veho.jobs where id = $1 for update", job).Scan(&state, &spec, &cancel); err != nil {
		return fmt.Errorf("engine: decide: %w", err)
	}
	var p policy.Policy
	if err := json.Unmarshal(spec, &p); err != nil {
		return fmt.Errorf("engine: decide: the policy: %w", err)
	}
	v, err := evaluate(ctx, tx, p, job)
	if err != nil {
		return err
	}
	next := state
	stopOthers := false
	if state == "ACCEPTED" || state == "RUNNING" {
		switch {
		case v.Outcome == policy.Satisfied: // a fact, even if a cancel came late
			next, stopOthers = "SUCCEEDED", p.OnSatisfied == policy.OnSatisfiedStop
		case cancel && v.Settled: // a cancel wins over an unsatisfiable rule
			next = "CANCELLED"
		case cancel:
			next = "RUNNING"
		case v.Outcome == policy.Unsatisfiable:
			next, stopOthers = "FAILED", true
		default:
			next = "RUNNING"
		}
	}
	if stopOthers && !v.Settled {
		// the fence moves too, so that their in-flight steps write nothing
		if _, err := tx.Exec(ctx, `update veho.transfers set state = 'CANCELLED', finished_at = now(), fence = fence + 1,
			lease_owner = null, lease_expires_at = null where job_id = $1 and state <> all($2)`, job, terminal); err != nil {
			return fmt.Errorf("engine: decide: %w", err)
		}
		if v, err = evaluate(ctx, tx, p, job); err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `update veho.jobs set state = $2, copies_done = $3, updated_at = now(),
		satisfied_at = case when $4 then coalesce(satisfied_at, now()) else satisfied_at end,
		completed_at = case when $5 then coalesce(completed_at, now()) else completed_at end,
		settled_at = case when $6 then coalesce(settled_at, now()) else settled_at end,
		finished_at = case when $2 in ('SUCCEEDED', 'FAILED', 'CANCELLED') then coalesce(finished_at, now()) else finished_at end
		where id = $1`, job, next, v.Copies, v.Outcome == policy.Satisfied, v.Complete, v.Settled)
	if err != nil {
		return fmt.Errorf("engine: decide: %w", err)
	}
	return nil
}

type transferRow struct {
	State    string
	Position []byte
}

// evaluate reads the job's transfers and evaluates the policy on them.
func evaluate(ctx context.Context, tx pgx.Tx, p policy.Policy, job uuid.UUID) (policy.Verdict, error) {
	rows, err := tx.Query(ctx, "select state, position from veho.transfers where job_id = $1", job)
	if err != nil {
		return policy.Verdict{}, fmt.Errorf("engine: decide: %w", err)
	}
	trs, err := pgx.CollectRows(rows, pgx.RowToStructByPos[transferRow])
	if err != nil {
		return policy.Verdict{}, fmt.Errorf("engine: decide: %w", err)
	}
	ts := make([]policy.Transfer, len(trs))
	for i, t := range trs {
		var pos topology.Position
		if err := json.Unmarshal(t.Position, &pos); err != nil {
			return policy.Verdict{}, fmt.Errorf("engine: decide: a position: %w", err)
		}
		ts[i] = policy.Transfer{State: policy.State(t.State), Position: pos}
	}
	v, err := policy.Evaluate(p, ts)
	if err != nil {
		return policy.Verdict{}, fmt.Errorf("engine: decide: %w", err)
	}
	return v, nil
}
