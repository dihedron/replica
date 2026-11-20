# 08 — Spike report: DBOS Go v0.11.0 (resolves VERIFY-1…6)

Method: read the module source (`dbos-transact-golang@v0.11.0`) and ran the
harness in `spikes/dbos/` against real PostgreSQL 16.15 (local, two instances
for failover). Executors were separate OS processes with distinct executor IDs
and the same application version. Timings are local-machine, so only
ordering/behaviour matters, not absolute values.

## Results

| ID | Question | Result | Evidence |
|----|----------|--------|----------|
| VERIFY-1 | Enqueue inside an application SQL transaction? | **No.** `Client.Enqueue` / `RunWorkflow` use their own transaction; the transactional step helper (`runAsTxn`) is package-private. ⇒ the **outbox pattern is the design**, not a fallback. | source: `workflow.go` (`runAsTxn` lower-case), `client.go` |
| VERIFY-1b | Is enqueue idempotent? | **Yes.** Enqueueing twice with the same workflow ID (`WithEnqueueWorkflowID`) ran the workflow once. | experiment E2 |
| VERIFY-2a | Does another executor take over a dead executor's work automatically (OSS)? | **No.** Executor B sat idle with A's workflow `PENDING` (owner A). Nothing detects dead executors; recovery is explicit and keyed on **executor ID**. | E1 |
| VERIFY-2b | Can another executor resume it on demand? | **Yes, via the public API (preferred; pointed out by the project owner).** `ListWorkflows(WithExecutorIDs([dead]), WithStatus(PENDING))` then `ResumeWorkflow(id)` on any live executor re-enqueues the workflow (`ENQUEUED`, internal queue); B picked it up and resumed after the last completed step (killed A mid part 3; B re-ran part 3 only). No admin server needed. Caveats: documented as "resume a cancelled workflow" but works on any non-terminal one, so semantics may change (pin the version + regression test, AT-21); it **resets `recovery_attempts` to 0**, defeating DBOS's poison-pill cap (reconciler keeps its own counter); in v1.x the signature takes a `Client` and has `WithResumeQueue`. Experiment E6. Earlier route (also works): `POST /dbos-workflow-recovery` with body `["exec-A"]` on B's admin server: B resumed `wf-1` from the last completed step (parts 0–3 not re-run; in-flight part 4 re-executed). `recovery_attempts` incremented. Queue-assigned workflows are simply reset to `ENQUEUED` and picked up by whichever executor dequeues next. | E1, `recovery.go` |
| VERIFY-2c | Zombie (paused) executor after takeover? | DBOS **fences it at the next checkpoint**: the stopped executor completed its in-flight step (a real side effect, "part 3 end" logged after B had taken over), then did not continue; B finished the workflow. ⇒ in-flight side effects can duplicate; our idempotent part writes + fence (02 §6) remain mandatory. | E1b (SIGSTOP/SIGCONT); migration `7_add_owner_xid.sql` |
| VERIFY-2d | Poison-pill limit | A workflow is recovered at most `WithMaxRetries` (default **100**) times, then parked as `MAX_RECOVERY_ATTEMPTS_EXCEEDED`. Reconciler must watch for that status. | source `workflow.go:472`, `system_database.go:681` |
| VERIFY-3a | Queue partitioning for per-tenant fairness | **Works.** With `WithPartitionQueue()` + `WithGlobalConcurrency(1)` the limit is **per partition key**: tenant X jobs ran serially while tenant Y's job ran concurrently with X's first. | E3 |
| VERIFY-3b | Other queue controls | Present: worker concurrency, global concurrency, priority, rate limiter, dedup ID. (A cap across *all* partitions at once was not tested.) | `queue.go` |
| VERIFY-4a | Multi-host DSN / Postgres failover | **Works.** pgx DSN `host=a,b&target_session_attrs=read-write`; primary killed (`-m immediate`) + standby promoted mid-workflow: executor reconnected on its own, ~1.5 s stall, workflow completed with no step re-run lost. | E5 |
| VERIFY-4b | PgBouncer | **Not tested; treat as unsupported.** DBOS opens a dedicated session with `LISTEN` for notifications/events (used by `Send/Recv/SetEvent/GetEvent`); transaction pooling breaks that. We do not use those APIs, but connect directly to be safe. | source `system_database.go:1836` |
| VERIFY-5 | Versioning for rolling deploys | Recovery and dequeue filter on **application version** (`application_version = $x OR NULL`). A new-version executor will **not** recover old-version `PENDING` workflows. ⇒ old executors must drain, or we use a stable `ApplicationVersion` and keep workflow code replay-compatible. (Not exercised end-to-end.) | `recovery.go`, `system_database.go:2754` |
| VERIFY-6 | Step retry/backoff | **Works:** `WithStepMaxRetries`, `WithBaseInterval`, `WithBackoffFactor`, `WithMaxInterval` (verified 200 ms→400→800→1 s cap, success on attempt 5). Limits: **no jitter, no error classification** (any returned error is retried; retries are in-process). ⇒ steps return *Permanent* failures as values, return `error` only for transient ones. | E4 |
| extra | CockroachDB | DBOS Go has a CockroachDB code path (`isCockroachDB`, polling instead of LISTEN). Available as an alternative state store but does not remove the WAN-latency concern (see 01 §3.3). | source |

## Consequences applied to the specs

- 02 §3: outbox is the mechanism.
- 02 §6: executor registry + reconciler that adopts orphaned work with
  `ListWorkflows(WithExecutorIDs)` + `ResumeWorkflow` (public API); own
  poison-pill counter; version policy.
- 02 §7: error handling in steps.
- 02 §8 and 07: fairness approach confirmed.
- 06 §3: connect directly to Postgres (no transaction pooler).
- 01 §3.3 and ADR-2: WAN consensus concern (separate from this spike).

## Reproduce

```
go build -o /tmp/spike ./spikes/dbos
spike worker <executorID> <adminPort> <dsn>      # executor with admin server
spike enqueue <dsn> <queue> <wfID> [partitionKey|-] [parts] [ms-per-part]
spike retry <dsn>
ADOPT=exec-A spike worker exec-B <port> <dsn>      # B lists A's PENDING workflows and ResumeWorkflow()s them
curl -X POST localhost:<adminPort>/dbos-workflow-recovery -d '["exec-A"]'   # alternative route
```
Events are appended to `/var/tmp/pgspike/events.log`.
`spikes/` is disposable: delete it once implementation starts.
