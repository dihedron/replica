# 07 — Open questions & decision log

## A. Decisions proposed in this draft (ADRs, to be confirmed)

| ADR | Decision | Rationale | Alternatives |
|-----|----------|-----------|--------------|
| 1 | DBOS Transact for Go on PostgreSQL as durable engine | Already scaffolded; only Postgres to operate; fits low concurrency | Temporal; custom outbox+leases; CockroachDB/Yugabyte |
| 2 | Single-primary Postgres, synchronous quorum (`ANY 1`) to a remote region; Patroni + WAN-tuned 3-member etcd for failover, **gated by AT-18**; fallback = operator-approved cross-region failover (01 §3.3) | RPO=0 for a region loss; Raft only on the failover path, not on commits | Async replication (RPO>0); distributed SQL (same WAN quorum); pg_auto_failover (monitor SPOF) |
| 3 | Consistency over availability (minority region refuses writes) | Never lose or fork job state | Active-active with conflict resolution |
| 4 | Symmetric nodes; any node can accept or execute | Matches "A, A2, B or C will resume" | Per-region pinned executors |
| 5 | Own executor registry + reconciler that adopts orphans via `ListWorkflows`+`ResumeWorkflow`, plus fencing | Spike: OSS DBOS never detects dead executors (08) | Rely on DBOS Conductor (commercial) |
| 6 | Part-level checkpointing; resumable where backend supports it | Large files; retry efficiency | Whole-object retry only |
| 7 | Compiled-in plugins behind a transport-neutral interface | Simplest and type-safe; keeps gRPC door open | go-plugin/gRPC now; Go `plugin` pkg (rejected) |
| 8 | Source URLs must match tenant-registered endpoints | SSRF and tenant isolation | Free-form URLs (rejected) |
| 9 | Tenant isolation via RLS + tenant-scoped refs, single shared cluster | Simple ops | DB-per-tenant (heavier) |

## B. DBOS spike — resolved

All six items are resolved in [08-spike-dbos.md](08-spike-dbos.md). Summary:
no same-transaction enqueue (outbox is the design); no automatic dead-executor
takeover, but explicit adoption works through the public API
(`ListWorkflows(WithExecutorIDs)` + `ResumeWorkflow`) and resumes from the last
step; zombie is fenced at next checkpoint but in-flight side effects
duplicate; partitioned queues give per-tenant limits; multi-host DSN survives
Postgres failover; no transaction-mode pooler; recovery is
application-version-scoped; step retry exists but has no jitter/classification.
Not yet exercised: rolling-upgrade behaviour end to end (AT-20), caps across
all partitions.

## C. Questions for you

- **Q-1 Source locality.** Is the source (e.g. the S3 bucket / Glance) always
  in region A, or can it be anywhere reachable? Is it readable from nodes in
  B and C (needed for failover execution and FR-10)?
- **Q-12 WAN RTT.** What are the actual RTT and jitter between the three
  sites? This decides whether WAN-tuned etcd (01 §3.3 option 1) is viable or we
  start from the operator-approved failover fallback (option 4).
- **Q-2 Regions & cloud.** Which regions/providers (cloud, on-prem,
  OpenStack)? RTT between them drives commit latency (NFR-4). Are three
  Postgres members with 1 sync standby acceptable, or must the system keep
  accepting jobs with a *single* region left (that would require relaxing
  RPO to async or abandoning strict quorum)?
- **Q-3 Completion semantics.** Is a job "done" only when **both** B and C
  have the data (current spec), or do you want policies like "done when any
  1 of 2" with the other continuing in the background?
- **Q-4 Scale of objects.** Typical and maximum object size? (affects part
  size, `MaxParts` handling, deadline defaults).
- **Q-5 Auth.** Which identity provider is in use (OIDC? Keystone, since you
  mention Glance?). Should tenants map to OpenStack projects?
- **Q-6 Glance.** Which OpenStack release / which Glance upload mechanisms
  (plain PUT of image data, staged + import, multi-store)? Determines
  resumability for that plugin.
- **Q-7 Policy richness.** Are named, reusable policies needed in v1, or is
  an inline policy enough? Other policy options (encryption, storage class,
  metadata/tag propagation, retention)?
- **Q-8 Notifications.** Is polling enough, or are webhooks/events needed in
  v1?
- **Q-9 Delete/update propagation.** Is replication create-only (new object
  versions) or should overwrites/deletes of X be supported?
- **Q-10 Out-of-process plugins.** Do third parties need to write plugins
  without recompiling `replicad`? If yes, promote gRPC plugins to v1.
- **Q-11 Existing scaffold.** `step/` (email, gemini, http, saga) and
  `workflow/` look like experiments. OK to replace them with the layout in
  01 §4, keeping `metadata`, logging init, and go-flags CLI?

- **Q-13 DBOS version.** go.mod pins v0.11.0; v1.9.0 exists (API changes: `Client`-based `ResumeWorkflow`, `WithResumeQueue`, `WithMaxRecoveryAttempts`, Conductor-aware launch). Upgrade before implementing? The spike should be re-run on the chosen version (AT-21).
