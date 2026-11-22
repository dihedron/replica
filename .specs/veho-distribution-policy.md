# veho: distribution policy

This file specifies the distribution policy: which additional copies a job creates and when the job counts as done. It is part of `veho_specification.md`.

Status: draft, 2026-11-21, for review. It answers OI-1 and OI-3. The topology model (§2) is a first version: a deep-dive session is planned (backlog 17).

## 1. Model

A job replicates one source object under one *distribution policy*. Every transfer exists in the context of that policy: the policy fixes the set of destinations (N), the completion rules (M of N, with a spread over the topology) and the options that apply to all transfers.

- The **source is external**. Populating it is the client's responsibility, and the object is assumed to be there. The policy describes only the **additional copies** (replicas) to create. The source is never a destination and never counts as a copy; a client that wants it to count lowers `min_copies` by one.
- The **control plane** (the state store) runs in three sites and accepts a job on a quorum of 2 of 3 (ADR-1). Those sites are an implementation detail of the control plane. They are **not** the destination topology and the two are decoupled: a destination can be in any location of the topology, including one that has no control-plane node.
- The **data plane** has N >= 1 destinations anywhere in the topology (§2). N and the topology belong to the policy and the platform configuration, not to the deployment of the control plane.

## 2. Topology

The topology is a tree of *locations*, with named levels. A platform could define, for example, the levels `region` and `az`:

```
region eu-1
  az eu-1a
  az eu-1b
region eu-2
  az eu-2a
```

- Every destination endpoint is registered at one location (an `az`, or a `region` when the level below is not modelled). The ancestors of that location give the destination's position at every level.
- The topology is platform configuration in v1: tenants refer to its locations, they cannot create them (T-22). The number of levels and their names are configuration, not code.
- A transfer stores the position of its destination as a snapshot taken at acceptance, for example `{"region": "eu-1", "az": "eu-1a"}`, so that a later change of the topology does not change a running job.
- The topology is independent of the control plane. It can model three regions, individual availability zones inside them, or any other number of regions.

Open for the deep dive (backlog 17): tenant-private locations, other relations than a tree (for example a site that belongs to two failure domains), distances and costs for choosing the source of a transfer, and weights.

## 3. Policy document

```json
{
  "destinations": [
    { "endpoint": "eu1a-archive", "path": "replicas/big.tar" },
    { "endpoint": "eu1b-archive", "path": "replicas/big.tar" },
    { "endpoint": "eu2a-archive", "path": "replicas/big.tar" }
  ],
  "completion": {
    "min_copies": 5,
    "spread": [
      { "level": "region", "min_distinct": 3 },
      { "level": "az", "min_distinct": 5 }
    ]
  },
  "on_satisfied": "complete",
  "verify": "sha256",
  "overwrite": "fail",
  "deadline": "168h",
  "retry": { "initial": "2s", "max_interval": "15m" }
}
```

- `destinations`: N entries. The location of a destination is the one of its registered endpoint, never a value the client supplies (INV-6).
- `completion.min_copies` (M): the number of verified copies that satisfies the policy. Default: N.
- `completion.spread`: a list of statements `{level, min_distinct}`: the verified copies must be in at least `min_distinct` distinct locations of that level. Default: none. "At least 5 AZs across 3 regions" is the two statements above; a region that holds a copy holds it in some AZ, so "at least one AZ per region" follows from the 3 regions.
- `on_satisfied`: what happens to the transfers still running once the policy is satisfied. `complete` (default): they continue until they end, within the deadline. `stop`: they are cancelled.

Examples:

| Intent | Policy |
| :--- | :--- |
| 7 of 9 destinations, anywhere | `min_copies: 7` |
| 5 copies over the 3 regions | `min_copies: 5`, spread `region` 3 (2+2+1 done, 3+2+0 not) |
| 5 AZs across 3 regions | `min_copies: 5`, spread `region` 3 and `az` 5 |
| All of them | `min_copies: N` (the default) |

The family of statements is **monotone**: more verified copies can never break a statement (§5 relies on it). Statements that cap (at most k per region) or that name a specific location (at least 2 in `eu-1`) are not in this version; the first kind breaks monotonicity, the second kind keeps it. Both are for the deep dive.

Named policies (`POST /v1/policies`, OI-7) hold the same document. A job stores an immutable snapshot of its policy.

## 4. Validation (FR-3)

The API rejects a policy, synchronously, with `422` and a stable code when:

- N = 0 (no destination) or N is above the tenant's limit (`max_destinations`; platform default 32, tenant default 16; both configurable, SEC quotas);
- `retry.initial` below the tenant's `min_retry_initial`, or `retry.max_interval` above its `max_retry_interval` (FR-12);
- two destinations have the same endpoint and path (`duplicate_destination`);
- a destination is the source (same endpoint and path; `destination_is_source`);
- an endpoint does not exist or belongs to another tenant (indistinguishable, INV-4);
- not (1 <= `min_copies` <= N);
- a spread names a level that is not in the topology, or asks for more distinct locations than the destinations can supply, or more than `min_copies` (`unsatisfiable_policy`).

## 5. Evaluation

The evaluation is a pure function with no I/O: `Evaluate(policy, transfers) → Verdict`, in `internal/policy`.

Let S be the transfers `SUCCEEDED`, P the transfers not yet terminal, and `distinct(X, level)` the number of distinct locations of that level among the transfers X.

- **Satisfied** when |S| >= `min_copies` and `distinct(S, l)` >= `min_distinct` for every statement.
- **Unsatisfiable** when the same test fails even for S ∪ P. Because every statement is monotone, taking all the transfers that can still succeed maximizes every quantity together, so the test is exact and needs no search.
- **Open** otherwise.

### 5.1 Three milestones

The job tracks three things separately, each with its own timestamp:

| Milestone | When | Field |
| :--- | :--- | :--- |
| **Satisfied** | the policy minimum is met (for example 7 of 9) | `satisfied_at` |
| **Complete** | every destination has a verified copy (9 of 9) | `completed_at` |
| **Settled** | every transfer is terminal (`SUCCEEDED`, `FAILED` or `CANCELLED`) | `settled_at` |

Complete implies satisfied; settled can come with or without complete (a permanent failure settles without completing). The job state follows the verdict:

| Verdict | Job state |
| :--- | :--- |
| Open | `ACCEPTED` before the first transfer starts, then `RUNNING` |
| Satisfied | `SUCCEEDED` (`satisfied_at` set) |
| Unsatisfiable, or deadline with the verdict open | `FAILED` |
| Cancel requested and every transfer stopped | `CANCELLED` |

A job fails as soon as the policy cannot be met (fail fast), not at the deadline. With `on_satisfied: complete`, the transfers keep running after `SUCCEEDED`, and `completed_at` and `settled_at` are set later; a later failure of one of them is visible on the transfer and never changes the job state. With `on_satisfied: stop` the remaining transfers are cancelled, so the job settles without being complete.

### 5.2 Where the evaluation runs

The evaluation runs in the same transaction that moves a transfer to a terminal state (the guarded update of `veho-job-lifecycle.md` §2):

1. `SELECT … FROM jobs WHERE id = $1 FOR UPDATE`: serializes transfers that finish at the same time, so two of them cannot both miss the milestone;
2. read the transfers of the job, call `Evaluate`;
3. set the job state and the milestones that changed.

With `on_satisfied: stop` the same transaction marks the other transfers `CANCELLED`; their workers see it at the next checkpoint (the fence, `veho-job-lifecycle.md` §6).

### 5.3 Retry

`POST /v1/jobs/{id}:retry` (FR-15) on a `FAILED` job resets the failed transfers and re-evaluates. It does not change the policy.

## 6. Source of a transfer

- A node of any location can run a transfer (FR-6); choosing a node close to the source or the destination is an optimization (backlog 13).
- In v1 every transfer reads the original source. With N = 9 the source is read up to 9 times. FR-10 already allows a pending transfer to read from a verified copy when the digest matches; choosing the nearest verified copy, using the topology, is backlog item 16.

## 7. Data model changes

(`veho-job-lifecycle.md` §1 is updated to match.)

- `locations(id, parent_id, level, name)`: the topology, platform configuration.
- `endpoints`: `location_id` replaces `region` (nullable for a source endpoint).
- `jobs`: `policy jsonb` is the immutable snapshot; new `policy_ref` (nullable), `satisfied_at`, `completed_at`, `settled_at`, `copies_done int`.
- `transfers`: new `position jsonb`, the snapshot of the destination's location at every level, so that the evaluation reads one table.
- `policies(id, tenant_id, name, version, spec jsonb, created_at)` for named policies, under row-level security like every table.

## 8. API changes

`GET /v1/jobs/{id}` adds:

```json
"completion": { "min_copies": 5, "spread": [ { "level": "region", "min_distinct": 3 } ] },
"copies": { "done": 3, "total": 9,
            "by_level": { "region": { "eu-1": 2, "eu-2": 1, "eu-3": 0 } } },
"satisfied_at": null, "completed_at": null, "settled_at": null
```

`progress` is `copies.done / completion.min_copies`, capped at 1; a second figure, `copies.done / copies.total`, tracks the full replication. `GET /v1/jobs/{id}/transfers` stays the detail per destination.

## 9. Requirements

- **DP-1** The policy describes only the additional copies; the source is external and is never counted.
- **DP-2** The completion rule is `min_copies` of N destinations plus spread statements over the levels of the topology; all are set per policy.
- **DP-3** The topology is independent of the control-plane sites and has any number of locations and levels.
- **DP-4** The location of a destination comes from its registered endpoint.
- **DP-5** The verdict is computed by a pure function and applied in the transaction that ends a transfer, under a lock on the job row.
- **DP-6** A job fails as soon as its policy cannot be met.
- **DP-7** The job tracks, separately, when the policy is satisfied, when all destinations have a copy and when all transfers are terminal.
- **DP-8** After `SUCCEEDED` a later transfer failure never changes the job state.
- **DP-9** A job keeps an immutable snapshot of its policy and of the position of each destination.
- **DP-10** The policy is validated at submission (§4). The limits on N are configurable.

## 10. Tests

- Unit and property tests of `Evaluate`: the verdict is exactly one of the three and is monotonic (a transfer that succeeds never turns a satisfied verdict into another one).
- A fuzz test (AGENTS.md rule 9) of the policy parser and validator, and of `Evaluate` against a brute-force check on small N and small topologies.
- Acceptance scenario AC-23 (`veho-acceptance-matrix.md`).
