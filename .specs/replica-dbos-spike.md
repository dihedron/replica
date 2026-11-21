# replica: DBOS spike report

This file records the experiments that checked the assumptions the design makes about DBOS Transact for Go (`EXT-nn` in `replica_specification.md`). Round 1 ran on v0.11.0, round 2 on v1.9.0, which is the pinned version.

Status: draft, 2026-11-21, for review.

Two rounds. **Round 1** ran on v0.11.0 (the version first pinned in `go.mod`).
**Round 2** re-ran everything on **v1.9.0**, which is now the pinned version
(`go.mod` upgraded; the existing `step/`, `workflow/` and `command.go`
scaffold was ported: `DBOSContext`→`Context`, `NewDBOSContext`→`NewContext`,
`Shutdown(c, d)`). Round 2 supersedes round 1 wherever they differ.

Method: read the module source and ran the harness in `test/spikes/dbos/` against
real PostgreSQL 16.15 (executors as separate OS processes with distinct
executor IDs and the same application version; a second Postgres instance for
failover). Timings are local, so only ordering/behaviour matters.

## Results (v1.9.0 unless stated)

| ID | Question | Result | Delta vs v0.11.0 |
|----|----------|--------|------------------|
| EXT-1 | Enqueue in the same SQL transaction as the app's own writes? | **Yes.** `dbos.WithEnqueueTransaction(tx)` (pgx.Tx, `*sql.Tx` or `dbos.Tx`) enqueues on the caller's transaction; the workflow exists only after the caller commits. Tested: insert + enqueue then **rollback** → 0 app rows, 0 workflows; **commit** → 1 and 1, and a running executor then executed it. Constraints: the tx must run on the DBOS system database (so app tables live in the same database); not usable inside a workflow; incompatible with `DeduplicationPolicyReturnExisting`; a failed enqueue leaves the tx aborted. | v0.11.0: **no**. Outbox was needed. |
| EXT-1b | Is enqueue idempotent by workflow ID? | **Yes.** Two enqueues of `wf-dup` ran it once. | same |
| EXT-2a | Automatic takeover of a dead executor's work (OSS)? | **No.** No executor heartbeat/liveness code exists in the module (source search); only Conductor (commercial) drives recovery, and `Launch` recovers only the **local** executor's PENDING work. | same |
| EXT-2b | Explicit adoption by another executor | **Yes, public and now documented.** `ListWorkflows(ctx, WithFilterExecutorIDs(dead), WithFilterStatus(PENDING))` then `ResumeWorkflows[any](ctx, ids)`: "each workflow that exists and is not in a terminal state is re-enqueued"; `WithResumeQueue(q)` chooses the queue. Tested: A killed mid-part-3, B adopted and re-ran only part 3; status `SUCCESS` on B. | v0.11.0: worked but documented only for cancelled workflows; the admin HTTP server no longer exists |
| EXT-2c | Zombie (SIGSTOP'd) executor after takeover | **Fenced at next checkpoint; in-flight step still completes.** After SIGCONT the old owner finished its current part, then stopped; exactly one extra execution of that part (15 begins for 14 parts). Our fence + idempotent part writes remain mandatory. | same |
| EXT-2d | Poison-pill limit | `WithMaxRecoveryAttempts(n)` (default 100) → workflow parked as `MAX_RECOVERY_ATTEMPTS_EXCEEDED`. `ResumeWorkflows` **resets `recovery_attempts`** (source: `SET … recovery_attempts = …` in resume SQL), so adoption by resume bypasses the cap ⇒ reconciler keeps its own counter. | renamed from `WithMaxRetries` |
| EXT-3a | Per-tenant fairness | **Works.** Partitioned queue with `WithPartitionConcurrency(1)`: tenant X's 3 jobs ran serially while tenant Y's job ran concurrently with X's first. `WithPartitionQueue()` is **deprecated** (setting both is an error); also `WithPartitionWorkerConcurrency`, `WithPartitionRateLimiter`. | v0.11.0: `WithPartitionQueue`+`WithGlobalConcurrency` |
| EXT-3b | Queue model | Queues are now **registered in the system DB** (`RegisterQueue` returns `(Queue, error)`, `RetrieveQueue`, `DeleteQueue`), owned by an application name. | new |
| EXT-4a | Postgres failover with multi-host DSN | **Works, no duplicate work.** DSN `host=a,b&target_session_attrs=read-write`; synchronous quorum standby (`ANY 1 (walreceiver)`, confirmed `quorum` in `pg_stat_replication`); primary killed with `-m immediate`, standby promoted mid-workflow: executor reconnected itself, ~3 s stall, all 18 parts executed exactly once, workflow `SUCCESS` on the new primary. | v0.11.0: ~1.5 s stall (async) |
| EXT-4b | PgBouncer / transaction pooling | Not tested. `LISTEN`-based notification session still used (poller only on backends without LISTEN). Connect directly. | same |
| EXT-5 | Versioning | Recovery/dequeue filter by application version; v1.9.0 adds `AutoUpgrade` (re-enqueue PENDING work from old versions) but it is a **Transact Enterprise** feature (needs a key) — not used. Rolling-upgrade behaviour not exercised end to end (AC-20). | enterprise-gated option added |
| EXT-6 | Step retry | `WithStepMaxRetries`, `WithStepBaseInterval`, `WithStepBackoffFactor`, `WithStepMaxInterval` **and `WithStepRetryPredicate(func(error) bool)`**. Tested: a transient error retried 5 times (200 ms→400→800→1 s cap) and succeeded; an error the predicate rejects was attempted **once** and returned as a workflow error. Still **no jitter**. ⇒ classify via the predicate; add jitter ourselves if needed. | v0.11.0: no classification (renamed options too: `WithStepBackoffFactor` etc.) |

Other v1.9.0 facts that matter:
- `AppName` must match `^[a-z0-9-_]{3,256}$`.
- Enterprise-key features (`AutoUpgrade`, `Sharding`, `SuspendMode`, Conductor)
  **must stay disabled**; the design uses none of them.
- Gob-encoded errors: custom error types returned by a workflow are persisted
  by message only unless registered, so `errors.Is/As` will not survive a
  read-back — carry the error *class* in the returned value, not the type.
- CockroachDB is a supported backend (does not remove the WAN-latency concern,
  replica-architecture.md §3.3).

## Test-harness note (honesty)

My first v1.9.0 failover attempt was **invalid**: the standby inherited a stale
`primary_slot_name` and never streamed, so after promotion it lacked the
workflow row and the executor stalled ("Workflow ID conflict … waiting").
That was my setup error, not DBOS behaviour; it is, however, a live
illustration of why asynchronous replication gives RPO > 0. After fixing the
setup and verifying `sync_state = quorum`, the test passed (above).

## Consequences applied to the specs

- replica-job-lifecycle.md §3: acceptance is a **single transaction** (job rows + DBOS enqueue).
- replica-job-lifecycle.md §6: adoption via `ListWorkflows` + `ResumeWorkflows`; own adoption counter.
- replica-job-lifecycle.md §7: retry via `WithStepRetryPredicate`.
- replica-job-lifecycle.md §8: `WithPartitionConcurrency`.
- 06: no admin server; AC-21 regression suite on each DBOS bump.
- 07: ADR-5, OI-13 resolved.

## Reproduce

```
go build -o /tmp/spike ./test/spikes/dbos
spike worker <executorID> <dsn>                        # ADOPT=<deadID> adopts its PENDING work
spike enqueue <dsn> <queue> <wfID> [partitionKey|-] [parts] [ms-per-part]
spike txenq <dsn> commit|rollback <wfID>               # transactional enqueue
spike retry <dsn>                                      # retry predicate
```
Events are appended to `/var/tmp/pgspike/events.log`. `spikes/` is disposable:
delete it once implementation starts.
