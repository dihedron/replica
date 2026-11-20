# 01 — Architecture

## 1. Topology

```
        Region A                    Region B                    Region C
   ┌───────────────┐          ┌───────────────┐          ┌───────────────┐
   │ replicad A1,A2│          │ replicad B1,B2│          │ replicad C1,C2│
   │ (API+executor)│          │ (API+executor)│          │ (API+executor)│
   └───────┬───────┘          └───────┬───────┘          └───────┬───────┘
           │ SQL                      │ SQL                      │ SQL
   ┌───────▼───────┐ sync/async ┌─────▼─────────┐ sync/async ┌───▼───────────┐
   │ Postgres      │◄──────────►│ Postgres      │◄──────────►│ Postgres      │
   │ (primary)     │ streaming  │ (sync standby)│ streaming  │ (sync standby)│
   │ + Patroni     │ replication│ + Patroni     │ replication│ + Patroni     │
   │ + etcd member │            │ + etcd member │            │ + etcd member │
   └───────────────┘            └───────────────┘            └───────────────┘
   Source storage (S3/Glance)   Dest storage B               Dest storage C
```

- All `replicad` nodes are **identical and symmetric**. "Node A" is simply
  whichever node accepted the request. Any node can accept API calls and any
  node can execute any transfer (FR-6).
- Nodes are stateless; all durable state lives in PostgreSQL (and in the
  storage systems themselves).
- Nodes connect to the current primary (via Patroni-aware endpoint, e.g.
  HAProxy/`target_session_attrs=read-write` with all three hosts listed).

## 2. Why DBOS + PostgreSQL

The repository already scaffolds DBOS Transact for Go. DBOS gives durable
workflows (steps checkpointed in Postgres, automatic replay), durable queues,
and workflow IDs usable as idempotency keys, with only Postgres as
infrastructure. This matches G7 and the low-concurrency target.

Alternatives considered (see 07, ADR-1): Temporal (heavier to operate; strong
fit but a second stateful system), a hand-rolled outbox + leases on Postgres
(simple but re-implements DBOS), CockroachDB/YugabyteDB as multi-region store
(DBOS Go has a CockroachDB code path, but it pays the same WAN quorum cost; see §3.3).

## 3. State store: PostgreSQL across three regions

- One writable primary; two standbys in the other regions.
- **Synchronous quorum commit**: `synchronous_standby_names = 'ANY 1 (b, c)'`,
  `synchronous_commit = on`. A commit is acknowledged only once at least one
  *other region* has flushed it ⇒ **RPO = 0 under loss of any one region**
  (NFR-1), including loss of the primary's region.
- **Failover**: Patroni with a 3-member etcd (one per region) as DCS.
  Leader election requires a regional majority; a candidate must be one of the
  synchronous standbys so no committed transaction is lost.
- **Consequence (CAP)**: a region partitioned from the other two cannot
  commit and therefore cannot accept jobs; the majority side fails over and
  continues (NFR-2). This is intentional.
- **Cost**: each commit waits one cross-region RTT. Acceptable at the target
  concurrency; the design therefore **batches progress writes** (per part, not
  per read) and never does per-chunk commits.
- Region choice: the spec is region-agnostic (see §3.3 for latency limits). Reference deployment needs three
  regions with RTT ≤ 150 ms between any pair; see 07 Q-2.
- Backups: continuous WAL archiving + PITR to object storage in a different
  region than the primary.

### 3.3 Consensus over WAN latency (etcd/Raft concern)

Raft-based stores degrade when members are far apart, and this is a real risk
here. What it does and does not affect:

- **Not affected: commit latency.** Job commits wait for the Postgres
  *synchronous standby* (streaming replication, no Raft). etcd is touched only
  for Patroni's leader key (a few small writes every `loop_wait`).
- **Affected: failure detection and failover stability.** An etcd write needs
  a majority round trip. With high RTT or jitter, default timeouts cause
  spurious leader elections, and Patroni demotes the primary whenever it
  cannot reach the DCS in time ⇒ needless failovers or flapping.

Mitigations, in order of preference:

1. **Tune for the WAN** (default posture). etcd: `heartbeat-interval` ≈ the
   max inter-region RTT, `election-timeout` ≥ 10 × heartbeat (etcd tuning
   guidance; defaults are 100 ms / 1000 ms and are too tight for RTT >
   ~50 ms), snapshot/quota defaults unchanged. Patroni: raise `ttl`,
   `loop_wait`, `retry_timeout` (must satisfy
   `loop_wait + 2 × retry_timeout ≤ ttl`), e.g. 60 / 15 / 15 for RTT ~150 ms.
   Cost: slower failover detection; this is why NFR-3 allows 5 minutes.
2. **Place the third etcd member where RTT is lowest** (it can be a tiny
   witness VM, no Postgres, in any site). Only a majority round trip matters,
   so the closest pair of members dominates.
3. **Keep etcd off the commit path and do not share it** with other
   workloads; dedicated disks (fsync latency also matters).
4. **Fallback if tuning proves insufficient:** Patroni *without* automatic
   cross-region failover: automatic failover stays inside the primary's region
   (local etcd, two local Postgres nodes, one synchronous), plus a synchronous
   standby in a remote region for RPO = 0, and **operator-approved**
   cross-region promotion (runbook + `patronictl failover`). RTO for region
   loss becomes human-bound (NFR-3 relaxed for that case) but nothing can
   flap over the WAN. Given low concurrency this is acceptable if mitigation 1
   fails validation.
5. Distributed SQL (CockroachDB, supported by DBOS Go) is **not** an escape:
   it runs Raft per range and pays the same WAN quorum on every write.

Gate: before committing to option 1, run acceptance test **AT-18** (06 §4):
inject the real inter-region RTT + jitter for 24 h and count leader changes and
Patroni demotions; pass criterion is zero spurious failovers.

The DBOS system database and replica's application tables live in the same
cluster (separate schemas), so job creation and workflow enqueue can occur in
**one transaction** (see 02 §3).

## 4. Components (inside one `replicad` process)

| Component | Responsibility |
|-----------|----------------|
| `api` | HTTP server, authn/z, validation, idempotency, job CRUD |
| `store` | Application tables (jobs, transfers, endpoints, tenants, quotas); repository interfaces; RLS session setup |
| `engine` | DBOS context, workflow/step registration, queues; thin wrapper (replaces current `workflow` package globals) |
| `workflows` | `ReplicateJob`, `Transfer` workflows (02) |
| `reconciler` | Periodic scan; adopts orphaned work, enforces deadlines, expires leases (02 §6) |
| `plugin` | Registry and contracts for storage drivers (04) |
| `driver/*` | Built-in plugins: `s3`, `glance`, `file` (testing), `http` (read-only source) |
| `tenant` | Tenant resolution, quota accounting, secret resolution (05) |
| `obs` | slog, Prometheus, OpenTelemetry |

Suggested repo layout (current `step/` and `workflow/` experiments are
scaffolding to be replaced):

```
cmd/replicad/          main, flags (go-flags), config loading
internal/api/          http handlers, openapi
internal/store/        sql, migrations, repositories
internal/engine/       dbos wrapper
internal/workflows/    replicate.go, transfer.go
internal/reconciler/
internal/tenant/
internal/obs/
plugin/                public plugin API (stable package)
plugin/conformance/    reusable plugin test suite
drivers/{s3,glance,file,http}/
docs/specs/
```

## 5. Data plane

- The executor node reads from the source and writes to the destination
  **directly** (streaming through the node). No intermediate storage.
- Node selection is not locality-aware in v1. Optimisation (SHOULD, post-v1):
  prefer an executor in the destination's region to keep the write local and
  the long hop on the read side, or vice-versa; make it a queue/tag decision
  (`executor_region` on the transfer).
- Part pipeline: reader goroutine pool → bounded channel → writer; SHA-256
  computed on the read path; backpressure through the channel.
- Bandwidth control: per-node and per-tenant token-bucket limiters.

## 6. Failure model

Assumed: crash-stop nodes, process restarts, network partitions between
regions, storage backend transient errors/throttling, Postgres failover.
Not assumed: byzantine nodes; silent storage corruption is detected by
checksum verification (FR-8) but not repaired.

| Failure | Effect | Recovery |
|---------|--------|----------|
| Node crash mid-transfer | Transfer lease expires | Reconciler on another node re-enqueues same workflow ID; resumes at last committed part |
| All nodes in region A down | Region A source unreachable if A is down entirely | Jobs wait; if a destination already completed it can serve as alt source (FR-10) |
| Region loss (incl. DB primary) | Patroni promotes a sync standby | Nodes reconnect; in-flight steps replay; no accepted job lost |
| Region partitioned (minority) | Cannot commit; API returns 503 | Nodes in majority continue; minority nodes' in-flight transfers fence themselves (02 §6) |
| Destination throttling/5xx | Step fails transiently | Backoff retry |
| Source object changed mid-copy | ETag/version mismatch | Permanent error `SOURCE_CHANGED` unless policy allows restart |
| Duplicate execution (zombie node) | Two writers to same destination upload | Fencing token + idempotent part writes (02 §6) |
