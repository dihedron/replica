# replica: operations and acceptance matrix

This file specifies observability, deployment and the acceptance scenarios (`AC-nn`) that every release is checked against. It is part of `replica_specification.md`.

Status: draft, 2026-11-21, for review.

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
  + etcd per region (tuned for WAN, replica-architecture.md §3.3). **No transaction-mode
  connection pooler**: DBOS holds a `LISTEN` session (replica-dbos-spike.md, EXT-4b); connect
  directly with a multi-host DSN (`target_session_attrs=read-write`),
  verified to survive failover (replica-dbos-spike.md, EXT-4a).
- Each node has a **unique, stable executor ID** (e.g. `region-host`), never
  the DBOS default `local`. (DBOS v1.9.0 has no admin server; adoption uses the public API.) Enterprise-key features (`AutoUpgrade`, `Sharding`, `SuspendMode`, Conductor) stay disabled.
- Rolling upgrades: schema migrations are forward-compatible
  (expand → migrate → contract). DBOS recovery and dequeue are filtered by
  `ApplicationVersion` (replica-dbos-spike.md, EXT-5): keep workflow code replay-compatible and
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
| AC-1 | Happy path 10 GiB, B and C | Job `SUCCEEDED`, digests equal, progress monotonic |
| AC-2 | `kill -9` executor mid-transfer | Another node adopts within lease+30 s, resumes from last part, final object correct |
| AC-3 | Kill the accepting node right after `202` | Job still runs to completion |
| AC-4 | Kill all nodes in region A | Nodes in B/C continue (source reachable) |
| AC-5 | Postgres primary host/region loss | Failover ≤ 5 min; no job lost; in-flight transfers resume |
| AC-6 | Partition region A from B+C | A returns 503 on submit; B/C continue; A's transfers self-fence; no double commit |
| AC-7 | Zombie: SIGSTOP node past lease, then SIGCONT | Zombie's writes rejected by fence; one final object |
| AC-8 | Destination returns 503 for 30 min | Retries with backoff; completes after recovery |
| AC-9 | Destination 403 | Transfer `FAILED(Permanent)`; other destination unaffected; `:retry` works after fix |
| AC-10 | Corrupt a part in flight (fault injection) | Integrity mismatch detected, retried, eventually correct |
| AC-11 | Source mutated mid-copy | `SOURCE_CHANGED`, policy-dependent restart/fail |
| AC-12 | Region A down after C completed, B pending | B sourced from C (FR-10) |
| AC-13 | Duplicate `POST` with same Idempotency-Key | Same job ID, one workflow |
| AC-14 | Tenant isolation: T1 reads/cancels T2 job, uses T2 endpoint, SSRF URL | 404/403, no effect |
| AC-15 | Quota exceeded | 429; running jobs unaffected |
| AC-16 | Cancel mid-transfer | Uploads aborted, no object under final name, `CANCELLED` |
| AC-17 | Soak: 50 concurrent jobs, random node/DB kills (chaos) | All jobs eventually `SUCCEEDED`, zero duplicates/corruption |
| AC-18 | WAN stability: 3 Patroni/etcd members with real inter-region RTT + jitter (Toxiproxy), 24 h idle + light load | No spurious etcd leader change, no Patroni demotion; otherwise fall back per replica-architecture.md §3.3 (4) |
| AC-19 | Poison pill: workflow that crashes the executor on every attempt | Workflow is parked by DBOS as `MAX_RECOVERY_ATTEMPTS_EXCEEDED`; reconciler detects it, transfer marked `FAILED(Internal)`, alert raised |
| AC-21 | Regression for DBOS behaviours we depend on (transactional enqueue commit/rollback, `ResumeWorkflows` on a `PENDING` orphan, zombie fencing, retry predicate, partitioned queues, multi-host failover), run on every DBOS version bump; promote `test/spikes/dbos` into this suite | Same outcomes as replica-dbos-spike.md |
| AC-22 | Release verification: build a release with a throwaway key (`make snapshot` with `GPG_FINGERPRINT` and `GPG_KEY_FILE`); then verify the checksums file against `packaging/signing-key.asc`'s counterpart, every artifact against the checksums, and the `.deb` and `.rpm` signatures; `make release` without the key refuses (T-21) | All verifications pass; the unsigned release is refused |
| AC-20 | Rolling upgrade with in-flight transfers | Old-version work drains or is recovered by compatible executors; none stuck `PENDING` |

## 5. Test strategy

- Unit: state machines (table-driven), policy validation, retry/backoff,
  fencing SQL.
- Property-based: part planning (sizes/limits), resume at any part boundary.
- Plugin conformance suite (replica-plugins.md §4).
- Integration: Postgres via testcontainers, MinIO, real DBOS.
- Chaos/acceptance: AT-* above, runnable via `make acceptance`.
- Each test names the requirement IDs it covers; CI reports uncovered IDs.

## 6. Capacity assumptions

≤ 50 concurrent transfers cluster-wide; objects up to 5 TiB; DB write rate
dominated by part commits (~1 commit per batch of 4 × 64 MiB ≈ 256 MiB, i.e.
for 1 Gbit/s ≈ 0.5 commits/s per transfer — negligible).

## 7. Releases and their verification

Releases are signed (T-21, `replica-threat-model.md`). The packaging key is an RSA 4096 GPG key created once by `packaging/create-signing-key.sh` on a trusted machine, in a throwaway keyring: it publishes the public half as `packaging/signing-key.asc` (committed), stores the private half and its passphrase as the repository secrets `GPG_PRIVATE_KEY` and `GPG_PASSPHRASE`, and writes the private key and a revocation certificate to a backup directory that goes to offline storage.

- `make release` requires `GPG_FINGERPRINT` and `GPG_KEY_FILE` (`signing-check` refuses half a configuration); `make snapshot` signs when both are set and skips signing otherwise.
- `.goreleaser.yaml` signs the checksums file (a detached, armored signature, `checksums.txt.asc`) and the `rpm` and `deb` packages. The checksums file covers every archive, package, SBOM and, once they exist, the release documents.
- `release.yml` imports the key, checks that the published public key is the one that signs, writes the key for `nfpm`, runs `make release` and removes the key.
- An operator verifies a release by importing `packaging/signing-key.asc`, checking the fingerprint against the one in the release notes, running `gpg --verify checksums.txt.asc checksums.txt` and `sha256sum --check checksums.txt`. The Setup Guide repeats this.
- The key expires in three years by default (`KEY_EXPIRY`); rotating it means removing `packaging/signing-key.asc` on purpose and running the script again.
