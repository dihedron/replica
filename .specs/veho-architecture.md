# veho: architecture

This file specifies the topology, the state store, the components and the failure model of `veho`. It is part of `veho_specification.md`, which holds the requirements and the decisions.

Status: draft, 2026-11-21, for review.

## 1. Topology

```
        Region A                    Region B                    Region C
   ┌───────────────┐          ┌───────────────┐          ┌───────────────┐
   │  veho A1,A2   │          │  veho B1,B2   │          │  veho C1,C2   │
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

- All `veho serve` nodes are **identical and symmetric**. "Node A" is simply
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
  regions with RTT ≤ 150 ms between any pair; see veho_specification.md OI-2.
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

**Decision (2026-11-21, OI-12).** Measured inter-region RTT is 30 ms ± 10 ms (worst case about 40 ms). Jitter inside a session is negligible, but the baseline differs between sessions because network paths can differ. Option 1 is viable with little tuning: the etcd defaults (heartbeat 100 ms, election timeout 1000 ms) already exceed the worst-case RTT by 2.5x and 25x. Use `heartbeat-interval` 100 ms, `election-timeout` 1000 ms and the Patroni defaults (`ttl` 30, `loop_wait` 10, `retry_timeout` 10) as the starting point, and size the timeouts on the worst-case baseline (40 ms), not the mean. The long-lived etcd peer and replication connections each keep their own path baseline, so a path change shows up as a one-off shift at reconnect, not as jitter; the timeouts must tolerate it. Each commit pays one synchronous-standby round trip, about 30 to 40 ms, which is acceptable at tens of jobs. The fallback (option 4) stays documented but is not the starting point.

Gate: before committing to option 1, run acceptance test **AC-18** (veho-acceptance-matrix.md §4):
inject the real inter-region RTT + jitter for 24 h and count leader changes and
Patroni demotions; pass criterion is zero spurious failovers.

#### Measured links (2026-11-21)

This is a **test setup**, in which everything is expected to work perfectly; it is not a limit of the product. What the control plane supports at larger latency and jitter, and how it degrades, is in `veho-latency-envelope.md`.

Topology of the links:

- Each of the three regions (A, B, C) has a link to each of the other two.
- Region A has three AZs: A1, A2 and A3. The links to the other regions start from A1 and A2 only; A3 has no external link and reaches the other regions through A1 and A2. B and C have the same two link-bearing AZs (1 and 2).
- Each link is redundant: each of AZ 1 and AZ 2 has two fibers. In a link ID the first digit is the AZ and the second is the fiber: `A1.1` is the first fiber out of A1, `A2.1` the first fiber out of A2.

One-way latency in ms, as measured:

| Link (regions) | Link ID | One-way (ms) |
| :--- | :--- | :--- |
| A – B | `A1.1B1.1` | 9.6 |
| A – B | `A1.2B1.2` | 11.7 |
| A – B | `A2.1B2.1` | 9.6 |
| A – B | `A2.2B2.2` | 11.5 |
| A – C | `A1.1C1.1` | 9.7 |
| A – C | `A1.2C1.2` | 12.0 |
| A – C | `A2.1C2.1` | 9.5 |
| A – C | `A2.2C2.2` | 12.0 |
| C – B | `C1.1B1.1` | 4.3 |
| C – B | `C1.2B1.2` | 7.5 |
| C – B | `C2.1B2.1` | 4.3 |
| C – B | `C2.2B2.2` | 7.3 |

Reading:

- The AZ makes no difference (AZ 1 and AZ 2 give the same figures); the fiber does, by up to 3.2 ms (fiber 2 is the slower one on every link).
- Region A is far from both others (9.5 to 12 ms one way, RTT 19 to 24 ms). B and C are close (4.3 to 7.5 ms one way, RTT 8.6 to 15 ms).
- The majority round trip of etcd is dominated by the closest pair, so the leader and the Postgres primary belong in B or C. A primary in B or C pays the B–C round trip for its synchronous standby (9 to 15 ms per commit); a primary in A would pay 19 to 24 ms.
- A detour through the third region costs 14 to 19.5 ms one way, so a path that fails over to a detour has a round trip of about 39 ms, inside the 40 ms of the decision above.
- A3 has no external link: it depends on A1 and A2 for every message to the other regions. Losing A1 and A2 isolates the whole of region A, A3 included.
- Placement (OI-15): one etcd and one PostgreSQL member per region. In A they run in A1 or A2, not A3: a member in A3 would add an intra-region hop to every message and gain nothing, since the region is one unit of the quorum anyway. The consequence to remember is that the AZ hosting a region's members is that region's failure domain: its loss costs the one failure the 2-of-3 quorum tolerates.

The DBOS system database and veho's application tables live in the same
cluster (separate schemas), so job creation and workflow enqueue can occur in
**one transaction** (see veho-job-lifecycle.md §3).

## 4. Components (inside one `veho serve` process)

| Component | Responsibility |
|-----------|----------------|
| `api` | HTTP server, authn/z, validation, idempotency, job CRUD |
| `store` | Application tables (jobs, transfers, endpoints, tenants, quotas); repository interfaces; RLS session setup |
| `engine` | DBOS context, workflow/step registration, queues; thin wrapper (replaces current `workflow` package globals) |
| `workflows` | `ReplicateJob`, `Transfer` workflows (veho-job-lifecycle.md) |
| `reconciler` | Periodic scan; adopts orphaned work, enforces deadlines, expires leases (veho-job-lifecycle.md §6) |
| `plugin` | Registry and contracts for storage drivers (veho-plugins.md) |
| `driver/*` | Built-in plugins: `s3`, `glance`, `file` (testing), `http` (read-only source) |
| `tenant` | Tenant resolution, quota accounting, secret resolution (veho-tenancy.md) |
| `obs` | slog, Prometheus, OpenTelemetry |

Repository layout, as in the other projects (`AGENTS.md`, "Project Structure
Overview"). The experimental `internal/step` and `internal/workflow` packages
are scaffolding that the implementation replaces.

```
cmd/veho/           main, command line (go-flags), logging and profiling init
api/                   protobuf definitions (source of truth, API-1)
internal/api/          gRPC services, REST gateway, interceptors (authn/z, quotas, audit)
internal/store/        sql, migrations, repositories
internal/engine/       DBOS wrapper (replaces internal/workflow)
internal/workflows/    replicate.go, transfer.go (replace internal/step)
internal/reconciler/
internal/tenant/
internal/obs/
pkg/metadata/          build metadata injected at link time
pkg/plugin/            public plugin API (stable package)
pkg/plugin/conformance/ reusable plugin test suite
internal/drivers/{s3,glance,file,http}/
migrations/            SQL schema migrations (embedded)
test/spikes/dbos/      the disposable DBOS harness (veho-dbos-spike.md)
test/lab/              lab scenarios (three regions; veho-acceptance-matrix.md)
packaging/             postinstall, service unit, signing key
docs/                  Setup, Operator's and Architecture and Design guides (AGENTS.md, rule 12)
.specs/                these specifications
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
| Region partitioned (minority) | Cannot commit; API returns 503 | Nodes in majority continue; minority nodes' in-flight transfers fence themselves (veho-job-lifecycle.md §6) |
| Destination throttling/5xx | Step fails transiently | Backoff retry |
| Source object changed mid-copy | ETag/version mismatch | Permanent error `SOURCE_CHANGED` unless policy allows restart |
| Duplicate execution (zombie node) | Two writers to same destination upload | Fencing token + idempotent part writes (veho-job-lifecycle.md §6) |

## 7. Target platforms

### 7.1 Tiers and principles

| Tier | Platforms | Meaning |
| :--- | :--- | :--- |
| 1 | **OpenStack** virtual machines and **LXD** (containers and VMs), on **Ubuntu 24.04 LTS or later** and **Rocky Linux / AlmaLinux** (the RHEL family), under systemd | Designed, built, tested in the lab and documented first |
| 2 | **VMware** (vSphere virtual machines, with the same Linux distributions), **Kubernetes** (container image, manifests) | Supported by design and by the strategies below; tested and documented after tier 1, as it becomes relevant |

Operating systems: Ubuntu and Rocky/Alma are both tier 1 and differ only in packaging, mandatory access control (AppArmor, SELinux), firewall and package versions (§7.2, §7.4). Other systems (the BSDs, for example) may come later and are not planned; Windows is not a target.

- **PLAT-1** Nothing in the core depends on one platform. A feature that works well only on one platform has a **fallback** that works on the others, possibly with weaker properties that the Setup Guide states.
- **PLAT-2** Where several strategies exist for a concern and some are better on a given platform, **all of them are in scope and each is selected by configuration** (never by build or by auto-detection alone), with instructions per platform in the Setup Guide (AGENTS.md rule 12). Implementation is staged by tier (backlog 29): the strategies tier 1 needs come first.
- **PLAT-3** The product treats the platform's services as dependencies it can check but does not manage: it verifies at start what it relies on (for example PostgreSQL's synchronous quorum settings, §7.3) and refuses or warns, instead of assuming a given platform provides them.
- **PLAT-4** The platform is declared in the configuration for what it changes in documentation and defaults (`platform: openstack | lxd | vmware | kubernetes | other`), never for turning code paths on silently.

### 7.2 Strategies by concern

| Concern | Strategies (each configurable) | Best on |
| :--- | :--- | :--- |
| Key of the secrets (SEC-26, SEC-29): **loaded** strategies, where the node loads the key at start | `file` (root-owned 0400; also a Kubernetes Secret mounted as a file, or a key delivered by the orchestrator); `systemd-credential` (encrypted, host key or TPM); `barbican` (OpenStack key manager); `vault-kv` (the key read from Vault at start); `shamir` (shares and peer hand-off, backlog 25) | systemd-credential with a vTPM on OpenStack and LXD VMs; file (Secret) on Kubernetes; `vault-kv` or file on VMware |
| Key of the secrets: **remote** strategies, where the key never leaves the service and the node asks it to unwrap | `vault-transit` (Vault or OpenBao, a cluster per region); `hsm` (a hardware security module, by KMIP, or by PKCS#11 in an out-of-process plugin so that the core stays free of cgo); cloud and vSphere key providers, later | `vault-transit` or `hsm` wherever one exists in the region; the only strategies in which a compromised node cannot copy the key |
| Signer of the audit checkpoints (SEC-28) | `local` (a key wrapped by the KEK, held by the sealer nodes); `vault-transit` (non-exportable); `hsm` | `vault-transit` or `hsm` where available, `local` otherwise |
| Node and caller identity (SEC-20) | SPIFFE (X.509-SVID over mTLS, JWT-SVID); OIDC JWT (including a Kubernetes projected service-account token); API key; operator-issued mTLS certificates | SPIFFE on OpenStack and LXD (the `openstack-spiffe` and `lxd-spiffe` projects); service-account tokens on Kubernetes |
| Packaging and supervision | signed `.deb` and systemd unit (Ubuntu); signed `.rpm` and systemd unit (Rocky/Alma); OCI container image and manifests | `.deb` and `.rpm` on VMs and LXD; image on Kubernetes |
| Location labels of a node (policy topology, executor ID) | explicit configuration; optional discovery from the platform (OpenStack metadata availability zone, Kubernetes node labels) as a default the configuration can override | configuration everywhere |
| State-store high availability | Patroni with etcd on Linux VMs (this document, §3); an externally managed PostgreSQL cluster with the same guarantees, reached by DSN | Patroni on VMs and LXD; external on Kubernetes and VMware |
| Placement of the members | OpenStack server groups (anti-affinity); LXD cluster groups; VMware DRS anti-affinity rules; Kubernetes `topologySpreadConstraints` and pod anti-affinity | each platform's own |
| Encryption at rest | Cinder volume encryption with Barbican; LXD storage-pool encryption; vSphere VM encryption; encrypted persistent volumes | each platform's own |
| Confinement | systemd hardening with AppArmor (Ubuntu) or SELinux (Rocky/Alma); container `securityContext`, seccomp and a read-only root file system | the distribution's own on VMs; the container settings on Kubernetes |
| Load balancing of the API | any balancer that passes HTTP/2 for gRPC (HAProxy, nginx, OpenStack Octavia, a VMware or Kubernetes ingress); or direct to the nodes | the platform's own |
| Time | chrony on the nodes (preferred; the default on Rocky/Alma), `systemd-timesyncd`, or the platform's time source; the node offset is monitored in every case (T-27) | chrony |

A strategy not yet implemented is listed in the backlog with its platform; it never blocks a tier-1 deployment.

### 7.2.1 Where the key lives: loaded or remote

- With a **loaded** strategy the node holds the KEK in memory (the design of SEC-24 to SEC-27). It is simple and has no runtime dependency, but a compromised node can read every tenant's secrets (R-12).
- With a **remote** strategy the KEK is a non-exportable key in Vault Transit or in an HSM; the node sends a wrapped data key to be unwrapped for each attempt, and never holds the KEK. A compromised node can use the service only while it is allowed to call it, which the service can revoke and records. The price is a dependency on that service for every attempt (an outage makes attempts `Transient`, SEC-10) and its latency in the start of every attempt.
- A remote service must therefore be **local to the region**: a cluster per region (Vault or OpenBao), or an HSM per region, with the same keys in each, so that no attempt crosses a region to unwrap a key. A node lists its region's endpoint first and may fall back to the others' (configurable; `veho-latency-envelope.md`).
- Keeping the same key in every regional cluster is the operator's task and depends on the product: Vault Enterprise replicates secrets and keys between clusters; with open-source Vault or OpenBao the key must be created or imported identically in each cluster (to verify: transit key import, and licensing, OI-17); an HSM is cloned or shared by the vendor's own means.
- The core stays free of cgo (NFR-6): an HSM is reached by KMIP (pure Go) or by a PKCS#11 plugin process (`veho-plugins.md` §6, the same loader as the storage plugins), never by linking a vendor library.

### 7.3 What the product checks at start

The node verifies, and logs or refuses (configurable severity): that it connects to a primary directly (no transaction-mode pooler, EXT-4b); that `synchronous_commit` is on and `synchronous_standby_names` names a quorum over standbys in the other regions (NFR-1); that the unwrap key can unwrap the active KEK (SEC-26); that the clock offset is within bounds. This is what lets an externally managed PostgreSQL on tier 2 be accepted safely.

### 7.4 Tier 1 details: Ubuntu 24.04 and Rocky/Alma on OpenStack and LXD

**Rocky Linux / AlmaLinux (RHEL family).** Same systemd, same static binary and the signed `.rpm`; differences to cover in the Setup Guide: SELinux (enforcing by default) needs a policy module for the service instead of the AppArmor profile (backlog 27); firewalld instead of `ufw`; chrony is the default time service; PostgreSQL, Patroni and etcd come from the PGDG and upstream repositories or the distribution's module streams, with versions to verify when the lab is built (the Ubuntu versions below were checked, these were not); `systemd-creds` and `LoadCredentialEncrypted` depend on the systemd version of the release (to verify per release, with `file` as the fallback). The unwrap-key, identity and placement strategies of §7.2 are the same.

**Ubuntu 24.04.**

Checked against the Ubuntu 24.04 archive on 2026-11-21: systemd 255 (`LoadCredentialEncrypted` and `systemd-creds` available), PostgreSQL 16.15, Patroni 3.2.2, etcd 3.4.30, chrony 4.5.

- **Packaging.** The `.deb` is the primary package (signed, `veho-acceptance-matrix.md` §7), installing the service unit; the `.rpm` stays a build artifact. The binary is static, so the glibc of the release does not matter. Go is not taken from the archive: the toolchain is the one pinned in `go.mod`.
- **etcd.** The archive ships 3.4.30; its tuning flags (`heartbeat-interval`, `election-timeout`) are the ones used in §3.3. A newer etcd (3.5 or 3.6, from upstream) is an operator's choice; AC-18 and AC-24 run on the version the deployment uses.
- **Time.** T-27 relies on the database clock, but a node's offset is still monitored. Ubuntu's default time service is `systemd-timesyncd`; use chrony (in the archive) on the nodes and on the PostgreSQL and etcd hosts.
- **Placement.** One VM group per region (OpenStack server groups with anti-affinity, so the nodes of a region do not share a hypervisor); the state-store members of a region on their own VMs and volumes, apart from the `veho serve` nodes where the secrets' unwrap key lives (see below). Neutron security groups open only the API (gRPC and REST), PostgreSQL, etcd (client and peer) and Patroni's REST port between the nodes that need them; every link uses mTLS (SEC-15).
- **Confinement.** The service unit (T-24) is complemented by an AppArmor profile, which Ubuntu enforces by default (backlog 27).
- **The unwrap key on a VM (SEC-26; other strategies in §7.2).** A systemd encrypted credential protects the key with a *host key* in `/var/lib/systemd/credential.secret` on the VM's disk, or, if the flavor gives the VM a virtual TPM (Nova's `hw:tpm_version=2.0`, `vTPM`), with the TPM. Without a TPM, anyone who copies the VM's disk or takes a volume snapshot gets the host key and the credential together, and decrypts the credential. So: use encrypted Cinder volumes (keys in Barbican); use a flavor with a vTPM where available; keep the `veho serve` VMs' volumes apart from the volumes that hold the database and its backups, so that a copy of one does not carry the other; never pass the key in instance user-data or in the metadata service. The Shamir alternative (backlog 25) removes the key from the disk altogether, for those who need that.
- **SPIFFE.** SPIFFE identities for the VMs are available in this environment (the `openstack-spiffe` and `lxd-spiffe` projects), so node identity (SEC-20) can rely on them.

