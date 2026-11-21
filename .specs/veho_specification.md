# Architectural & Technical Specification: `veho`, durable replication of large objects to any number of destinations, controlled from three regions

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
| `T-nn`, `R-nn` | Threat and accepted residual risk (`veho-threat-model.md`) |
| `AC-nn` | Acceptance scenario (`veho-acceptance-matrix.md` §4) |
| `EXT-nn` | External API assumption, verified against the pinned SDK (`veho-dbos-spike.md`) |
| `OI-nn` | Open issue to decide before implementing the affected component (§9) |
| `FW-nn` | Future work: recorded for later, not part of this version |

### 0.3 Status of the DBOS assumptions
Every assumption this design makes about DBOS Transact for Go was checked against v1.9.0 with a running PostgreSQL (`veho-dbos-spike.md`, `test/spikes/dbos`). The checks are to be promoted into the acceptance suite (AC-21) and repeated on every DBOS version bump.

## 1. Problem statement

A client holds a large object X in a storage system that `veho` can read
(e.g. an S3 bucket or an OpenStack Glance repository, anywhere). The client
asks `veho` to copy X to N destinations (N >= 2), according to a
*distribution policy*. The destinations are any storage systems that a plugin
supports, anywhere: on premises, or in other clouds (for example an S3 bucket
at one provider and another provider's object store). They are controlled by
`veho` nodes that run in three regions and share one durable control plane;
the topology of the destinations is described by the policy and has nothing to
do with the three control-plane regions. The request returns immediately with
a *job ID*; the copy happens asynchronously and the client polls the job for
progress and completion.

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
- A control plane in other than three regions (design should not preclude it; not tested). The number of destinations and their topology are not limited by this.
- Hosting or managing the storage systems themselves.
- Client-side upload of the data into veho (data must already be in a
  reachable source).
- Webhooks/event streams (polling only in v1; see 07).

## 4. Glossary

| Term | Meaning |
|------|---------|
| Region | Failure domain of the control plane (A, B, C), each with ≥1 `veho serve` node. Not the same as a *location* of the destination topology (`veho-distribution-policy.md` §2) |
| Node | A running `veho serve` process (API + workflow executor) |
| Tenant | Isolation unit: owns endpoints, credentials, policies, jobs, quotas |
| Endpoint | Tenant-registered storage location config (plugin type, URL, credential ref) |
| Source | Object reference the client submits (`s3://…`, `glance://…`, …) |
| Policy | Distribution policy: the additional copies to create (N destinations), the completion rule (M of N, spread over the topology) and the options and the options (`veho-distribution-policy.md`) |
| Job | One request to replicate one source object according to one policy |
| Transfer | The copy of the object to one destination; a job has one per destination, in the context of the job's policy |
| Part | Fixed-size byte range of the object; unit of checkpoint and resume |

## 5. Functional requirements

### Submission
- **FR-1** The API MUST accept a job (source URL + policy) and respond
  `202 Accepted` with a job ID only after the job record is durably committed
  to the replicated state store (see veho-architecture.md §3).
- **FR-2** Submission MUST be idempotent per tenant using a client-supplied
  `Idempotency-Key`; repeats return the original job.
- **FR-3** The API MUST validate synchronously: authn/z, policy schema,
  source URL against the tenant's registered endpoints, destination endpoints
  exist and belong to the tenant, quota. It SHOULD also check source
  existence (`Stat`) but a transient failure there MUST NOT reject the job
  (it is retried asynchronously).
- **FR-4** The policy MUST support N >= 2 destinations anywhere in the
  topology, each with its own target path/bucket, a completion rule
  (`min_copies` of N plus spread statements over the topology levels) and options (verification mode, deadline,
  overwrite behaviour, retry tuning); see `veho-distribution-policy.md`.

### Execution
- **FR-5** Each destination is transferred independently; failure or delay of
  one MUST NOT block or fail the other.
- **FR-6** Any node in any region MAY execute a transfer; the same job is
  never executed concurrently by two nodes in a way that corrupts state
  (see veho-job-lifecycle.md §6, fencing).
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
  veho-job-lifecycle.md §7 until success, permanent failure, cancellation or deadline.
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
  impossible (veho-tenancy.md).

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
- **NFR-6 (portability)** Linux amd64/arm64 static binary; no cgo. Tier 1: OpenStack and LXD on Ubuntu 24.04 LTS or later and Rocky/Alma under systemd; tier 2: VMware and Kubernetes; other systems later; no feature works on one platform only (`veho-architecture.md` §7).
- **NFR-7 (observability)** Structured logs (`slog`), Prometheus metrics,
  OpenTelemetry traces propagated across steps (veho-acceptance-matrix.md).

## 7. Decisions


| ADR | Decision | Rationale | Alternatives |
|-----|----------|-----------|--------------|
| 1 | DBOS Transact for Go **v1.9.0** on PostgreSQL as durable engine (OSS features only) | Already scaffolded; only Postgres to operate; fits low concurrency | Temporal; custom outbox+leases; CockroachDB/Yugabyte |
| 2 | Single-primary Postgres, synchronous quorum (`ANY 1`) to a remote region; Patroni + WAN-tuned 3-member etcd for failover, **gated by AC-18**; fallback = operator-approved cross-region failover (veho-architecture.md §3.3) | RPO=0 for a region loss; Raft only on the failover path, not on commits | Async replication (RPO>0); distributed SQL (same WAN quorum); pg_auto_failover (monitor SPOF) |
| 3 | Consistency over availability (minority region refuses writes) | Never lose or fork job state | Active-active with conflict resolution |
| 4 | Symmetric nodes; any node can accept or execute | Matches "A, A2, B or C will resume" | Per-region pinned executors |
| 5 | Own executor registry + reconciler that adopts orphans via `ListWorkflows`+`ResumeWorkflows`, plus fencing | Spike: OSS DBOS never detects dead executors (veho-dbos-spike.md) | Rely on DBOS Conductor (commercial) |
| 6 | Part-level checkpointing; resumable where backend supports it | Large files; retry efficiency | Whole-object retry only |
| 7 | Plugins behind a transport-neutral `Driver` interface, with two loaders: out-of-process (`hashicorp/go-plugin`, gRPC) and in-process (compiled-in); which one the built-in drivers use is decided later | Process isolation and third-party drivers without recompiling, with no change to the core whichever way the decision goes | Compiled-in only (no isolation); Go `plugin` pkg (rejected) |
| 8 | Source URLs must match tenant-registered endpoints | SSRF and tenant isolation | Free-form URLs (rejected) |
| 9 | Tenant isolation via RLS + tenant-scoped refs, single shared cluster | Simple ops | DB-per-tenant (heavier) |
| 10 | gRPC as the primary API for application-to-application flows and the CLI, with REST generated from the same protobuf definitions | One implementation of authentication, authorization and audit; streaming (`WatchJob`) fits the client-initiated rule of OI-8; no drift between the two interfaces | REST only (no streaming, hand-kept OpenAPI); two separate implementations |
| 11 | Secrets in PostgreSQL under envelope encryption, KEK wrapped in the database and unwrapped at start by a key supplied outside it; Vault and Shamir unseal later behind the same interface | No extra service to run; unattended restarts and recovery (G2, NFR-3); ciphertext replicates and backs up with the state store | Vault only (per-tenant policies and dynamic credentials, but a service to run across three regions); manual Shamir unseal from the start (conflicts with automatic recovery) |
| 12 | Platform tiers: OpenStack and LXD first, VMware and Kubernetes after; strategies per concern all in scope and chosen by configuration; nothing platform-only without a fallback | The first deployment is OpenStack, but the product must not be tied to it | Build for OpenStack only; auto-detect the platform and switch code paths silently |

## 8. System invariants

Each invariant MUST have at least one automated test (`veho-acceptance-matrix.md`).

| ID | Invariant | Where |
| :--- | :--- | :--- |
| INV-1 | A job for which the API returned `202` is never lost: its rows and its workflow exist together or not at all. | `veho-job-lifecycle.md` §3 |
| INV-2 | A destination object is visible under its final name only after it was committed and verified (size and checksum). | FR-8, FR-9 |
| INV-3 | A write that carries a stale fence changes nothing: at most one executor's state writes take effect for a transfer. | `veho-job-lifecycle.md` §6 |
| INV-4 | A tenant never reads or changes another tenant's resources; a resource of another tenant is indistinguishable from a missing one. | `veho-tenancy.md` |
| INV-5 | No credential is stored in a workflow input, a log line, an audit entry or an API response. | SEC-7, SEC-9 |
| INV-6 | A submitted source URL resolves to a registered endpoint of the submitting tenant. | SEC-12 |

## 9. Open issues

- **OI-1 Source locality.** *Decided* (2026-11-21): the source is external and populating it is the client's responsibility; it can be in any location. The policy models only the additional copies; the source is never a destination and is not counted (`veho-distribution-policy.md` §1).
- **OI-2 Regions & cloud.** *Decided* (2026-11-21): `veho` is a CP system. A job is accepted only when a quorum of the three state-store regions (2 of 3) has committed it; a region in the minority answers 503. Which providers and the real RTT remain open (OI-12).
- **OI-3 Completion semantics.** *Decided* (2026-11-21): acceptance and replication are decoupled. Acceptance needs the 2-of-3 state-store quorum; copying is asynchronous. The destination topology is decoupled from the three control-plane sites: a tree of locations with named levels (for example regions and AZs), any number of them. A policy has N >= 2 destinations and `min_copies` (M of N) plus spread statements (`min_distinct` locations at a level), for example 7 of 9, or 5 copies in at least 5 AZs across 3 regions. The job tracks three milestones separately: satisfied (the minimum is met; job `SUCCEEDED`), complete (all N have a copy) and settled (all transfers terminal). It fails as soon as the policy cannot be met. See `veho-distribution-policy.md`. A deep dive on the topology model is planned (backlog 17).
- **OI-4 Scale of objects.** Typical and maximum object size? (affects part
  size, `MaxParts` handling, deadline defaults).
- **OI-5 Auth.** *Decided* (2026-11-21): OIDC JWT, API keys and SPIFFE identities (X.509-SVID over mTLS, JWT-SVID) in v1 (SEC-20); Keystone is not in v1 and can come later as an auth plugin or through OIDC federation. The product also has a Web UI and a CLI for humans to administer it: define policies, define who can trigger each, monitor progress (SEC-22, backlog 19).
- **OI-6 Glance.** *Decided* (2026-11-21): S3 first (Garage in the lab); the Glance driver follows once the OpenStack release and upload mechanism (plain PUT, staged upload plus import, multi-store) are known; they decide whether it can resume.
- **OI-7 Policy richness.** *Decided* (2026-11-21): named, versioned policies are in v1, with per-policy grants (who may trigger which policy; SEC-21). Still open: other policy options (encryption, storage class,
  metadata/tag propagation, retention)?
- **OI-8 Notifications.** *Decided* (2026-11-21): polling stays the primary mechanism, because only clients reach the servers and never the reverse (no inbound path to clients, no outbound calls to tenant-supplied URLs). To avoid wasted polls, `GET /v1/jobs/{id}` supports long polling (`?wait=<seconds>` with `If-None-Match`): it answers when the job changes or the wait ends, still a client-initiated request. Webhooks stay a later option, off by default, calling only registered endpoints under the SSRF rules (SEC-12, SEC-13), signed and retried from an outbox (backlog 11).
- **OI-9 Delete/update propagation.** Is replication create-only (new object
  versions) or should overwrites/deletes of X be supported?
- **OI-10 Out-of-process plugins.** *Decided* (2026-11-21): plugins use `hashicorp/go-plugin` (gRPC) and are incorporated in the design now (`veho-plugins.md` §6). The core supports both an out-of-process and an in-process loader. Still to decide, later: whether the built-in drivers ship as separate plugin binaries or compiled in (backlog 12).
- **OI-11 Existing scaffold.** *Resolved* (2026-11-21): the project follows the layout of the other projects (`cmd/`, `internal/`, `pkg/`, `.specs/`, `test/`); the experimental `step/` and `workflow/` code moved to `internal/` until the implementation replaces it.
- **OI-12 WAN RTT.** *Decided* (2026-11-21): the test setup has RTT up to 24 ms per link (39 ms on a detour), about 30 ms ± 10 ms, negligible jitter within a session, a different baseline per session (paths may differ). Start from WAN-tuned etcd with near-default timeouts (veho-architecture.md §3.3); AC-18 still validates it.
- **OI-15 Control-plane placement.** *Decided* (2026-11-21): one etcd member and one PostgreSQL member per region (A, B, C), so the region is the unit of the quorum. In region A the members run in A1 or A2, the AZs with external links; A3 hosts only stateless `veho serve` nodes. Consequence: the loss of the AZ that hosts a region's members is the loss of that region's member, so one AZ failure uses up the one failure the 2-of-3 quorum tolerates (`veho-architecture.md` §3.3). Two members per region would remove that, at the cost of more nodes; not planned.
- **OI-16 Secrets and audit signing.** *Decided* (2026-11-21): secrets are envelope-encrypted in PostgreSQL, with the KEK stored wrapped in the database and unwrapped at start automatically by a key the platform supplies outside the database (root-owned file, systemd encrypted credential or TPM). No Vault in v1, to avoid its operational weight. Vault (KV or Transit, a cluster per region), HSMs and a Shamir unseal with peer hand-off are further strategies behind the same interface, in scope and staged by tier (SEC-29, `veho-architecture.md` §7.2; backlog 25, 26 and 31). The audit checkpoints are signed by a key stored the same way (SEC-24 to SEC-28). Accepted cost: a compromised node can read every tenant's secrets (R-12).
- **OI-17 Key services.** *Decided* (2026-11-21): the HSM is a Thales Luna reached through KMIP (pure Go, no cgo; PKCS#11 stays a later option through a plugin process). Vault is the community edition or OpenBao, and an Enterprise licence may become available; it makes no difference to the product, only to operators, who need to keep the secrets and the transit key in step between regional clusters (with the community edition or OpenBao by creating or importing the same key in each cluster, with Enterprise by replication): the Setup Guide describes both.
- **OI-13 DBOS version.** *Resolved*: upgraded to v1.9.0 and the spike re-run (veho-dbos-spike.md).
- **OI-14 Threat model.** *Drafted* (2026-11-21): `veho-threat-model.md` (STRIDE, 37 threats, 12 accepted residual risks). To be reviewed with the answers to the other open issues, which can add or remove threats.

### 9.1 Resolved: the DBOS spike

All items are resolved in [veho-dbos-spike.md](veho-dbos-spike.md). Summary:
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
| `veho_specification.md` | This file: conventions, requirements, decisions, invariants, open issues |
| `veho-architecture.md` | Topology, state store, components, failure model |
| `veho-job-lifecycle.md` | Data model, state machines, workflows, adoption, retries |
| `veho-audit-housekeeping.md` | Audit log (hash chain, checkpoints, export, pruning) and housekeeping (zombie tasks, retention), copied from ligo |
| `veho-latency-envelope.md` | Supported latency and jitter, degradation, tuning, AC-24 |
| `veho-secrets.md` | Secrets: key hierarchy, envelope encryption, KEK rotation, `veho kek` |
| `veho-lab.md` | The lab: three regions in one LXD VM, the emulated WAN, Patroni and etcd, Garage; `test/lab/lab.sh` |
| `veho-distribution-policy.md` | Distribution policy: destinations, M of N completion, topology spread |
| `veho-api.md` | REST API and command line clients |
| `veho-plugins.md` | Storage plugin interfaces and conformance suite |
| `veho-tenancy.md` | Multitenancy and security |
| `veho-threat-model.md` | Threat model (STRIDE) and accepted residual risks |
| `veho-acceptance-matrix.md` | Observability, deployment, acceptance scenarios |
| `veho-dbos-spike.md` | The DBOS experiments |
| `veho-implementation-plan.md` | The implementation chunks and their order |
| `veho-backlog.md` | Work agreed for later |
