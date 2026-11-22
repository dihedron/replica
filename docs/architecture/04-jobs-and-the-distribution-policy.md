# Jobs, transfers and the distribution policy

## The model

A **job** replicates one source object, named by a registered endpoint and a path, to N **destinations**, each an endpoint and a path, N ≥ 1. Each destination gets one **transfer**. The job's **policy** says when the job is done:

```json
{
  "destinations": [
    {"endpoint": "b-images", "path": "golden/img.qcow2"},
    {"endpoint": "c-images", "path": "golden/img.qcow2"},
    {"endpoint": "c-dr",     "path": "golden/img.qcow2"}
  ],
  "completion": {"min_copies": 2, "spread": [{"level": "region", "min_distinct": 2}]},
  "on_satisfied": "complete",
  "deadline": "168h",
  "retry": {"initial": "2s", "max_interval": "15m"}
}
```

- `completion.min_copies`: M of the N destinations (default N, all of them).
- `completion.spread`: statements over the levels of the topology: at least `min_distinct` distinct locations at a level among the verified copies (for example, copies in at least 2 regions, or in 5 availability zones across 3 regions).
- `on_satisfied`: once the policy is met, the remaining transfers continue (`complete`, the default) or are cancelled (`stop`).
- `deadline`: the job fails if its verdict is still open then.
- `retry`: the back-off of the transfers' retries, within the tenant's bounds.
- `overwrite`: what a destination that already holds the object does (`fail`, `replace`, `skip`); `verify`: `sha256`, the only mode.

Policies are named and versioned, with grants (`submit`, `read`, `manage`) to principals and groups, or given inline when the tenant allows it.

## The topology

Destinations are anywhere: veho's three regions are where its control plane runs, not a limit on where copies go. Each endpoint is placed in a **location** of a tree with named levels (for example `region`, then `az`), and spread statements count distinct locations at a level.

## The verdict

The verdict is computed in the transaction that ends a transfer, under a lock on the job's row, so two transfers that end at the same instant give one consistent verdict:

- **Satisfied** when the verified copies meet `min_copies` and every spread statement: the job is `SUCCEEDED`.
- **Unsatisfiable** when even every transfer still running succeeding could not meet them: the job is `FAILED` at once (fail fast) and its other transfers are cancelled. Every statement is monotone (more copies never break one), so this test is exact.
- Otherwise the job is still `RUNNING`.

Three times are recorded separately: `satisfied_at`, `completed_at` (every destination has a copy) and `settled_at` (every transfer has ended). With `on_satisfied: complete`, a transfer that fails after the job succeeded is visible on the transfer and never changes the job.

A cancel requested wins over an unsatisfiable verdict: the job ends `CANCELLED`, not `FAILED`. A job met meanwhile stays `SUCCEEDED`. `retry` on a failed job restarts its failed and cancelled transfers and evaluates again.

## Validation

A policy is checked when it is created and when a job is submitted, synchronously, with a stable code: no destination; more than the tenant's `max_destinations`; an unknown endpoint (another tenant's endpoint looks unknown too); two destinations with the same endpoint and path; a destination equal to the source; `min_copies` outside 1 to N; a spread that the destinations cannot satisfy; an option out of range; a retry outside the tenant's bounds.

## Every transfer reads the source

In this version every transfer reads the original source. With many destinations the source is read many times; reading each transfer from the nearest verified copy instead is planned.
