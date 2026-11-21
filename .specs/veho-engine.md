# veho: the engine (chunk C6)

**Status:** approved 2026-11-21. C6a done. Requirements: `veho-job-lifecycle.md` (all), `veho-distribution-policy.md` §5, `veho-audit-housekeeping.md` §3 (housekeeping; the audit sealer is backlog 32), FR-5 to FR-15, NFR-1, NFR-3, NFR-5, INV-1 to INV-6; DBOS facts: `veho-dbos-spike.md` (EXT-1 to EXT-6). Patterns copied from ligo 9f11da9 (`internal/workflows`: `engine.go` error sanitising, `executors.go`, `sweeper.go`; `internal/config`), adapted.

This document fixes the decisions the lifecycle leaves open, and amends it where noted. The HTTP and gRPC API is C7: C6 exposes `Submit`, `Cancel`, `Retry` and `Status` as Go functions, which C7's handlers call.

## 1. Sub-chunks

C6 is built and committed in five steps, each with its tests, like C2 and C5:

| Step | Scope |
| :--- | :--- |
| **C6a** Core | `internal/config` (`veho.yaml`); `internal/engine`: the DBOS context on the state store, the queues, the executor registry with heartbeat and self-fencing; `Submit` (transactional acceptance, §3); `veho serve` running the executor only |
| **C6b** Transfer | the `Transfer` workflow (§4): lease and fence per step, the part plan, begin or resume, part batches streamed and checkpointed, commit, verify; credentials per attempt from `internal/secrets`; driver resolution (§6); the retry loop (§5) |
| **C6c** Job | the `ReplicateJob` workflow: resolve the source, start the transfers, finalize; policy evaluation in the transaction that ends a transfer (§4.3); `on_satisfied`; `Cancel`, `Retry`, `Status`; the deadline |
| **C6d** Housekeeping | the loop of `veho-audit-housekeeping.md` §3 under `TryLock`: adoption of a dead executor's work, the zombie kinds, expired leases, retention; `SYSTEM_*` audit events; Prometheus metrics on `veho serve` |
| **C6e** DBOS suite | the spike's experiments (EXT-1 to EXT-6) promoted into an integration suite (AC-21) that runs on every DBOS version bump, and `test/spikes/dbos` deleted; the multi-region cases on the lab (an executor killed and adopted from another region, a failover of the primary mid-transfer) |

## 2. Configuration and process (C6a)

- `veho serve --config /etc/veho/veho.yaml`, read with `go.yaml.in/yaml/v3` (as ligo), unknown keys refused, validated at start (fail fast, T-24); its SHA-256 is logged at start. DSNs are not in the file: each is the name of an environment variable (`dsn_env`), as for `veho migrate`.
- Keys (defaults in brackets): `node.id` (the executor ID; [hostname]), `node.region` (a location of the topology), `database.{api,worker,dbos}.dsn_env`, `secrets.unwrap_key` (`file:` or `systemd-credential:`), `audit.sink` [syslog], `engine.{heartbeat [10s], lease_ttl [60s], part_size_mib [64], parts_in_flight [4], worker_concurrency [16], tenant_concurrency [4]}`, `housekeeping.{interval [30s], workflow_retention [168h], jobs_retention [2160h]}` (Go durations; `0s` keeps everything), `plugins` (process plugins: name, path, sha256), `metrics.listen` [127.0.0.1:9464; `off` turns it off].
- The DBOS application: name `veho`, system database the state store (`dbos` schema, role `veho_dbos`), application version = the build's version (EXT-5), executor ID = `node.id`. Enterprise features stay off.
- Start order: configuration; audit sink; pools and the PostgreSQL checks (`store.Checks`); the keyring (`secrets.LoadKeyring`, fail fast); the topology (`node.region` must be a location); the plugins; one heartbeat (the executor is registered before it dequeues); DBOS `Launch`; the heartbeat loop; housekeeping. A failure at any step stops the start with its error. `SIGTERM` stops dequeuing, waits up to 30 s for steps in flight, then exits; their workflows resume elsewhere (§7).

## 3. Acceptance (C6a)

`Submit(ctx, tenant, Request{IdempotencyKey, SourceEndpoint, SourcePath, Policy}) (JobID, created bool, error)`, in one transaction on the API pool (`veho_api`), as in `veho-job-lifecycle.md` §3:

1. resolve the source endpoint by name in the tenant (not found and another tenant's look the same, INV-4, INV-6);
2. parse and validate the policy (`policy.Validate` with the tenant's endpoints and the topology);
3. check `max_active_jobs` of the tenant's quotas (non-terminal jobs; `429` in C7);
4. insert the job, `ON CONFLICT (tenant_id, idempotency_key) DO NOTHING`; on a conflict, return the existing job with `created = false` (FR-2);
5. insert one transfer per destination (`PENDING`);
6. enqueue `ReplicateJob(jobID)` on queue `jobs` with workflow ID `job/<jobID>`, partition key the tenant, `WithEnqueueTransaction(tx)` (EXT-1);
7. commit; audit `JOB_SUBMIT`.

The workflow inputs are IDs only (SEC-7, INV-5).

## 4. Workflows (C6b, C6c)

### 4.1 `Transfer(transferID)`, queue `transfers`, workflow ID `transfer/<transferID>`, partition key the tenant

```
for {
    r := step attempt(transferID)        // plan, begin/resume, batches, commit, verify, finish
    switch r.Class {
    case done:                           return
    case Transient, Unknown (< 10 in a row):
                                         dbos.Sleep(backoff(r.Attempt))   // §5
    case ResumeInvalid:                  step reset(transferID); continue
    case Integrity (< 3):                step reset(transferID); continue
    default:                             step fail(transferID, r); return
    }
}
```

The work of one attempt is a sequence of steps, each idempotent and each starting with a **claim** (§4.2):

| Step | Does | Records (with `WHERE fence = $mine`) |
| :--- | :--- | :--- |
| `plan` | reads the job's part size (§4.4) and the source size; the part list is derived, not stored | `state = COPYING`, `bytes_total`, `attempt + 1`, `started_at` |
| `begin` | `Upload` by `Resume(resume_token)` if one is stored, else `Begin`; `ResumeInvalid` from `Resume` drops the token and begins again | `resume_token` |
| `batch` (repeated) | the next `parts_in_flight` parts not in `parts`: each is read with `ReadRange(…, ifVersion)` and written with `WritePart`, its SHA-256 computed on the read path and compared with the driver's `PartResult.SHA256` when the driver gives one | the parts and `bytes_done`, in one transaction per batch |
| `commit` | `Upload.Commit` with the parts in order (FR-9) | `state = VERIFYING` |
| `verify` | `Stat` of the destination: size and version; the driver's checksum when it has one; the transfer's digest (§4.4) against the job's | — |
| `finish` | the guarded update to `SUCCEEDED` and the policy evaluation (§4.3) | `state`, `finished_at`, lease released |

Between batches the step reads the job: a cancel (or the job's end with `on_satisfied: stop`, or the job `FAILED`) aborts the upload and ends the transfer `CANCELLED`.

Credentials are fetched inside each step that opens a driver (`secrets.Store.Fetch`), used, and zeroed when the step ends (SEC-10); a failure to fetch is `Transient`. A driver's client is opened and closed within the step.

### 4.2 Lease and fence: claimed in every step (amends `veho-job-lifecycle.md` §6.3)

The lifecycle increments the fence once, in an `acquire` step. That does not fence anything after an adoption: DBOS resumes a workflow from its last completed step, so the adopter replays `acquire`'s recorded output and would write with the **same** fence as the zombie. So every step begins with a claim, in its own short transaction:

```sql
UPDATE veho.transfers
   SET fence = CASE WHEN lease_owner = $me THEN fence ELSE fence + 1 END,
       lease_owner = $me, lease_expires_at = now() + $ttl
 WHERE id = $1 AND state NOT IN ('SUCCEEDED', 'FAILED', 'CANCELLED')
RETURNING fence
```

and every write of the step is guarded by `fence = $mine` (INV-3). The adopter's first step takes the fence; the zombie's in-flight step writes nothing at its end (0 rows: it stops), and its part uploads are idempotent per `(transfer, part_no)` (INV-3, EXT-2c). The heartbeat (§7) renews `lease_expires_at` of this executor's transfers, so a long batch does not lose its lease. A step that finds the transfer terminal returns `done`.

### 4.3 Where the job is decided

`finish`, `fail` and the cancel path end a transfer in one transaction that also, as in `veho-distribution-policy.md` §5.2: locks the job (`FOR UPDATE`), reads its transfers, calls `policy.Evaluate`, and sets the job's state and milestones. Decisions:

- **Unsatisfiable** sets the job `FAILED` and marks its non-terminal transfers `CANCELLED` (they would produce copies nobody waits for); `Retry` restarts both the `FAILED` and those `CANCELLED` transfers.
- **Satisfied** with `on_satisfied: stop` marks the others `CANCELLED`; with `complete` they keep running and only set `completed_at` and `settled_at`.
- A workflow learns of a `CANCELLED` row at its next claim (the claim's `WHERE` finds a terminal state: `done`).

### 4.4 Part size and digest

- **One part size per job**, chosen by `ReplicateJob`'s `resolve` step and stored in `source_info`: `max(engine.part_size, ceil(size / max_parts))`, within every destination driver's `MinPartSize` and `MaxPartSize`; no such size fails the job (`Permanent`). The same size for every transfer makes their digests comparable.
- **The digest** of an object is `SHA-256("veho-digest/v1" ‖ part size ‖ size ‖ SHA-256(part 0) ‖ … ‖ SHA-256(part n-1))`, computed from the parts' digests, which the core computes while streaming (FR-8, PLG-17). It survives a resume, because the parts' digests are stored. The first transfer to verify records it in `source_info` (compare-and-set); every other must match it (FR-10's "digest recorded at first read"), else `Integrity`.
- **Source change**: `resolve` records the source's version; every `ReadRange` passes it as `ifVersion`, so a changed source fails as `SourceChanged` (`Permanent`, as `veho-job-lifecycle.md` §7).

### 4.5 `ReplicateJob(jobID)`, queue `jobs`, workflow ID `job/<jobID>`

1. step `resolve`: `Stat` of the source, record `source_info` (size, version, part size); `ACCEPTED → RUNNING`. Not found is `Permanent`: the job `FAILED`.
2. enqueue one `Transfer` per transfer row (child workflows, IDs as §4.1).
3. wait for each child's result; the job's state is set by the transfers (§4.3), not here.
4. step `finalize`: if the deadline passed with the verdict open, the job `FAILED` and its transfers `CANCELLED`; audit `JOB_SETTLED`.

The deadline is also enforced while waiting: the wait is bounded by `deadline_at`, after which `finalize` runs and cancels the children (`dbos.CancelWorkflow`).

### 4.6 `Cancel`, `Retry`, `Status`

- `Cancel(tenant, job)`: sets `cancel_requested`; transfers stop at their next batch or claim (§4.1); the job is `CANCELLED` when they all are. Idempotent; audited `JOB_CANCEL`.
- `Retry(tenant, job)`: only a `FAILED` job; resets its `FAILED` and `CANCELLED` transfers (`PENDING`, `attempt = 0`, token, parts and digest cleared), the job to `RUNNING`, and enqueues a new `ReplicateJob` with workflow ID `job/<jobID>/retry-<n>` in the same transaction (EXT-1). Audited `JOB_RETRY`.
- `Status(tenant, job)`: the job and its transfers (state, bytes, attempts, last error; FR-14).

## 5. Errors and retries (C6b)

- A step never returns a driver's error to DBOS as is: it returns a result with the **class** (`pkg/plugin` classes), a sanitised message and the attempt (DBOS persists messages only, EXT-6 note); `last_error` gets the same. Messages are sanitised as in ligo (no URL query strings, no credentials: T-06).
- **The loop is the workflow's, not DBOS's step retry.** DBOS's step retry is in-process and bounded; ours must be unlimited until the deadline and survive restarts. The workflow sleeps with `dbos.Sleep`, which is durable: a crash during the sleep resumes the wait elsewhere.
- **Backoff** is the policy's (`retry.initial` [2s], ×2, `retry.max_interval` [15m]), with ±20 % jitter **derived from a hash of the transfer ID and the attempt**, so that it is deterministic under replay and still spreads transfers apart (FR-12).
- `Unknown` counts as `Transient` up to 10 in a row, then `FAILED`; `Integrity` restarts from scratch up to 3 times; a `Permanent` authentication error first re-fetches the credential once (it may have rotated, SEC-10) before failing.
- DB failover during a step: the step's statements fail, the step fails as `Transient`, and the loop retries it.

## 6. Drivers and endpoints (C6b)

- The compiled-in drivers are registered (`file`, `s3`); `plugins` in the configuration loads process plugins (`pkg/plugin/process`) and registers them under their names. Which ships how is backlog 12.
- An endpoint gives the driver (`endpoints.driver`), its configuration (`endpoints.config`, passed as is) and `credential_ref` (`db://<name>`; a JSON object of strings, decoded into `plugin.Credentials`). A secret that is not a JSON object of strings fails as `Permanent`.
- The position of an endpoint for the policy is its location in the topology (`veho.locations`).

## 7. Executors, adoption and housekeeping (C6a, C6d)

- **Heartbeat** every `heartbeat` (10 s): upsert of `veho.executors` (`last_seen = now()`, the database's clock, T-27) and renewal of this executor's leases. **Self-fencing**: when the heartbeat has not succeeded for `lease_ttl / 2`, the node cancels the context of its running steps (they stop writing; their workflows are adopted), and every claim fails `Transient` until a heartbeat succeeds again, so no step starts work meanwhile.
- **Housekeeping** (C6d) under `TryLock("veho_housekeeping")`, every `interval`: the four zombie kinds of `veho-audit-housekeeping.md` §3.1 (adoption with `ListWorkflows` + `ResumeWorkflows` for executors whose `last_seen` is older than `lease_ttl`, counting `transfers.adoptions`, cap 20 then `FAILED`; workflows unknown to DBOS; rows not marked after their workflow ended; expired leases), retention (§3.2) and the defensive check of `ACCEPTED` jobs without a workflow. Each repair is audited (`SYSTEM_*`). A failed task waits for the next pass.
- **Metrics** (C6d): `veho_jobs` and `veho_transfers` by state, `veho_transfer_bytes_total`, `veho_transfer_attempts_total` by class, `veho_adoptions_total`, `veho_heartbeat_age_seconds`, `veho_audit_dropped_total`, on `metrics.listen` with `prometheus/client_golang`. Labels carry no tenant name (T-19).

## 8. Threat model of C6

| ID | Threat | Mitigation | Residual |
| :--- | :--- | :--- | :--- |
| INV-3 | A zombie writes after an adoption | The claim of §4.2 in every step; every write guarded by the fence; part writes idempotent; ENG-6 kills and pauses an executor mid-batch | The zombie's in-flight part upload reaches the destination's staging (harmless: it is replaced or aborted) |
| INV-1 | A job accepted without its workflow | One transaction with the enqueue (EXT-1); housekeeping's defensive check | — |
| T-06 | Credentials in DBOS's tables or logs | Workflow inputs are IDs; step results carry a class and a sanitised message; credentials fetched and zeroed within a step; ENG-9 scans `dbos.workflow_status`, `dbos.operation_outputs` and the logs for a canary secret | — |
| C6-1 | A tenant starves the others | Partitioned queues by tenant with `tenant_concurrency`; `max_active_jobs` at submission | No byte-rate fairness yet (`max_bytes_in_flight` and rate limits, backlog) |
| C6-2 | A poison job kills every executor | The adoption counter (cap 20) and DBOS's `MaxRecoveryAttempts` | — |
| C6-3 | A source that changes during the copy yields a mixed object | `ifVersion` on every read; the digest compared across transfers | A driver that cannot check versions (none of today's) |
| C6-4 | A clock-skewed node expires live leases | Every time comparison is the database's `now()` (T-27) | — |
| C6-5 | A minority-partition node keeps copying | Self-fencing at `lease_ttl / 2` without a heartbeat | Its in-flight part (as INV-3) |

## 9. Acceptance

Integration tests run on the lab (PostgreSQL in region B, Garage in each region); unit tests use the `file` driver and fakes.

| ID | Test |
| :--- | :--- |
| ENG-1 | `Submit` commits the job, its transfers and its workflow together, or none (an injected failure after the inserts); the same idempotency key returns the same job (INV-1, FR-2) |
| ENG-2 | A 3-destination job (file and S3 on the three Garages) succeeds; every destination object has the source's bytes; `bytes_done`, digest and milestones set (FR-7, FR-8) |
| ENG-3 | Policy: 2 of 3 with one destination failing permanently: `SUCCEEDED` with `on_satisfied: complete`, the third `FAILED`, settled; 3 of 3 with one failing: `FAILED` at once and the others `CANCELLED` |
| ENG-4 | `on_satisfied: stop`: the remaining transfers end `CANCELLED` |
| ENG-5 | Retries: a driver that fails `Transient` k times then succeeds; `Unknown` capped at 10; `Integrity` 3 times then `FAILED`; `ResumeInvalid` restarts; the backoff and its jitter deterministic under replay |
| ENG-6 | Fencing: an executor paused (SIGSTOP) mid-batch, its work adopted by another, then resumed: exactly one executor's writes take effect, the object is correct, the zombie stops (INV-3, EXT-2c) |
| ENG-7 | Adoption: an executor killed mid-batch; housekeeping on another resumes its workflows within `lease_ttl` + `interval` (NFR-3); the transfer continues after its last committed part (FR-7) |
| ENG-8 | `Cancel` mid-copy: uploads aborted, no destination object, the job `CANCELLED`; `Retry` of a failed job succeeds |
| ENG-9 | No canary secret in DBOS's tables, the application tables or the logs after a run (INV-5, T-06) |
| ENG-10 | Self-fencing: a node whose database is blocked (the lab's partition) stops writing within `lease_ttl / 2` |
| ENG-11 | A source changed mid-copy fails the transfer `SourceChanged` |
| ENG-12 | Deadline: a job whose transfers keep failing `Transient` is `FAILED` at its deadline, the children cancelled |
| ENG-13 | Housekeeping: each zombie kind is created by hand and repaired, and audited; retention removes only settled jobs |
| ENG-14 | The AC-21 suite (C6e): EXT-1 to EXT-6 as tests |
| ENG-15 | The lab's multi-region cases (C6e): adoption across regions; the primary failed over mid-transfer, with every part written exactly once in the table |

Fuzzing: the part plan and the digest (`FuzzPlan`: any size and limits give a plan that covers the object exactly once, or a refusal). Benchmark: a 1 GiB file-to-file job end to end, against C5's PLG-17 figure. A long soak (many jobs, random kills and partitions) is a lab run for the night, proposed with C9.

## 10. Amendments to other specifications

- `veho-job-lifecycle.md` §4 and §6.3: the fence is claimed by every step (§4.2), not once by `acquire`; the retry loop is the workflow's (§5); a transfer has the steps of §4.1.
- `veho-job-lifecycle.md` §7: the implementation note on DBOS step retry is replaced by §5.
- `veho-distribution-policy.md` §5.1: an unsatisfiable job cancels its non-terminal transfers (§4.3).
- `veho-architecture.md` §4: `internal/workflows` and `internal/reconciler` are `internal/engine`.
