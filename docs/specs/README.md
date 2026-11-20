# replica — Specifications

Status: **Draft v0.1** · Language: Go · Durable engine: DBOS (Transact for Go) on PostgreSQL

`replica` asynchronously replicates large objects from a source storage system
to destinations in two other regions, durably, with retries, multi-tenancy and
pluggable storage backends.

## How to read these specs

Specs are the source of truth for spec-driven development: code, tests and
docs are derived from them, and a change in behaviour starts as a change here.

| # | Document | Content |
|---|----------|---------|
| 00 | [Overview & requirements](00-overview.md) | Goals, non-goals, glossary, numbered requirements |
| 01 | [Architecture](01-architecture.md) | Topology, state store, components, failure model |
| 02 | [Job lifecycle](02-job-lifecycle.md) | Data model, state machines, workflows, retries, resume |
| 03 | [API](03-api.md) | REST API contract, errors, idempotency |
| 04 | [Storage plugins](04-plugins.md) | Plugin interfaces and conformance suite |
| 05 | [Multi-tenancy & security](05-multitenancy-security.md) | Isolation, authn/z, quotas, secrets, SSRF |
| 06 | [Operations & acceptance](06-operations-acceptance.md) | Observability, deployment, failure scenarios, acceptance tests |
| 07 | [Open questions & decisions](07-open-questions.md) | ADR log and open questions |
| 08 | [DBOS spike report](08-spike-dbos.md) | Experiments resolving the DBOS assumptions |

## Conventions

- **MUST / SHOULD / MAY** follow RFC 2119.
- Requirements have stable IDs (`FR-*` functional, `NFR-*` non-functional,
  `SEC-*` security, `PLG-*` plugin, `API-*`). Tests reference these IDs.
- Items marked **[VERIFY]** are assumptions about third-party behaviour
  (mainly DBOS Go v0.11.0). The DBOS ones were resolved by the spike in
  [08](08-spike-dbos.md); remaining questions are in [07](07-open-questions.md).

## Proposed workflow

1. Review and agree these specs (resolve 07 §C, notably Q-12 on WAN RTT).
2. Derive an implementation plan and acceptance tests per requirement ID.
3. Implement in vertical slices: (a) single-region happy path with the
   local-filesystem plugin, (b) retries/resume, (c) multi-region state store,
   (d) S3 and Glance plugins, (e) multi-tenancy hardening.
