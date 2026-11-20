# 00 — Overview & requirements

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
| Region | Failure domain (A, B, C). Each has storage and ≥1 `replicad` node |
| Node | A running `replicad` process (API + workflow executor) |
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
  to the replicated state store (see 01 §3).
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
  (see 02 §6, fencing).
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
  02 §7 until success, permanent failure, cancellation or deadline.
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
  impossible (05).

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
  OpenTelemetry traces propagated across steps (06).
