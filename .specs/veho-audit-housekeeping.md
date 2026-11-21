# veho: audit log and housekeeping

This file specifies the audit log, the periodic housekeeping (zombie tasks, retention, pruning) and how they are built. It is part of `veho_specification.md`.

Status: draft, 2026-11-21, for review. The design is copied from ligo (`ligo/.specs/ligo-pruning.md`, `ligo/internal/audit`, `ligo/internal/workflows/sweeper.go`), which already has it built and tested, and adapted where veho differs (§6). The implementation is copied too, as far as it is feasible (§7).

## 1. Audit log

The audit log is append-only, hash-chained and checkpointed with signatures, so that tampering or deletion is detectable (`veho-threat-model.md` T-35).

- **AUD-1** Every security-relevant action appends one entry in the same transaction as the change it records: job submission, cancel and retry; policy, grant and endpoint changes; credential reference changes; tenant and identity-binding changes; authentication and authorization failures; break-glass; and the system actions of §5 (`SYSTEM_*`, which name no resource).
- **AUD-2** An entry holds: time, tenant, principal (and its groups), action, resource type, id and name, job and transfer ids, request id, workflow id, and a **non-secret** JSON snapshot (INV-5). It never holds a credential, a token or personal data beyond the principal's identifier.
- **AUD-3** Entries form a chain: `entry_hash = SHA-256(prev_hash || JCS(row))`, where JCS is the canonical JSON form (RFC 8785) of the entry, so a hash is recomputed from the stored row alone. The first entry's `prev_hash` is a fixed genesis value. The chain is global (one per installation) but **it is built asynchronously, not by the writer** (§1.1): a writer inserts a plain row, and a single *sealer* chains the rows later, in batches.
- **AUD-4** At most once an hour the sealer writes a **signed checkpoint**: the id and `entry_hash` of the newest sealed entry, signed with the audit key (SEC-28). The private key is stored wrapped by the KEK and held in memory by the nodes that run the sealer; the public keys are published, and a new key version is announced by a checkpoint signed with the previous one.
- **AUD-5** The application role has only `SELECT` and `INSERT` on the audit table, and a trigger refuses `UPDATE` and `DELETE`; the only deletion path is the pruning function of §4.
- **AUD-6** `veho audit verify` checks the chain (incrementally, in a background loop on one node, and in full on demand) and every checkpoint's signature; a break is reported with the first broken entry, raises an alert and a metric, and is never repaired silently.
- **AUD-7** A tenant admin reads the audit entries of its own tenant (`GET /v1/audit`, filtered by tenant by row-level security); a platform administrator reads the system entries; the full chain is verified by the platform.
- **AUD-8** Audit entries are also written to the log stream (structured, `slog`), so that a SIEM keeps an independent copy; that copy is the authoritative record if the database is compromised (T-35, R-10).

### 1.1 The write path over distance

The state store commits across regions: a commit waits for the synchronous standby (about 10 to 40 ms here, more on a wider deployment: `veho-latency-envelope.md`). The audit log must not add a quorum round trip per entry, and must not serialize writers behind that round trip. ligo, from which the design is copied, appends under a chain lock inside the writer's transaction; that is cheap on one site and wrong here, because the lock would be held across the synchronous commit, so every audited transaction of every tenant would queue behind the previous commit (at most 1/RTT audited transactions per second, about 25 to 30 here).

The design therefore separates writing from chaining:

- **AUD-10 Atomic entries (state changes).** An action that changes state (a job accepted, cancelled or retried; a policy, grant, endpoint, tenant or identity-binding change; a housekeeping repair) writes its audit row **in the same transaction as the change**, as a plain `INSERT` with no chain fields and no lock. It rides the commit that the change needs anyway: **no extra round trip**, no contention between writers, and the change cannot commit without its audit entry.
- **AUD-11 Buffered entries (no state change).** An event that changes nothing in the state store (an authentication failure, an authorization denial, a read, a rejected request) is written to the structured log stream at once (AUD-8) and to a bounded in-memory queue with a local spill file, and **inserted in batches in the background**, one commit per batch, never on the request path. During a partition, or when the state store has no quorum, the queue holds the events; when it is full, the oldest are dropped, counted (`veho_audit_dropped_total`) and the drop is logged. These events may be lost if the node crashes before the batch is written; the SIEM copy covers them (R-11).
- **AUD-12 The sealer.** One node at a time (the housekeeping lock, §3) reads the rows not yet chained in id order, computes `prev_hash` and `entry_hash` for a batch, and updates them in **one transaction per batch**, so the cost is one commit per batch, not per entry. It only seals rows whose inserting transaction is committed and visible to all (below the snapshot's xmin horizon), so a row that commits late can never be skipped. It then writes the signed checkpoint (AUD-4). Its lag is a metric (`veho_audit_unsealed_entries`) and an alert.
- **AUD-13** The tamper-evidence of an entry starts when it is sealed, normally seconds after it was written. Before that it is protected by the append-only rules (AUD-5), by the log stream copy (AUD-8) and by the transaction it shares with the change it records. Export, verification and pruning work only on sealed entries and checkpoints.

## 2. Export and offline verification

As in ligo (`ligo-pruning.md` §3.2 and §3.3):

- `veho audit export [--through ID] --out FILE` writes a JSON Lines file: a header (range, the `prev_hash` of the first entry, the public keys), the signed checkpoints of the range plus the newest one below it (the anchor), then every entry with its hash and its canonical row. The file must not exist, is verified after writing, and only then is the export **recorded** (`POST /v1/admin/audit/exports` with the last entry id and the file's SHA-256), in one transaction with a `SYSTEM_AUDIT_EXPORTED` entry.
- `veho audit verify --file FILE` verifies a file without the server: signatures, the anchor, every hash, ids increasing, the last entry being a checkpoint. An auditor should take the public keys from the secret backend, not from the file under test.
- Exporting needs the platform's full-access role: an export spans every tenant and recording one allows deletion.

## 3. Housekeeping loop

One loop in `veho serve` runs housekeeping (including the audit sealer, AUD-12); **only one node runs it at a time**, under a PostgreSQL advisory lock (`veho_sweeper`) taken with `TryLock`, so the others skip. It runs every 30 s (configurable) and a failed task waits for the next pass, never stopping the loop. Nothing runs while maintenance mode is on.

### 3.1 Zombie tasks

A zombie is work that no live executor is doing. Four kinds, each handled:

| Zombie | Detection | Action |
| :--- | :--- | :--- |
| **Orphan of a dead executor**: a workflow `PENDING` on an executor that stopped heartbeating | `ListWorkflows(PENDING)` against the executor registry (`last_seen` older than the lease TTL) | `ResumeWorkflows`; the node that dequeues it becomes the executor. The reconciler counts its own adoptions, because `ResumeWorkflows` resets DBOS's recovery counter; a job that keeps killing executors stops at the ceiling and fails (AC-19). `veho-job-lifecycle.md` §6. |
| **Unknown to DBOS**: a job or transfer non-terminal and quiet for longer than `stale` (default 60 s) whose workflow DBOS does not know | row scan against `GetWorkflowStatus` | Start the workflow again (idempotent by workflow ID). With a transactional acceptance this means a bug or a restore, so it is audited and counted. |
| **Ended without marking**: the workflow is `SUCCESS`, `ERROR` or `CANCELLED` but the job or transfer row is still non-terminal | same scan | Mark the row from the workflow's outcome (the compare-and-set of the lifecycle) and audit it. |
| **Expired lease**: a transfer whose lease expired and was never adopted | database clock against `lease_expires_at` (T-27) | Release it so that the executor path of the first row takes it. |

The fence (`veho-job-lifecycle.md` §6) stays the protection against the zombie that is not dead but paused: it finishes its in-flight step and is refused at its next checkpoint.

### 3.2 Retention

| What | Default | Setting |
| :--- | :--- | :--- |
| Finished DBOS workflows and their step outputs | 168 h | `storage.dbos.workflow_retention` |
| Finished jobs, with their transfers and parts | 90 days after `settled_at` | `jobs.retention` |
| Executor registry rows of nodes gone | 7 days | constant |
| Expired or revoked Web UI sessions | 7 days | constant |

Retention applies only to jobs that are settled. A value of 0 keeps everything. A value below 24 h fails the configuration check, so a typo cannot empty a table.

### 3.3 Audit retention

Default **off**: `storage.audit.retention` unset keeps every entry. When set (at least 24 h) the pruning of §4 applies. The operator exports before anything is deleted.

## 4. Audit pruning

Copied from ligo (`ligo-pruning.md` §3.4 to §3.6):

- The boundary B is the largest audit id that is a signed checkpoint whose signature verifies, at or below the newest recorded export, and older than `now - retention`. Entries up to B are deleted; B's checkpoint stays and is the anchor the verification accepts for a pruned head. Checkpoints are never deleted.
- The pruning runs in the verify loop after a valid pass, at most once every 24 h, and skips when the signer is unavailable or the last verification was not valid. It appends `SYSTEM_AUDIT_PRUNED` with B, the count and the export it relied on.
- Deleting does not open T-35: the application role has no `DELETE`; a `SECURITY DEFINER` function owned by the migrator role, `audit_prune(through)`, takes the chain's lock, refuses unless `through` is a checkpoint at or below the newest recorded export and signed more than 24 hours ago, and sets a transaction-local variable that the trigger requires. Setting the variable alone gives nothing.
- Metrics: `veho_audit_pruned_entries_total`, `veho_audit_oldest_entry_timestamp_seconds`.

## 5. System actions

New audit actions: `SYSTEM_AUDIT_EXPORTED`, `SYSTEM_AUDIT_PRUNED`, `SYSTEM_ZOMBIE_ADOPTED` (an orphan resumed), `SYSTEM_ZOMBIE_RESTARTED`, `SYSTEM_ZOMBIE_REPAIRED`, `SYSTEM_RETENTION_PURGED` (counts per table).

## 6. What differs from ligo

- Ligo appends under a chain lock inside the writer's transaction. Veho does not: it separates the write from the chaining (§1.1), because commits cross regions.
- Ligo signs checkpoints with a non-exportable key in Vault transit; veho has no Vault in v1 (OI-16), so its signing key is held by the nodes (SEC-28). A node compromise can therefore steal the key and re-sign a rewritten history; the SIEM copy and the exports are the defence (R-10, R-12). Moving the key into Vault later (backlog 26) restores ligo's property.
- The audit entries carry `tenant_id`, `job_id` and `transfer_id` instead of ligo's resource model, and tenants read their own entries (AUD-7).
- Ligo's sweeper handles bindings, change requests and sessions; veho's handles jobs, transfers and executors (§3). Ligo's outbox retention does not apply until webhooks exist (backlog 11).
- Veho adds the executor registry and the adoption counter of `veho-job-lifecycle.md` §6; ligo's `Pending` and `Resume` wrappers are the same two public DBOS calls.

## 7. How it is built

- **Copy, then adapt.** The ligo packages are internal to that module and cannot be imported. Packages to copy into `internal/`: `audit` (chain, checkpoints, export, verify, prune), `jcs` (canonical JSON), the advisory-lock helper from `store`, and the shape of the sweeper and reconciler loops. Their tests and integration tests come along, adapted to veho's rows.
- Keep a **sync note** per copied package (source commit, divergences) like the `.mk` files, so fixes can move both ways.
- Once both projects have stabilized, extracting the audit and the lock helper into a shared module is the cleaner end state (backlog 22); it is not done now, to keep ligo untouched.
- Tests first (AGENTS.md rule 4): the boundary rule, export and offline verification with tampered entries, a forged checkpoint, prune then verify, appends during a prune, and each zombie kind (AC-25).

## 8. Requirements

- **AUD-9** Audit retention is off by default; pruning never deletes an entry that no export covers.
- **AUD-14** No audit write waits for a cross-region round trip of its own: a state change's entry shares the change's commit (AUD-10), every other entry is asynchronous (AUD-11) and chaining is batched (AUD-12).
- **HK-1** Housekeeping runs on one node at a time, under an advisory lock.
- **HK-2** Every zombie kind of §3.1 is detected and handled, and each action is audited.
- **HK-3** Retention values below 24 h are refused at startup.
- **HK-4** A failed housekeeping task never stops the loop.
