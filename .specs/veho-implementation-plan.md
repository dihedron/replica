# veho: implementation plan

This file plans the implementation in chunks. It is part of `veho_specification.md`. Each chunk is a pull request with its own review.

Status: draft, 2026-11-21, for review. The specifications it relies on are drafts too; backlog item 2 (their approval) comes first.

## 1. Rules for every chunk

- **Spec first.** The chunk names the sections of the specifications it implements; they are approved before the code starts (AGENTS.md rules 1 and 2, in plan mode).
- **Tests first.** Unit or integration tests that match the acceptance criteria come before the implementation (rule 4); fuzz tests and benchmarks where the plan says so (rule 9).
- **Gates before the commit:** `go test ./...`, `make lint`, `govulncheck`, the secret scanners (rules 6, 7 and 13). Documentation is updated with the code (rule 11).
- **Compliance audit at the end** of the chunk: the implementation against the API contracts, data model and edge cases of the spec (rule 5).
- **Errors, context and logging** follow AGENTS.md: wrapped errors, `context.Context` everywhere, `slog` with the contextual keys, no secrets in logs.
- **Copy from ligo, with a sync note.** Where a chunk copies ligo's code (C2, C3, C4, C6), the commit records the source commit and the divergences (`veho-audit-housekeeping.md` §7).

## 2. The chunks

| Chunk | Scope | Specifications | Where it runs |
| :--- | :--- | :--- | :--- |
| **C0** Foundation | Commit the specs; delete the scaffold (`internal/step`, `internal/workflow`); the root command, `.env` loading, logging and `veho version` stay as they are. Configuration and the metrics registry are built by the chunks that need them (C2 and C6), not ahead of them | `veho-architecture.md` §4 | here |
| **C1** Topology and policy | `internal/topology` (tree of locations, named levels) and `internal/policy`: parse, validate (§4), `Evaluate` (§5). Property tests (the verdict is exactly one of three; monotonic); a fuzz test of the parser and of `Evaluate` against a brute-force oracle on small cases | `veho-distribution-policy.md` | here; pure Go |
| **C2** State store | `internal/store`: the pools, `veho migrate`, the core schema (tenants, locations, endpoints, policies, jobs, transfers, parts, executors), row-level security, the database roles (`packaging/postgres/roles.sql`), the advisory-lock helper (copied from ligo), the start-up checks of §7.3 that concern PostgreSQL. The secrets and KEK tables come with C3, the audit tables and `internal/jcs` with C4 | `veho-store.md` | the lab (`veho-lab.md`, L1). **Done 2026-11-21** |
| **C3** Secrets | `internal/secrets`: the KEK-provider interface; loaded providers `file` and `systemd-credential`; envelope encryption, rotation; the write-only secret service; fail-fast unwrap (copied from ligo's `sealing`, adapted) | `veho-tenancy.md` §3 (SEC-9 to SEC-11, SEC-24 to SEC-29); `veho-secrets.md` | here. **Done 2026-11-21**; the secret API and `veho secret` come with C7, the background re-wrap with the housekeeping loop, the audit entries with C4 |
| **C4** Audit | `internal/audit`, lightweight: one structured event per action on the system log (`authpriv`, `veho-audit`) or standard error, forwarded to the SIEM by the platform; the secrets and `veho kek` audited. The audit table, chain, checkpoints, signer, export and pruning are backlog 32 | `veho-audit.md` (`veho-audit-housekeeping.md` §1 to §5 deferred) | here. **Done 2026-11-21** |
| **C5** Plugins | `pkg/plugin` and its protobuf; the in-process and go-plugin loaders; a `file` driver; the conformance suite run under both loaders; the S3 driver (Garage); the benchmark of PLG-17 | `veho-plugins.md` | here for the file driver and the loaders; the S3 driver against the lab's Garage (`veho-lab.md`, L2). **Done 2026-11-21** (C5a to C5c, `veho-plugins.md` §7) |
| **C6** Engine | `internal/engine`: transactional acceptance, the Job and Transfer workflows, part checkpoints, leases and fences, the executor registry, retries, partitioned queues, policy evaluation under the job lock, cancel and retry; the housekeeping loop (zombies, retention, the audit sealer); the DBOS spike promoted into the AC-21 suite | `veho-engine.md` (C6a to C6e), `veho-job-lifecycle.md`, `veho-audit-housekeeping.md` §3 | here, with PostgreSQL; multi-region cases in the lab. C6a done 2026-11-21 |
| **C7** API | Protobuf definitions, gRPC services, the REST gateway and the OpenAPI document; interceptors for authentication (OIDC, API key, SPIFFE), authorization (roles and policy grants), quotas, rate limits and audit; jobs, policies, endpoints, secrets, tenants, audit; `WatchJob`; idempotency | `veho-api.md`, `veho-tenancy.md` | here |
| **C8** CLI | `veho job`, `policy`, `endpoint`, `secret`, `audit` and the process commands over gRPC, with `--api rest`; `job wait` | `veho-api.md` §4 | here |
| **C9** Lab | `test/lab/` (`veho-lab.md`): its L1 and L2 steps (three regions, the emulated WAN, Patroni and etcd, Garage per region) come with C2 and C5; C9 adds L3, the veho nodes and the acceptance suite (AC-1 to AC-26); AC-18 and AC-24 overnight; rolling upgrade (AC-20); chaos runs | `veho-acceptance-matrix.md`, `veho-latency-envelope.md` | **your machine** |
| **C10** Web UI | Its own specification first (backlog 19), then the code | `veho-tenancy.md` SEC-22, T-34 | after C7 |
| **C11** Release | Documentation pipeline and the three guides; Ubuntu and Rocky/Alma packaging (AppArmor, SELinux); the signing key; Sonar and Codecov; a pre-release | `veho-acceptance-matrix.md` §7, backlog 7, 8, 9, 27 | your machine for the key and the services |

## 3. Order and dependencies

```
C0 ──┬─ C1 ─────────────────────────┐
     ├─ C2 ──┬─ C3 ─┐               │
     │       └─ C4 ─┤               ├─ C6 ── C7 ── C8 ── C9 ── C10, C11
     └─ C5 ─────────┴───────────────┘
```

C1, C2 and C5 start after C0 and can run in parallel. C3 and C4 need C2. C6 needs C1 to C5. C7 needs C3, C4 and C6. The lab (C9) is started earlier, in small steps, as soon as C2 gives it something to test; the full suite waits for C8.

## 4. First step

C0 and C1 together: they need nothing outside this repository, C1 is pure Go and carries the most delicate logic (the verdict of a job), and it is the part the topology deep dive (backlog 17) most depends on, so having it executable makes the deep dive concrete.

## 5. What must be settled first

| Before | Settle |
| :--- | :--- |
| C0 | Approval of the specifications (backlog 2) and the threat model review (backlog 1) |
| C1 | The topology model for v1: a tree with named levels (`veho-distribution-policy.md` §2); the deep dive can change the store of it later, not `Evaluate` |
| C2 | The defaults proposed for retention, quotas and the database roles (`veho_specification.md`, threat model open points) |
| C5 | The S3 driver is first; the Glance driver waits for the OpenStack release and upload mechanism (backlog 21) |
| C7 | The choice of the REST generation library (`grpc-gateway`, see ADR-10) |

## 6. Later, not in this plan

Webhooks, Keystone, the Glance driver, Vault and HSM (KMIP) providers, the Shamir unseal, locality-aware executors, server-side copy, reading from the nearest verified copy: backlog items 11, 13, 14, 16, 20, 21, 25, 26 and 31.
