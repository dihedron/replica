# veho: operations and acceptance matrix

This file specifies observability, deployment and the acceptance scenarios (`AC-nn`) that every release is checked against. It is part of `veho_specification.md`.

Status: draft, 2026-11-21, for review.

## 1. Observability

- **Logs**: `slog` (already scaffolded), JSON in production; fields
  `tenant_id, job_id, transfer_id, attempt, fence, executor_id, region`.
  Never log credentials or full presigned URLs.
- **Metrics** (Prometheus): 
  `veho_jobs{state}`, `veho_transfers{state,region}`,
  `veho_bytes_copied_total{region,driver}`,
  `veho_transfer_attempts_total{class}`,
  `veho_lease_expired_total`, `veho_oldest_nonterminal_job_age_seconds`,
  `veho_db_commit_seconds`, `veho_part_copy_seconds`,
  `veho_reconciler_adoptions_total`, per-tenant variants bounded.
- **Tracing**: OpenTelemetry; one trace per job, spans per step; trace
  context persisted in the job row so resumed work continues the trace.
- **Alerts** (reference): job older than 2×expected; no reconciler tick in
  5 min; Postgres sync standby count < 1; etcd quorum lost; transfer
  failing with `Permanent` for any tenant.

## 2. Configuration

Single YAML/env config (env prefix `VEHO_`, `.env` already supported):
region/node name, DB URL list, DBOS app name, listen addr, auth (OIDC
issuer/audience), secret backends, global limits, defaults for part size /
parallelism / retry. Config is validated at startup; fail fast.

## 3. Deployment

- Reference: 2 nodes per region on OpenStack VMs or LXD (Ubuntu or Rocky/Alma) under systemd, tier 1 (`veho-architecture.md` §7; VMware and Kubernetes are tier 2), Patroni
  + etcd per region (tuned for WAN, veho-architecture.md §3.3). **No transaction-mode
  connection pooler**: DBOS holds a `LISTEN` session (veho-dbos-spike.md, EXT-4b); connect
  directly with a multi-host DSN (`target_session_attrs=read-write`),
  verified to survive failover (veho-dbos-spike.md, EXT-4a).
- Each node has a **unique, stable executor ID** (e.g. `region-host`), never
  the DBOS default `local`. (DBOS v1.9.0 has no admin server; adoption uses the public API.) Enterprise-key features (`AutoUpgrade`, `Sharding`, `SuspendMode`, Conductor) stay disabled.
- Rolling upgrades: schema migrations are forward-compatible
  (expand → migrate → contract). DBOS recovery and dequeue are filtered by
  `ApplicationVersion` (veho-dbos-spike.md, EXT-5): keep workflow code replay-compatible and
  the version stable across compatible releases; for incompatible releases,
  start the new version, let old executors drain their `PENDING` work, and only
  then stop them.
- Graceful shutdown: stop dequeueing, finish or checkpoint current part,
  release leases (`dbos.Shutdown` with timeout).

## 4. Failure scenarios (acceptance, each automated)

Environment: the lab (`veho-lab.md`): three "regions" (separate networks) in one LXD VM, the WAN emulated with netem (latency, jitter, loss) and nftables (partitions), Garage as S3 in each region.

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
| AC-18 | WAN stability: 3 Patroni/etcd members with real inter-region RTT (the lab's WAN with the measured per-link one-way latencies of `veho-architecture.md` §3.3, sublink changed at reconnect and between runs, plus the 20, 30 and 40 ms baselines), 24 h idle + light load | No spurious etcd leader change, no Patroni demotion; otherwise fall back per veho-architecture.md §3.3 (4) |
| AC-19 | Poison pill: workflow that crashes the executor on every attempt | Workflow is parked by DBOS as `MAX_RECOVERY_ATTEMPTS_EXCEEDED`; reconciler detects it, transfer marked `FAILED(Internal)`, alert raised |
| AC-20 | Rolling upgrade with in-flight transfers | Old-version work drains or is recovered by compatible executors; none stuck `PENDING` |
| AC-21 | Regression for DBOS behaviours we depend on (transactional enqueue commit/rollback, `ResumeWorkflows` on a `PENDING` orphan, zombie fencing, replay after the last completed step (in place of the retry predicate, no longer used), partitioned queues, multi-host failover), run on every DBOS version bump; the suite `test/dbos` | Same outcomes as veho-dbos-spike.md |
| AC-25 | Audit and housekeeping (`veho-audit-housekeeping.md`): chain verifies after a run; a tampered entry, a forged checkpoint and a deleted entry are each detected by `audit verify`; export, record, prune and offline verification; `DELETE` as the application role refused; appends during a prune keep the chain valid; audit writes add no cross-region round trip (acceptance latency with auditing on equals that with it off, within noise) and concurrent submissions are not serialized by the chain; the sealer never skips a late-committing row; an event buffered during a partition is written after it, and a full buffer drops and counts; gRPC and REST give the same answers, errors and audit entries, and a revoked principal's stream ends; each zombie kind (dead executor, unknown to DBOS, ended without marking, expired lease) is handled and audited; retention purges only settled jobs; one node runs the loop at a time (with backlog 32; in v1, AUDL-1 to AUDL-5 of `veho-audit.md`) | Every tamper detected; every zombie handled; no valid entry lost |
| AC-26 | Secrets (`veho-tenancy.md` §3): a dump of the database contains no plaintext secret and no unwrap key; a ciphertext moved to another tenant or name fails to decrypt; the secret API never returns a value; a node without the unwrap key refuses to start; a KEK rotation re-wraps every data key with no failed attempt and the old KEK retires; an unwrap failure makes an attempt `Transient`, not a lost job; the audit key rotates with a checkpoint signed by the old key | No plaintext outside memory; every negative case refused |
| AC-22 | Release verification: build a release with a throwaway key (`make snapshot` with `GPG_FINGERPRINT` and `GPG_KEY_FILE`); then verify the checksums file against `packaging/signing-key.asc`'s counterpart, every artifact against the checksums, and the `.deb` and `.rpm` signatures; `make release` without the key refuses (T-21) | All verifications pass; the unsigned release is refused |
| AC-23 | Distribution policy: 9 destinations (3 per region), `min_copies` 7: the job is `SUCCEEDED` at the seventh copy and `FAILED` as soon as 3 destinations fail permanently; `min_copies` 5 with spread `region` 3: 3+2+0 is not done, 2+2+1 is; a policy over AZs and regions (5 AZs across 3 regions); `satisfied_at`, `completed_at` and `settled_at` set separately (a permanent failure settles without completing); destinations in locations with no control-plane node; two transfers that end at the same instant give one consistent verdict; `on_satisfied: stop` cancels the rest, `complete` finishes them and a later failure does not change the job; source in B; invalid policies are refused (`veho-distribution-policy.md` §3) | Verdicts as specified; no lost or double verdict under concurrency |
| AC-24 | Latency and jitter envelope: lab WAN sweep of RTT (25, 50, 100, 200, 400, 800 ms) by jitter (none, ±10 %, ±25 %, ±50 %, baseline shift at reconnect), the profile's timeouts, 1 hour of light load per cell; record elections, demotions, acceptance p50/p99, primary-kill recovery and adoption time (`veho-latency-envelope.md` §6) | No spurious election or demotion in the cells a profile claims; the measurements replace the estimates |

## 5. Test strategy

- Unit: state machines (table-driven), policy validation, retry/backoff,
  fencing SQL.
- Property-based: part planning (sizes/limits), resume at any part boundary.
- Plugin conformance suite (veho-plugins.md §4).
- Integration: PostgreSQL and Garage from the lab (or any instance named by the `VEHO_TEST_*` variables), real DBOS.
- Chaos/acceptance: AT-* above, runnable via `make acceptance`.
- Each test names the requirement IDs it covers; CI reports uncovered IDs.

## 6. Capacity assumptions

≤ 50 concurrent transfers cluster-wide; objects up to 5 TiB; DB write rate
dominated by part commits (~1 commit per batch of 4 × 64 MiB ≈ 256 MiB, i.e.
for 1 Gbit/s ≈ 0.5 commits/s per transfer — negligible).

## 7. Releases and their verification

Releases are signed (T-21, `veho-threat-model.md`). The packaging key is an RSA 4096 GPG key created once by `packaging/create-signing-key.sh` on a trusted machine, in a throwaway keyring: it publishes the public half as `packaging/signing-key.asc` (committed), stores the private half and its passphrase as the repository secrets `GPG_PRIVATE_KEY` and `GPG_PASSPHRASE`, and writes the private key and a revocation certificate to a backup directory that goes to offline storage.

- `make release` requires `GPG_FINGERPRINT` and `GPG_KEY_FILE` (`signing-check` refuses half a configuration); `make snapshot` signs when both are set and skips signing otherwise.
- `.goreleaser.yaml` signs the checksums file (a detached, armored signature, `checksums.txt.asc`) and the `rpm` and `deb` packages. The checksums file covers every archive, package, SBOM and, once they exist, the release documents.
- `release.yml` imports the key, checks that the published public key is the one that signs, writes the key for `nfpm`, runs `make release` and removes the key.
- An operator verifies a release by importing `packaging/signing-key.asc`, checking the fingerprint against the one in the release notes, running `gpg --verify checksums.txt.asc checksums.txt` and `sha256sum --check checksums.txt`. The Setup Guide repeats this.
- The key expires in three years by default (`KEY_EXPIRY`); rotating it means removing `packaging/signing-key.asc` on purpose and running the script again.
