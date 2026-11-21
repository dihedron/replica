# replica: job lifecycle, workflows and retries

This file specifies the data model, the state machines, the workflows, resumption, adoption of orphaned work and the retry policy. It is part of `replica_specification.md`.

Status: draft, 2026-11-21, for review.

## 1. Data model (application schema)

All tables carry `tenant_id` and are protected by row-level security (replica-tenancy.md).

```
tenants(id, name, state, quotas jsonb, created_at)
endpoints(id, tenant_id, name, region, driver, config jsonb,
          credential_ref, created_at)            -- no secrets stored inline
jobs(id uuid, tenant_id, idempotency_key, source_endpoint_id, source_path,
     source_info jsonb,            -- size, etag/version, sha256 once known
     policy jsonb, state, deadline_at, cancel_requested bool,
     created_at, updated_at, finished_at, last_error jsonb)
transfers(id uuid, job_id, tenant_id, destination_endpoint_id, dest_path,
          state, attempt int, bytes_done bigint, bytes_total bigint,
          resume_token jsonb,        -- driver-specific (e.g. multipart upload id)
          lease_owner, lease_expires_at, fence bigint,
          last_error jsonb, started_at, finished_at)
parts(transfer_id, part_no, offset, length, sha256, etag, state)  -- committed parts
UNIQUE (tenant_id, idempotency_key)
```

`transfers.fence` is a monotonically increasing integer incremented each time
a node takes the lease (see §6).

## 2. State machines

Job: `ACCEPTED → RUNNING → SUCCEEDED | FAILED | CANCELLED`

- `ACCEPTED`: committed, no transfer started yet.
- `RUNNING`: at least one transfer started and not all terminal.
- `SUCCEEDED`: all transfers `SUCCEEDED`.
- `FAILED`: all transfers terminal and ≥1 `FAILED` (others may be `SUCCEEDED`;
  per-transfer detail is authoritative). Retryable via API (FR-15).
- `CANCELLED`: cancel requested and all transfers stopped.

Transfer: `PENDING → COPYING → VERIFYING → SUCCEEDED`
with side exits `RETRY_WAIT` (transient failure, back to `COPYING`),
`FAILED` (permanent / deadline), `CANCELLED`.

Transitions are written only inside DBOS steps or guarded SQL
(`UPDATE … WHERE state = $expected AND fence = $mine`).

## 3. Acceptance (the "write down reliably" step)

```
API handler (any node):
  authenticate, resolve tenant, validate, check quota
  BEGIN                                   -- on the DBOS system database
    INSERT jobs (…)                       -- ON CONFLICT (tenant_id, idempotency_key) → return existing
    INSERT transfers (one per destination, state=PENDING)
    dbos.Enqueue(queue "jobs", "ReplicateJob", {jobID},
                 WithEnqueueWorkflowID(job.id), WithEnqueueTransaction(tx))
  COMMIT   -- synchronous cross-region commit (NFR-1): job rows and workflow atomically
  return 202 { job_id }
```

Verified on DBOS Go v1.9.0 (replica-dbos-spike.md, EXT-1): the workflow exists only if the
transaction commits, so there is no window in which a job exists without its
workflow (no outbox needed). Requirements: application tables live in the
same database as the DBOS system schema; `WithEnqueueTransaction` cannot be
combined with `DeduplicationPolicyReturnExisting` (idempotency is provided by
our `UNIQUE (tenant_id, idempotency_key)` instead); on error roll back the
transaction rather than retrying on it. The reconciler still checks for
`ACCEPTED` jobs without a workflow as a defensive invariant check (alert, then
enqueue), but this is not an expected path.

## 4. Workflows

### `ReplicateJob(jobID)`  (queue: `jobs`)
1. Step `resolve`: load job, `Stat` the source, record `source_info`
   (size, version/ETag). Transient failure → retried (§7). `NOT_FOUND` after
   grace period → job `FAILED`.
2. Start one child `Transfer` workflow per destination, ID
   `"{jobID}/{transferID}"`, on queue `transfers`. Run in parallel.
3. Wait for all children; compute job state; step `finalize` (set
   terminal state, release quota).

### `Transfer(transferID)`  (queue: `transfers`)
1. Step `acquire`: take lease, `fence++`.
2. Step `plan`: split into parts (`part_size`, default 64 MiB; larger if
   needed to stay ≤ 10,000 parts), persist plan. Idempotent.
3. Step `begin`: `Sink.Begin` (or reuse `resume_token`); persist token.
4. Loop over part *batches* (batch = `parts_in_flight`): step `copy_batch`
   streams parts, records each committed part (`parts` + `bytes_done`)
   in one transaction per batch. On replay, parts already in `parts` are skipped.
5. Step `commit`: `Upload.Commit` (atomic publish, FR-9).
6. Step `verify`: compare size and digest with source info (FR-8). Mismatch →
   delete destination object, reset parts, retry from scratch (counts as an
   attempt; after N mismatches → `FAILED: INTEGRITY`).
7. Step `finish`: `SUCCEEDED`, release lease.

Cancellation: workflows check `cancel_requested` between batches; on cancel
call `Upload.Abort` and mark `CANCELLED`.

Determinism: workflow functions only orchestrate; all I/O is inside steps,
as required by DBOS replay semantics.

## 5. Resume semantics

- Plugin advertises `ResumableUpload` (e.g. S3 multipart, Glance staged
  upload). Then resume continues after the last committed part using
  `resume_token`.
- Without it, resume restarts the transfer from byte 0 (still correct, just
  slower).
- Source reads are range reads; `Source` MUST support `ReadRange` (replica-plugins.md) so any
  part can be re-read independently.
- Resume token validity is checked (`Sink.Resume` returns `ErrTokenExpired`
  → fall back to restart).

## 6. Node failure, adoption and fencing

Facts established by the spike (replica-dbos-spike.md): DBOS queues are durable and any
listening executor dequeues new work; **nothing detects a dead executor**;
adoption is explicit (list by executor ID + `ResumeWorkflows`), resumes the *same workflow ID* from
its last completed step on whichever executor performs the recovery; a paused
("zombie") executor is fenced by DBOS at its next checkpoint, but its in-flight
step still finishes. Therefore:

1. **Executor registry**: each node has a unique stable executor ID and
   heartbeats into `executors(id, region, last_seen, app_version)` every 10 s.
2. **Reconciler** (every node, every 30 s; idempotent, so no leader needed; an
   advisory lock MAY reduce duplicate work): for executors with
   `last_seen` older than the lease TTL (default 60 s) it lists that
   executor's `PENDING` workflows (`ListWorkflows(WithFilterExecutorIDs(dead),
   WithFilterStatus(PENDING))`) and calls `ResumeWorkflows(ids)` (public,
   documented API; replica-dbos-spike.md EXT-2b). The workflows return to the queue and any node
   picks them up. Caveat:
   `ResumeWorkflows` resets DBOS's `recovery_attempts`, so the reconciler keeps
   its own adoption counter per transfer (`transfers.adoptions`, cap e.g. 20 →
   `FAILED(Internal)` + alert), and still handles
   `MAX_RECOVERY_ATTEMPTS_EXCEEDED`. Also: the §3 invariant check and deadlines.
3. **Fencing for side effects**: every state-changing write includes
   `fence = $mine`, where `fence` is incremented when a node takes the
   transfer (`acquire` step). DBOS does not fence the *in-flight step*, so a
   zombie may still write a part after takeover; part writes are idempotent per
   `(transfer, part_no)`, and the zombie's next table write fails
   (`0 rows affected`) and aborts it.
4. **Self-fencing on partition**: if a node cannot renew its heartbeat/lease
   for `TTL/2`, it stops reading/writing and drops its transfers (bounding
   the zombie window).
5. **A live node wrongly declared dead** (e.g. GC pause) is just the zombie
   case in 3.
6. **Application version**: recovery only applies to workflows of the same
   `ApplicationVersion` (replica-dbos-spike.md, EXT-5) — see replica-acceptance-matrix.md §3 for upgrades.

## 7. Error taxonomy and retry policy

Plugins classify errors (replica-plugins.md). Core handles by class:

| Class | Examples | Behaviour |
|-------|----------|-----------|
| `Transient` | timeouts, 5xx, throttling (429/503), connection reset, DB failover | Retry with backoff; unlimited attempts until `deadline_at` |
| `ResumeInvalid` | multipart upload expired/aborted | Discard token, restart transfer (attempt++) |
| `Integrity` | checksum/size mismatch | Restart from scratch, max 3, then `FAILED` |
| `Permanent` | 403/401 after credential refresh, destination bucket missing, source deleted, `SOURCE_CHANGED`, quota exceeded at destination | Transfer `FAILED` immediately; no further retries |
| `Unknown` | unclassified | Treated as `Transient` but capped at 10 consecutive occurrences, then `FAILED` |

Defaults (overridable in policy within tenant limits):
`initial=2s, multiplier=2, max_interval=15m, jitter=±20%, deadline=7d`.

Implementation note (replica-dbos-spike.md, EXT-6): DBOS v1.9.0 step retry
(`WithStepMaxRetries`, `WithStepBaseInterval`, `WithStepBackoffFactor`,
`WithStepMaxInterval`) is in-process and has no jitter, but supports
classification through `WithStepRetryPredicate(func(error) bool)`: steps wrap
plugin errors in typed errors and the predicate returns `false` for
`Permanent`/`Integrity`, so DBOS stops retrying and the workflow handles the
failure. Because custom error types do not survive persistence
(gob-encoding, 08), the class is also carried in the step's result value and
in `transfers.last_error`. Jitter, where wanted, is added inside the step.
A crash mid-retry is covered by workflow recovery (§6).

Authentication failures first trigger a credential re-resolution (secret may
have rotated) before being deemed permanent.

A `FAILED` transfer can be retried by the client (`POST …:retry`), which
resets attempts and re-enqueues; this is the answer to "permanent" being
wrong (e.g. operator fixed bucket permissions).

## 8. Quotas and fairness

- Per-tenant `max_active_jobs` and `max_bytes_in_flight`: enforced at
  submission (reject `429`) and at transfer start (wait).
- Queue concurrency: global limit per node and per cluster; per-tenant
  fairness through **partitioned queues keyed by `tenant_id`**
  (`WithPartitionConcurrency(n)`: the limit applies per partition;
  `WithPartitionQueue()` is deprecated; verified in 08, EXT-3a). A per-node cap
  (`WithWorkerConcurrency`) bounds total load. A cap across all partitions is
  not verified; enforce it with our own counter if needed.
