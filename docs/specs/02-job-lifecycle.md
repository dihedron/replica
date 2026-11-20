# 02 — Job lifecycle, workflows and retries

## 1. Data model (application schema)

All tables carry `tenant_id` and are protected by row-level security (05).

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
  BEGIN
    INSERT jobs (…)               -- ON CONFLICT (tenant_id, idempotency_key) → return existing
    INSERT transfers (one per destination, state=PENDING)
    enqueue DBOS workflow ReplicateJob  with workflowID = job.id   [VERIFY same-tx enqueue]
  COMMIT   -- synchronous cross-region commit (NFR-1)
  return 202 { job_id }
```

If DBOS cannot enqueue within the application transaction **[VERIFY]**, the
fallback is: commit the job rows only; the `reconciler` enqueues any
`ACCEPTED` job lacking a workflow (transactional outbox pattern). Because
`workflowID = job.id`, enqueue is idempotent. The 202 is returned after the
commit in both cases, so no accepted job can be lost.

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
- Source reads are range reads; `Source` MUST support `ReadRange` (04) so any
  part can be re-read independently.
- Resume token validity is checked (`Sink.Resume` returns `ErrTokenExpired`
  → fall back to restart).

## 6. Node failure, adoption and fencing

DBOS queues are durable and any listening executor dequeues new work. For
work already *started* by an executor that then dies, recovery behaviour of
the open-source runtime must be confirmed **[VERIFY]** (recovery is keyed on
executor ID; automatic takeover across executors may require DBOS Conductor).
The spec therefore does not depend on it and defines its own mechanism:

1. **Lease**: while a node runs a transfer it renews `lease_expires_at`
   (default TTL 60 s, renew every 15 s) in the same transaction as part
   progress or by heartbeat.
2. **Reconciler** (runs on every node, every 30 s; correctness does not
   require a single leader because actions are idempotent, but a
   `pg_try_advisory_lock` MAY be used to reduce redundant work): finds
   non-terminal transfers with expired lease and re-enqueues the DBOS
   workflow with the **same workflow ID**, tagged for any executor.
   **[VERIFY]** re-start of an existing workflow ID by another executor;
   if unsupported, the new execution is a fresh workflow with ID
   `"{transferID}#{fence}"` which replays from DB state (`parts`,
   `resume_token`) rather than DBOS checkpoints — design the steps so that
   they are idempotent from table state alone.
3. **Fencing**: every state-changing write includes `fence = $mine`. A zombie
   node (paused, partitioned) whose lease was taken over fails its next write
   (`0 rows affected`), aborts without touching the destination upload, and
   exits. Plugin writes are idempotent by `(transfer, part_no)`, so a zombie's
   in-flight part write is harmless (same bytes, same part number).
4. **Self-fencing on partition**: if a node cannot renew its lease for
   `TTL/2`, it stops reading/writing and drops the transfer.

## 7. Error taxonomy and retry policy

Plugins classify errors (04). Core handles by class:

| Class | Examples | Behaviour |
|-------|----------|-----------|
| `Transient` | timeouts, 5xx, throttling (429/503), connection reset, DB failover | Retry with backoff; unlimited attempts until `deadline_at` |
| `ResumeInvalid` | multipart upload expired/aborted | Discard token, restart transfer (attempt++) |
| `Integrity` | checksum/size mismatch | Restart from scratch, max 3, then `FAILED` |
| `Permanent` | 403/401 after credential refresh, destination bucket missing, source deleted, `SOURCE_CHANGED`, quota exceeded at destination | Transfer `FAILED` immediately; no further retries |
| `Unknown` | unclassified | Treated as `Transient` but capped at 10 consecutive occurrences, then `FAILED` |

Defaults (overridable in policy within tenant limits):
`initial=2s, multiplier=2, max_interval=15m, jitter=±20%, deadline=7d`.

Authentication failures first trigger a credential re-resolution (secret may
have rotated) before being deemed permanent.

A `FAILED` transfer can be retried by the client (`POST …:retry`), which
resets attempts and re-enqueues; this is the answer to "permanent" being
wrong (e.g. operator fixed bucket permissions).

## 8. Quotas and fairness

- Per-tenant `max_active_jobs` and `max_bytes_in_flight`: enforced at
  submission (reject `429`) and at transfer start (wait).
- Queue concurrency: global limit per node and per cluster; per-tenant
  fairness through round-robin / partitioned queues by `tenant_id`
  **[VERIFY queue partitioning in dbos-transact-golang v0.11.0]**.
