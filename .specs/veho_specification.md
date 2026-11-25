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
Every assumption this design makes about DBOS Transact for Go was checked against v1.9.0 with a running PostgreSQL (`veho-dbos-spike.md`). The checks are the regression suite `test/dbos` (AC-21), run on every DBOS version bump.

## 1. Problem statement

A client holds a large object X in a storage system that `veho` can read
(e.g. an S3 bucket or an OpenStack Glance repository, anywhere). The client
asks `veho` to copy X to N destinations (N >= 1), according to a
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
- Propagating deletions or later changes of the source: veho replicates data, not commands (OI-9).
- A control plane in other than three regions (design should not preclude it; not tested). The number of destinations and their topology are not limited by this.
- Hosting or managing the storage systems themselves.
- Client-side upload of the data into veho (data must already be in a
  reachable source).
- Event streams (polling and long polling are primary; webhooks for job completion are optional and off by default, `veho-webhooks.md`).

## 4. Glossary

| Term | Meaning |
|------|---------|
| Region | Failure domain of the control plane (A, B, C), each with ≥1 `veho serve` node. Not the same as a *location* of the destination topology (`veho-distribution-policy.md` §2) |
| Node | A running `veho serve` process (API + workflow executor) |
| Tenant | Isolation unit: owns endpoints, credentials, policies, jobs, quotas |
| Endpoint | Tenant-registered storage location config (plugin type, URL, credential ref) |
| Source | The object a job copies, named by the client as an endpoint of its tenant and a path on it (`veho-api-server.md` §5) |
| Policy | Distribution policy: the additional copies to create (N destinations), the completion rule (M of N, spread over the topology) and the options and the options (`veho-distribution-policy.md`) |
| Job | One request to replicate one source object according to one policy |
| Transfer | The copy of the object to one destination; a job has one per destination, in the context of the job's policy |
| Part | Fixed-size byte range of the object; unit of checkpoint and resume |
| Entitlement | Gives an identity (an OIDC subject or group, a SPIFFE ID) a role in a tenant, or the platform-admin role; set by an administrator, never by the caller (SEC-20, `veho-api-server.md` §4.4). Not ligo's *binding* (a source's access to a target), nor its *role binding*, whose meaning it shares |
| Grant | A permission on a named policy: `submit`, `read` or `manage` (SEC-21); never used for entitlements |

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
- **FR-4** The policy MUST support N >= 1 destinations anywhere in the
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
  marked `SUCCEEDED`. The checksum is computed per part, not over the whole
  object at once: the SHA-256 of each part is computed while it streams and
  committed with the part's checkpoint, and the object's digest is the
  SHA-256 over the parts' SHA-256s, the part size and the size
  (`veho-engine.md` §4.4). A transfer resumed on another node therefore
  needs only the parts it has not copied: the hashes of the committed parts
  are in the state store, so a restart never loses checksum state and never
  forces the transfer to begin again. Each part is also compared with the
  destination's own integrity metadata where available (for S3, the part's
  MD5). The digest depends on the part size, which is fixed per job, so all
  of a job's destinations are comparable; it is not the `sha256sum` of the
  object.
- **FR-9** Destination writes MUST be atomic from a reader's point of view:
  a partially written object is never visible under its final name.
- **FR-10** If a completed destination is available, it MAY be used as an
  alternative source for a pending destination (e.g. region A unavailable).
  The data digest MUST match the one recorded at first read.

### Reliability
- **FR-11** Any failure MUST be retried according to the error taxonomy in
  veho-job-lifecycle.md §7 until success, permanent failure, cancellation or deadline.
- **FR-12** Retries MUST use exponential backoff with jitter. A policy sets
  its backoff (`retry.initial`, `retry.max_interval`); the tenant's quotas
  bound what a policy may set (`min_retry_initial`, default 1 s, at most 2 s;
  `max_retry_interval`, default 1 h, at least 15 min), so that no policy
  hammers a shared destination. The policy defaults (2 s, 15 min) are always
  within a tenant's bounds. A policy outside them is refused
  (`invalid_policy`).
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
- **NFR-5 (memory)** A node's memory for parts is bounded by
  `engine.part_memory_mib` (default 1 GiB), across all its transfers: a part
  waits for room before it is read, since a driver may hold a whole part in
  memory (the S3 driver does, for its `Content-MD5`). Within that bound, one
  transfer holds at most `parts_in_flight` × the job's part size. The part
  size is `engine.part_size_mib` (default 64 MiB) until the object needs
  more parts than a destination allows: S3's 10,000 parts make it grow above
  640 GiB, up to the S3 driver's 512 MiB for a 5 TiB object, so the bound per
  transfer is not independent of the object's size, the node's bound is. A
  part larger than the whole budget takes all of it and runs alone
  (`veho-engine.md` §1, memory).
- **NFR-6 (portability)** Linux amd64/arm64 static binary; no cgo. Windows and macOS are not supported, not even for the client; the BSDs may be added later as tier 2 (backlog 33). Tier 1: OpenStack and LXD on Ubuntu 24.04 LTS or later and Rocky/Alma under systemd; tier 2: VMware and Kubernetes; other systems later; no feature works on one platform only (`veho-architecture.md` §7).
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
- **OI-3 Completion semantics.** *Decided* (2026-11-21): acceptance and replication are decoupled. Acceptance needs the 2-of-3 state-store quorum; copying is asynchronous. The destination topology is decoupled from the three control-plane sites: a tree of locations with named levels (for example regions and AZs), any number of them. A policy has N >= 1 destinations and `min_copies` (M of N) plus spread statements (`min_distinct` locations at a level), for example 7 of 9, or 5 copies in at least 5 AZs across 3 regions. The job tracks three milestones separately: satisfied (the minimum is met; job `SUCCEEDED`), complete (all N have a copy) and settled (all transfers terminal). It fails as soon as the policy cannot be met. See `veho-distribution-policy.md`. A deep dive on the topology model is planned (backlog 17).
- **OI-4 Scale of objects.** *Decided* (2026-11-24): the typical object is a VM or container image, a few hundred megabytes to a few gigabytes, 5 to 10 GB at most. The defaults hold: a 64 MiB part gives at most about 160 parts per object (S3 allows 10,000), and the 7-day deadline is far above the copy time; the maximum object size is a per-tenant quota of the quota catalog (`veho-tenancy.md` §4).
- **OI-5 Auth.** *Decided* (2026-11-21): OIDC JWT, API keys and SPIFFE identities (X.509-SVID over mTLS, JWT-SVID) in v1 (SEC-20); Keystone is not in v1 and can come later as an auth plugin or through OIDC federation. The product also has a Web UI and a CLI for humans to administer it: define policies, define who can trigger each, monitor progress (SEC-22, backlog 19).
- **OI-6 Glance.** *Decided* (2026-11-21): S3 first (Garage in the lab); the Glance driver follows once the OpenStack release and upload mechanism (plain PUT, staged upload plus import, multi-store) are known; they decide whether it can resume.
- **OI-7 Policy richness.** *Decided* (2026-11-21): named, versioned policies are in v1, with per-policy grants (who may trigger which policy; SEC-21). *Decided* (2026-11-24): the storage class is a property of the endpoint, not of the policy (`veho-storage-class.md`); archive classes are refused in v1. Still open: other policy options (encryption, metadata/tag propagation, retention)?
- **OI-8 Notifications.** *Decided* (2026-11-21): polling stays the primary mechanism, because only clients reach the servers and never the reverse (no inbound path to clients, no outbound calls to tenant-supplied URLs). To avoid wasted polls, `GET /v1/jobs/{id}` supports long polling (`?wait=<seconds>` with `If-None-Match`): it answers when the job changes or the wait ends, still a client-initiated request. Webhooks are an optional addition, off by default, calling only URLs a tenant registered, under the SSRF rules (SEC-12, SEC-13), signed and retried from an outbox: implemented 2026-11-24 (backlog 11, `veho-webhooks.md`).
- **OI-9 Delete/update propagation.** *Decided* (2026-11-24): replication is explicit only. veho replicates data, not commands: a job copies the object as it was read at the start of the job, and a later change or deletion of the source does nothing to the copies. Deleting the copies is the client's responsibility. The policy's `overwrite` option (`fail`, `replace`, `skip`) only says what a job does when the destination already holds an object under the same name.
- **OI-10 Out-of-process plugins.** *Decided* (2026-11-21): plugins use `hashicorp/go-plugin` (gRPC) and are incorporated in the design now (`veho-plugins.md` §6). The core supports both an out-of-process and an in-process loader. *Decided* (2026-11-24): the built-in drivers (`file`, `s3`, and the later source drivers that ship with veho) are compiled in; `go-plugin` is for third-party and site-specific drivers, loaded from pinned executables (backlog 12). Out of process costs about 15 % of throughput (PLG-17) and a binary to package, sign and version per driver, for no isolation the built-ins need. *Revised* (2026-11-24, the same day): only the drivers that need nothing but the standard library (`file`, `http`) are compiled in; every driver with third-party dependencies (`s3`, `sftp`, `ftp`, later ones) is a plugin binary of the release with a signed manifest, to keep the core lean (`veho-plugin-distribution.md`).
- **OI-11 Existing scaffold.** *Resolved* (2026-11-21): the project follows the layout of the other projects (`cmd/`, `internal/`, `pkg/`, `.specs/`, `test/`); the experimental `step/` and `workflow/` code moved to `internal/` until the implementation replaces it.
- **OI-12 WAN RTT.** *Decided* (2026-11-21): the test setup has RTT up to 24 ms per link (39 ms on a detour), about 30 ms ± 10 ms, negligible jitter within a session, a different baseline per session (paths may differ). Start from WAN-tuned etcd with near-default timeouts (veho-architecture.md §3.3); AC-18 still validates it.
- **OI-15 Control-plane placement.** *Decided* (2026-11-21): one etcd member and one PostgreSQL member per region (A, B, C), so the region is the unit of the quorum. In region A the members run in A1 or A2, the AZs with external links; A3 hosts only stateless `veho serve` nodes. Consequence: the loss of the AZ that hosts a region's members is the loss of that region's member, so one AZ failure uses up the one failure the 2-of-3 quorum tolerates (`veho-architecture.md` §3.3). Two members per region would remove that, at the cost of more nodes; not planned.
- **OI-16 Secrets and audit signing.** *Decided* (2026-11-21): secrets are envelope-encrypted in PostgreSQL, with the KEK stored wrapped in the database and unwrapped at start automatically by a key the platform supplies outside the database (root-owned file, systemd encrypted credential or TPM). No Vault in v1, to avoid its operational weight. Vault (KV or Transit, a cluster per region), HSMs and a Shamir unseal with peer hand-off are further strategies behind the same interface, in scope and staged by tier (SEC-29, `veho-architecture.md` §7.2; backlog 25, 26 and 31). The audit checkpoints are signed by a key stored the same way (SEC-24 to SEC-28). Accepted cost: a compromised node can read every tenant's secrets (R-12).
- **OI-17 Key services.** *Decided* (2026-11-21): the HSM is a Thales Luna reached through KMIP (pure Go, no cgo; PKCS#11 stays a later option through a plugin process). Vault is the community edition or OpenBao, and an Enterprise licence may become available; it makes no difference to the product, only to operators, who need to keep the secrets and the transit key in step between regional clusters (with the community edition or OpenBao by creating or importing the same key in each cluster, with Enterprise by replication): the Setup Guide describes both.
- **OI-13 DBOS version.** *Resolved*: upgraded to v1.9.0 and the spike re-run (veho-dbos-spike.md).
- **OI-14 Threat model.** *Reviewed* (2026-11-24): `veho-threat-model.md` (STRIDE, 39 threats, 18 accepted residual risks); R-07, R-12 and R-13 accepted, the audit hash chain in backlog 32, the quota catalog still open.

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
| `veho-cli.md` | The command line client (C8) |
| `veho-api-server.md` | The API server: transport, interceptors, identities, jobs (C7) |
| `veho-engine.md` | The engine: acceptance, workflows, fencing, retries, housekeeping (C6) |
| `veho-audit.md` | Lightweight audit: events to the system log and the SIEM |
| `veho-secrets.md` | Secrets: key hierarchy, envelope encryption, KEK rotation, `veho kek` |
| `veho-lab.md` | The lab: three regions in one LXD VM, the emulated WAN, Patroni and etcd, Garage; `test/lab/lab.sh` |
| `veho-lab-l3.md` | Lab L3 and the acceptance suite: the veho nodes, the fault-injecting S3 proxy, the daytime scenarios and the overnight campaigns (C9) |
| `veho-distribution-policy.md` | Distribution policy: destinations, M of N completion, topology spread |
| `veho-api.md` | REST API and command line clients |
| `veho-plugins.md` | Storage plugin interfaces and conformance suite |
| `veho-tenancy.md` | Multitenancy and security |
| `veho-threat-model.md` | Threat model (STRIDE) and accepted residual risks |
| `veho-acceptance-matrix.md` | Observability, deployment, acceptance scenarios |
| `veho-dbos-spike.md` | The DBOS experiments |
| `veho-implementation-plan.md` | The implementation chunks and their order |
| `veho-backlog.md` | Work agreed for later |
