# Architectural & Technical Specification: `replica`, durable replication of large objects across three regions

- **Document Version:** `0.1.0`
- **Status:** Draft, for review; not yet approved for implementation (`AGENTS.md`, rule 1)
- **Target Stack:** Go 1.27.4+, PostgreSQL 15+ (`pgx/v5`), DBOS Transact for Go (pinned at v1.9.0; open-source features only), Patroni with etcd for the state store's failover, `github.com/jessevdk/go-flags`, `log/slog`; storage backends as plugins (S3, OpenStack Glance, local files for tests)
- **Purpose:** Requirements, decisions and open issues for Spec-Driven Development; the topic specifications listed in §10 hold the detail

---

## 0. Document Conventions

### 0.1 Normative language
The key words **MUST**, **MUST NOT**, **REQUIRED**, **SHALL**, **SHOULD**, **SHOULD NOT**, **MAY** are to be interpreted as described in RFC 2119 / RFC 8174.

### 0.2 Identifier scheme
| Prefix | Meaning |
| :--- | :--- |
| `FR-nn`, `NFR-nn` | Functional and non-functional requirement (§2 to §6) |
| `SEC-nn`, `PLG-nn`, `API-nn` | Security, plugin and API requirement, in the topic specifications |
| `ADR-nn` | Architectural decision (§7) |
| `INV-nn` | System invariant (MUST hold at all times; each MUST have at least one automated test; §8) |
| `T-nn`, `R-nn` | Threat and accepted residual risk (`replica-threat-model.md`) |
| `AC-nn` | Acceptance scenario (`replica-acceptance-matrix.md` §4) |
| `EXT-nn` | External API assumption, verified against the pinned SDK (`replica-dbos-spike.md`) |
| `OI-nn` | Open issue to decide before implementing the affected component (§9) |
| `FW-nn` | Future work: recorded for later, not part of this version |

### 0.3 Status of the DBOS assumptions
Every assumption this design makes about DBOS Transact for Go was checked against v1.9.0 with a running PostgreSQL (`replica-dbos-spike.md`, `test/spikes/dbos`). The checks are to be promoted into the acceptance suite (AC-21) and repeated on every DBOS version bump.

## 1. Problem statement

A client holds a large object X in a storage system reachable from region A
(e.g. an S3 bucket or an OpenStack Glance repository). The client asks
`replica` to copy X to destinations in regions B and C, according to a
*replication policy*. The request returns immediately with a *job ID*; the
copy happens asynchronously and the client polls the job for progress and
completion.

The system must never lose an accepted job and must keep retrying until the
job succeeds, is cancelled, or exceeds its deadline — even if the node that
accepted it (or an entire region) fails.

## 2. Goals

- G1. Durable acceptance: once the API returns a job ID, the job survives
  loss of any single node and of any single region.
- G2. Automatic resumption by another node (same region or other regions).
- G3. Efficient handling of very large objects (hundreds of GiB to TiB):
  streaming, bounded memory, resumable at part granularity.
- G4. Integrity: every destination copy is verified before being reported done.
- G5. Pluggable storage backends.
- G6. Multi-tenancy with strong isolation of data, credentials and quotas.
- G7. Low operational footprint: a single Go binary plus PostgreSQL.

## 3. Non-goals (v1)

- High concurrency or throughput scale-out (target: tens of concurrent jobs,
  not thousands).
- Continuous/bidirectional sync, change-data-capture, or conflict resolution.
- More than three regions (design should not preclude it; not tested).
- Hosting or managing the storage systems themselves.
- Client-side upload of the data into replica (data must already be in a
  reachable source).
- Webhooks/event streams (polling only in v1; see 07).

## 4. Glossary

| Term | Meaning |
|------|---------|
| Region | Failure domain (A, B, C). Each has storage and ≥1 `replica serve` node |
| Node | A running `replica serve` process (API + workflow executor) |
| Tenant | Isolation unit: owns endpoints, credentials, policies, jobs, quotas |
| Endpoint | Tenant-registered storage location config (plugin type, URL, credential ref) |
| Source | Object reference the client submits (`s3://…`, `glance://…`, …) |
| Policy | Declarative description of destinations and options |
| Job | One request to replicate one source object according to one policy |
| Transfer | The copy of the object to one destination; a job has one per destination |
| Part | Fixed-size byte range of the object; unit of checkpoint and resume |

## 5. Functional requirements

### Submission
- **FR-1** The API MUST accept a job (source URL + policy) and respond
  `202 Accepted` with a job ID only after the job record is durably committed
  to the replicated state store (see replica-architecture.md §3).
- **FR-2** Submission MUST be idempotent per tenant using a client-supplied
  `Idempotency-Key`; repeats return the original job.
- **FR-3** The API MUST validate synchronously: authn/z, policy schema,
  source URL against the tenant's registered endpoints, destination endpoints
  exist and belong to the tenant, quota. It SHOULD also check source
  existence (`Stat`) but a transient failure there MUST NOT reject the job
  (it is retried asynchronously).
- **FR-4** The policy MUST support specifying destinations for region B and
  region C independently, per-destination target path/bucket, and options
  (verification mode, deadline, overwrite behaviour, retry tuning).

### Execution
- **FR-5** Each destination is transferred independently; failure or delay of
  one MUST NOT block or fail the other.
- **FR-6** Any node in any region MAY execute a transfer; the same job is
  never executed concurrently by two nodes in a way that corrupts state
  (see replica-job-lifecycle.md §6, fencing).
- **FR-7** Transfers MUST stream data (no full-object buffering on disk or
  memory) and checkpoint per part so a resumed transfer continues from the
  last committed part where the destination plugin supports it.
- **FR-8** Every transfer MUST be verified (size + checksum) before being
  marked `SUCCEEDED`. Default checksum: SHA-256 computed while streaming,
  compared to the destination's own integrity metadata where available.
- **FR-9** Destination writes MUST be atomic from a reader's point of view:
  a partially written object is never visible under its final name.
- **FR-10** If a completed destination is available, it MAY be used as an
  alternative source for a pending destination (e.g. region A unavailable).
  The data digest MUST match the one recorded at first read.

### Reliability
- **FR-11** Any failure MUST be retried according to the error taxonomy in
  replica-job-lifecycle.md §7 until success, permanent failure, cancellation or deadline.
- **FR-12** Retries MUST use exponential backoff with jitter, with
  per-tenant-configurable bounds.
- **FR-13** Retried operations MUST be idempotent (same job + destination +
  part always yields the same result).

### Visibility and control
- **FR-14** `GET /jobs/{id}` MUST return job state, per-transfer state,
  bytes copied / total, attempt counts and last error.
- **FR-15** Clients MUST be able to cancel a job and to retry failed
  transfers of a job.
- **FR-16** Clients MUST be able to list jobs (filter by state, time, source).

### Extensibility and tenancy
- **FR-17** Storage backends are plugins implementing the interfaces in 04.
  Adding a backend MUST NOT require changes to core workflow code.
- **FR-18** All resources are tenant-scoped; cross-tenant access MUST be
  impossible (replica-tenancy.md).

## 6. Non-functional requirements

- **NFR-1 (durability)** RPO = 0 for accepted jobs under loss of one region.
- **NFR-2 (availability)** Job submission and status are available while a
  majority (2 of 3) of regions are mutually reachable. Behaviour when only a
  minority is reachable: reads of the last known state MAY be served stale
  (flagged), writes are refused (`503`). Consistency is preferred over
  availability.
- **NFR-3 (recovery time)** After a node crash, an orphaned transfer is picked
  up by another node within 2 minutes (configurable lease).
  After a region loss, within 5 minutes including state-store failover.
- **NFR-4 (submission latency)** p99 < 1 s at inter-region RTT ≤ 150 ms
  (dominated by the synchronous cross-region commit).
- **NFR-5 (memory)** Per-transfer memory bounded by `parts_in_flight × part_size`
  (defaults 4 × 64 MiB); independent of object size.
- **NFR-6 (portability)** Linux amd64/arm64 static binary; no cgo.
- **NFR-7 (observability)** Structured logs (`slog`), Prometheus metrics,
  OpenTelemetry traces propagated across steps (replica-acceptance-matrix.md).

## 7. Decisions


| ADR | Decision | Rationale | Alternatives |
|-----|----------|-----------|--------------|
| 1 | DBOS Transact for Go **v1.9.0** on PostgreSQL as durable engine (OSS features only) | Already scaffolded; only Postgres to operate; fits low concurrency | Temporal; custom outbox+leases; CockroachDB/Yugabyte |
| 2 | Single-primary Postgres, synchronous quorum (`ANY 1`) to a remote region; Patroni + WAN-tuned 3-member etcd for failover, **gated by AC-18**; fallback = operator-approved cross-region failover (replica-architecture.md §3.3) | RPO=0 for a region loss; Raft only on the failover path, not on commits | Async replication (RPO>0); distributed SQL (same WAN quorum); pg_auto_failover (monitor SPOF) |
| 3 | Consistency over availability (minority region refuses writes) | Never lose or fork job state | Active-active with conflict resolution |
| 4 | Symmetric nodes; any node can accept or execute | Matches "A, A2, B or C will resume" | Per-region pinned executors |
| 5 | Own executor registry + reconciler that adopts orphans via `ListWorkflows`+`ResumeWorkflows`, plus fencing | Spike: OSS DBOS never detects dead executors (replica-dbos-spike.md) | Rely on DBOS Conductor (commercial) |
| 6 | Part-level checkpointing; resumable where backend supports it | Large files; retry efficiency | Whole-object retry only |
| 7 | Compiled-in plugins behind a transport-neutral interface | Simplest and type-safe; keeps gRPC door open | go-plugin/gRPC now; Go `plugin` pkg (rejected) |
| 8 | Source URLs must match tenant-registered endpoints | SSRF and tenant isolation | Free-form URLs (rejected) |
| 9 | Tenant isolation via RLS + tenant-scoped refs, single shared cluster | Simple ops | DB-per-tenant (heavier) |

## 8. System invariants

Each invariant MUST have at least one automated test (`replica-acceptance-matrix.md`).

| ID | Invariant | Where |
| :--- | :--- | :--- |
| INV-1 | A job for which the API returned `202` is never lost: its rows and its workflow exist together or not at all. | `replica-job-lifecycle.md` §3 |
| INV-2 | A destination object is visible under its final name only after it was committed and verified (size and checksum). | FR-8, FR-9 |
| INV-3 | A write that carries a stale fence changes nothing: at most one executor's state writes take effect for a transfer. | `replica-job-lifecycle.md` §6 |
| INV-4 | A tenant never reads or changes another tenant's resources; a resource of another tenant is indistinguishable from a missing one. | `replica-tenancy.md` |
| INV-5 | No credential is stored in a workflow input, a log line, an audit entry or an API response. | SEC-7, SEC-9 |
| INV-6 | A submitted source URL resolves to a registered endpoint of the submitting tenant. | SEC-12 |

## 9. Open issues

- **OI-1 Source locality.** Is the source (e.g. the S3 bucket / Glance) always
  in region A, or can it be anywhere reachable? Is it readable from nodes in
  B and C (needed for failover execution and FR-10)?
- **OI-2 Regions & cloud.** Which regions/providers (cloud, on-prem,
  OpenStack)? RTT between them drives commit latency (NFR-4). Are three
  Postgres members with 1 sync standby acceptable, or must the system keep
  accepting jobs with a *single* region left (that would require relaxing
  RPO to async or abandoning strict quorum)?
- **OI-3 Completion semantics.** Is a job "done" only when **both** B and C
  have the data (current spec), or do you want policies like "done when any
  1 of 2" with the other continuing in the background?
- **OI-4 Scale of objects.** Typical and maximum object size? (affects part
  size, `MaxParts` handling, deadline defaults).
- **OI-5 Auth.** Which identity provider is in use (OIDC? Keystone, since you
  mention Glance?). Should tenants map to OpenStack projects?
- **OI-6 Glance.** Which OpenStack release / which Glance upload mechanisms
  (plain PUT of image data, staged + import, multi-store)? Determines
  resumability for that plugin.
- **OI-7 Policy richness.** Are named, reusable policies needed in v1, or is
  an inline policy enough? Other policy options (encryption, storage class,
  metadata/tag propagation, retention)?
- **OI-8 Notifications.** Is polling enough, or are webhooks/events needed in
  v1?
- **OI-9 Delete/update propagation.** Is replication create-only (new object
  versions) or should overwrites/deletes of X be supported?
- **OI-10 Out-of-process plugins.** Do third parties need to write plugins
  without recompiling `replica`? If yes, promote gRPC plugins to v1.
- **OI-11 Existing scaffold.** *Resolved* (2026-11-21): the project follows the layout of the other projects (`cmd/`, `internal/`, `pkg/`, `.specs/`, `test/`); the experimental `step/` and `workflow/` code moved to `internal/` until the implementation replaces it.
- **OI-12 WAN RTT.** What are the actual RTT and jitter between the three
  sites? This decides whether WAN-tuned etcd (replica-architecture.md §3.3 option 1) is viable or we
  start from the operator-approved failover fallback (option 4).
- **OI-13 DBOS version.** *Resolved*: upgraded to v1.9.0 and the spike re-run (replica-dbos-spike.md).
- **OI-14 Threat model.** *Drafted* (2026-11-21): `replica-threat-model.md` (STRIDE, 30 threats, 9 accepted residual risks). To be reviewed with the answers to the other open issues, which can add or remove threats.

### 9.1 Resolved: the DBOS spike

All items are resolved in [replica-dbos-spike.md](replica-dbos-spike.md). Summary:
**transactional enqueue works** (job rows and workflow commit atomically, no
outbox); no automatic dead-executor takeover, but explicit adoption works via
the documented public API (`ListWorkflows(WithFilterExecutorIDs)` +
`ResumeWorkflows`); zombie is fenced at next checkpoint but in-flight side
effects duplicate; partitioned queues give per-tenant limits; multi-host DSN
survives Postgres failover with a synchronous standby (no duplicate work);
no transaction-mode pooler; step retry now supports a retry predicate (no
jitter). Not yet exercised: rolling upgrades end to end (AC-20), a cap across
all partitions at once.

## 10. Specifications

| File | Content |
| :--- | :--- |
| `replica_specification.md` | This file: conventions, requirements, decisions, invariants, open issues |
| `replica-architecture.md` | Topology, state store, components, failure model |
| `replica-job-lifecycle.md` | Data model, state machines, workflows, adoption, retries |
| `replica-api.md` | REST API and command line clients |
| `replica-plugins.md` | Storage plugin interfaces and conformance suite |
| `replica-tenancy.md` | Multitenancy and security |
| `replica-threat-model.md` | Threat model (STRIDE) and accepted residual risks |
| `replica-acceptance-matrix.md` | Observability, deployment, acceptance scenarios |
| `replica-dbos-spike.md` | The DBOS experiments |
| `replica-backlog.md` | Work agreed for later |
