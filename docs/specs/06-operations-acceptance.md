# 06 — Operations & acceptance

## 1. Observability

- **Logs**: `slog` (already scaffolded), JSON in production; fields
  `tenant_id, job_id, transfer_id, attempt, fence, executor_id, region`.
  Never log credentials or full presigned URLs.
- **Metrics** (Prometheus): 
  `replica_jobs{state}`, `replica_transfers{state,region}`,
  `replica_bytes_copied_total{region,driver}`,
  `replica_transfer_attempts_total{class}`,
  `replica_lease_expired_total`, `replica_oldest_nonterminal_job_age_seconds`,
  `replica_db_commit_seconds`, `replica_part_copy_seconds`,
  `replica_reconciler_adoptions_total`, per-tenant variants bounded.
- **Tracing**: OpenTelemetry; one trace per job, spans per step; trace
  context persisted in the job row so resumed work continues the trace.
- **Alerts** (reference): job older than 2×expected; no reconciler tick in
  5 min; Postgres sync standby count < 1; etcd quorum lost; transfer
  failing with `Permanent` for any tenant.

## 2. Configuration

Single YAML/env config (env prefix `REPLICA_`, `.env` already supported):
region/node name, DB URL list, DBOS app name, listen addr, auth (OIDC
issuer/audience), secret backends, global limits, defaults for part size /
parallelism / retry. Config is validated at startup; fail fast.

## 3. Deployment

- Reference: 2 nodes per region (Kubernetes Deployment or systemd), Patroni
  + etcd per region (tuned for WAN, 01 §3.3). **No transaction-mode
  connection pooler**: DBOS holds a `LISTEN` session (08, VERIFY-4b); connect
  directly with a multi-host DSN (`target_session_attrs=read-write`),
  verified to survive failover (08, VERIFY-4a).
- Each node has a **unique, stable executor ID** (e.g. `region-host`), never
  the DBOS default `local`. (DBOS v1.9.0 has no admin server; adoption uses the public API.) Enterprise-key features (`AutoUpgrade`, `Sharding`, `SuspendMode`, Conductor) stay disabled.
- Rolling upgrades: schema migrations are forward-compatible
  (expand → migrate → contract). DBOS recovery and dequeue are filtered by
  `ApplicationVersion` (08, VERIFY-5): keep workflow code replay-compatible and
  the version stable across compatible releases; for incompatible releases,
  start the new version, let old executors drain their `PENDING` work, and only
  then stop them.
- Graceful shutdown: stop dequeueing, finish or checkpoint current part,
  release leases (`dbos.Shutdown` with timeout).

## 4. Failure scenarios (acceptance, each automated)

Environment: docker-compose / kind with three "regions" (separate networks)
using Toxiproxy for latency and partitions; MinIO as S3 in each region.

| ID | Scenario | Expected |
|----|----------|----------|
| AT-1 | Happy path 10 GiB, B and C | Job `SUCCEEDED`, digests equal, progress monotonic |
| AT-2 | `kill -9` executor mid-transfer | Another node adopts within lease+30 s, resumes from last part, final object correct |
| AT-3 | Kill the accepting node right after `202` | Job still runs to completion |
| AT-4 | Kill all nodes in region A | Nodes in B/C continue (source reachable) |
| AT-5 | Postgres primary host/region loss | Failover ≤ 5 min; no job lost; in-flight transfers resume |
| AT-6 | Partition region A from B+C | A returns 503 on submit; B/C continue; A's transfers self-fence; no double commit |
| AT-7 | Zombie: SIGSTOP node past lease, then SIGCONT | Zombie's writes rejected by fence; one final object |
| AT-8 | Destination returns 503 for 30 min | Retries with backoff; completes after recovery |
| AT-9 | Destination 403 | Transfer `FAILED(Permanent)`; other destination unaffected; `:retry` works after fix |
| AT-10 | Corrupt a part in flight (fault injection) | Integrity mismatch detected, retried, eventually correct |
| AT-11 | Source mutated mid-copy | `SOURCE_CHANGED`, policy-dependent restart/fail |
| AT-12 | Region A down after C completed, B pending | B sourced from C (FR-10) |
| AT-13 | Duplicate `POST` with same Idempotency-Key | Same job ID, one workflow |
| AT-14 | Tenant isolation: T1 reads/cancels T2 job, uses T2 endpoint, SSRF URL | 404/403, no effect |
| AT-15 | Quota exceeded | 429; running jobs unaffected |
| AT-16 | Cancel mid-transfer | Uploads aborted, no object under final name, `CANCELLED` |
| AT-17 | Soak: 50 concurrent jobs, random node/DB kills (chaos) | All jobs eventually `SUCCEEDED`, zero duplicates/corruption |
| AT-18 | WAN stability: 3 Patroni/etcd members with real inter-region RTT + jitter (Toxiproxy), 24 h idle + light load | No spurious etcd leader change, no Patroni demotion; otherwise fall back per 01 §3.3 (4) |
| AT-19 | Poison pill: workflow that crashes the executor on every attempt | Workflow is parked by DBOS as `MAX_RECOVERY_ATTEMPTS_EXCEEDED`; reconciler detects it, transfer marked `FAILED(Internal)`, alert raised |
| AT-21 | Regression for DBOS behaviours we depend on (transactional enqueue commit/rollback, `ResumeWorkflows` on a `PENDING` orphan, zombie fencing, retry predicate, partitioned queues, multi-host failover), run on every DBOS version bump; promote `spikes/dbos` into this suite | Same outcomes as 08 |
| AT-20 | Rolling upgrade with in-flight transfers | Old-version work drains or is recovered by compatible executors; none stuck `PENDING` |

## 5. Test strategy

- Unit: state machines (table-driven), policy validation, retry/backoff,
  fencing SQL.
- Property-based: part planning (sizes/limits), resume at any part boundary.
- Plugin conformance suite (04 §4).
- Integration: Postgres via testcontainers, MinIO, real DBOS.
- Chaos/acceptance: AT-* above, runnable via `make acceptance`.
- Each test names the requirement IDs it covers; CI reports uncovered IDs.

## 6. Capacity assumptions

≤ 50 concurrent transfers cluster-wide; objects up to 5 TiB; DB write rate
dominated by part commits (~1 commit per batch of 4 × 64 MiB ≈ 256 MiB, i.e.
for 1 Gbit/s ≈ 0.5 commits/s per transfer — negligible).
