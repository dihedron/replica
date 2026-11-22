# veho: API

This file specifies the REST API and the command line clients. It is part of `veho_specification.md`.

Status: draft, 2026-11-21, for review. The server as built and its amendments of this document are in `veho-api-server.md` (§10): the source is `{endpoint, path}`, retry restarts every failed transfer, submission does not return `422`, labels filter as `label[<k>]=<v>`, and `AuditService` waits for backlog 32.

Two interfaces to one service: **gRPC** (protobuf over HTTP/2) and **REST/JSON** over HTTPS, both versioned (`/v1`, package `veho.v1`). gRPC is preferred for application-to-application flows and for the CLI; REST stays for browsers' simple needs, scripts and tools. The protobuf definitions in `api/` are the **single source of truth** (API-1): the REST interface and the OpenAPI 3.1 document are generated from them, so the two cannot drift.

## 0. gRPC

- **API-8** One protobuf service per resource (`JobService`, `PolicyService`, `EndpointService`, `AuditService`, `AdminService`) with `google.api.http` annotations that give the REST mapping of §2. The REST gateway is generated (`grpc-gateway`) and runs in the same process, on the same handlers, so authentication, authorization, quotas and audit are implemented once, in gRPC interceptors.
- **API-9** The CLI uses gRPC by default; `--api rest` (or `VEHO_API=rest`) selects REST, for environments where only HTTP/1.1 passes.
- **API-10** Streaming, client-initiated like a poll (OI-8): `JobService.WatchJob(id)` is a server stream of job and transfer changes until the job is settled or the client cancels, and `WatchJobs(filter)` does the same for a set; REST's long poll (`?wait=`) is its counterpart. The server never connects to the client.
- **API-11** Authentication per call: mTLS with an X.509-SVID (SPIFFE) as the preferred identity for application-to-application calls, a JWT (OIDC or JWT-SVID) or an API key in metadata (SEC-20). A stream is authorized when it opens and again at every refresh interval, so a revoked right ends it.
- **API-12** Errors map to gRPC status codes with a typed `ErrorInfo` detail carrying the same stable `code` and `retryable` as REST's `problem+json` (API-3); the table of §3 gets a gRPC column in the proto documentation. `Idempotency-Key` is a request field and metadata (API-4).
- **API-13** Server reflection is off in production, messages are size-limited, calls have deadlines, and keepalives are enforced (T-36). Health follows the gRPC health protocol next to `/healthz`.
- **API-14** The plugin protocol (`pkg/plugin`, `veho-plugins.md`) is a separate protobuf package and is not part of this API.

## 1. Conventions

- **API-2** Auth: `Authorization: Bearer <JWT>` (OIDC) or API key; tenant
  resolved from token claim `tenant` (veho-tenancy.md). Tenant is never taken from the
  URL or body.
- **API-3** Errors: RFC 9457 `application/problem+json` with `code`
  (stable machine string), `retryable` bool.
- **API-4** `Idempotency-Key` header on `POST /v1/jobs` (FR-2); same key +
  different body → `409 idempotency_key_reuse`.
- **API-5** Timestamps RFC 3339 UTC; sizes in bytes (int64).
- **API-6** Pagination: cursor-based (`?limit=&cursor=`).
- **API-7** Every response carries `X-Request-Id`.

## 2. Endpoints

### Create job
`POST /v1/jobs`
```json
{
  "source": { "url": "s3://tenant-a-data/ingest/big.tar" },
  "policy": {
    "destinations": [
      { "endpoint": "b-archive", "path": "replicas/big.tar" },
      { "endpoint": "c-archive", "path": "replicas/big.tar" }
    ],
    "completion": { "min_copies": 2, "spread": [ { "level": "region", "min_distinct": 2 } ] },
    "on_satisfied": "complete",
    "verify": "sha256",
    "overwrite": "fail",
    "deadline": "168h",
    "retry": { "initial": "2s", "max_interval": "15m" }
  },
  "labels": { "ticket": "OPS-1234" }
}
```
`202 Accepted`, `Location: /v1/jobs/{id}`
```json
{ "id": "6f1c…", "state": "ACCEPTED", "created_at": "…" }
```
- The policy is specified in `veho-distribution-policy.md`: N >= 1
  destinations (the location of each comes from its registered endpoint), the
  completion rule `min_copies` of N plus spread statements over the levels of
  the topology, and `on_satisfied`. The source is external and is not counted.
- `source.url`: resolved to a registered endpoint by scheme+authority (veho-tenancy.md §5).
- `overwrite`: `fail` (default; error if exists with different digest, succeed
  if identical), `replace`, `skip`.
- A named policy is used by reference: `"policy": { "ref": "geo-3x", "version": 3 }`
  (`version` optional: the latest at acceptance; the job stores a snapshot).
  Named policies are in v1. An inline policy is accepted only from a principal
  who may submit ad-hoc policies and only if the tenant allows it (SEC-21).

### Get job
`GET /v1/jobs/{id}` → `200`
```json
{
  "id": "6f1c…", "state": "RUNNING",
  "source": { "url": "s3://…", "size": 107374182400, "version": "etag-…" },
  "created_at": "…", "updated_at": "…", "deadline_at": "…",
  "transfers": [
    { "id": "…", "region": "B", "state": "COPYING",
      "bytes_done": 53687091200, "bytes_total": 107374182400,
      "attempt": 2, "last_error": { "code": "throttled", "message": "…", "at": "…" } },
    { "id": "…", "region": "C", "state": "SUCCEEDED",
      "bytes_done": 107374182400, "bytes_total": 107374182400,
      "sha256": "…", "finished_at": "…" }
  ],
  "completion": { "min_copies": 2, "spread": [ { "level": "region", "min_distinct": 2 } ] },
  "copies": { "done": 1, "total": 2, "by_level": { "region": { "B": 0, "C": 1 } } },
  "satisfied_at": null, "completed_at": null, "settled_at": null,
  "progress": 0.5
}
```
`bytes_done` advances at part granularity (staleness ≤ one batch).
Long polling: `?wait=<seconds>` (maximum 60) with `If-None-Match` holds the request until the job changes or the wait ends, answering `304` on timeout; it is a client-initiated request like any poll (OI-8).
Clients SHOULD poll with `ETag`/`If-None-Match`; response includes
`Retry-After` hinting the next poll.

### Get transfers
`GET /v1/jobs/{id}/transfers` → `200`: the `transfers` array of the job view, one entry per destination (N >= 1), with the same fields and the same staleness. Use it to follow the progress across all replicas without fetching the whole job.

### List jobs
`GET /v1/jobs?state=&since=&source=&label.ticket=&limit=&cursor=`

### Cancel job
`POST /v1/jobs/{id}:cancel` → `202`; idempotent. Terminal jobs → `409`.

### Retry failed transfers
`POST /v1/jobs/{id}:retry` body `{ "transfers": ["…"] }` (default: all
`FAILED`) → `202`.

### Policies
Policies are tenant resources with immutable versions (`veho-distribution-policy.md`).
- `POST /v1/policies` creates a policy or, for an existing name, its next version.
- `GET /v1/policies[/{name}[?version=]]`, `DELETE /v1/policies/{name}` (refused
  while a running job uses it).
- `GET|PUT /v1/policies/{name}/grants`: who may `submit`, `read` or `manage` the
  policy (SEC-21).
- `POST /v1/policies:validate` checks a policy document without storing it and
  reports what would be refused (`veho-distribution-policy.md` §4).

### Secrets (tenant admin)
`PUT /v1/secrets/{name}` creates or replaces a secret (write-only: the value
is in the request and never in any response), `GET /v1/secrets[/{name}]` shows
metadata only (name, version, times, KEK version), `DELETE` removes it (refused
while an endpoint references it). Referenced from an endpoint as
`credential_ref: "db://{name}"`. Every call is audited without the value.

### Endpoints (tenant admin)
`POST|GET|PUT|DELETE /v1/endpoints[/{name}]` — driver, location, config,
`credential_ref`. `POST /v1/endpoints/{name}:test` performs a connectivity
and permission probe (list/stat, and a zero-byte write+delete for sinks).

### Operational (unauthenticated or admin-only)
`GET /healthz` (liveness), `GET /readyz` (state store reachable, DBOS
launched), `GET /metrics`.

## 3. Status/response codes

| Code | When |
|------|------|
| 202 | job accepted, cancel/retry accepted |
| 400 | schema/policy invalid |
| 401/403 | authn/authz failure; source or endpoint not allowed for tenant |
| 404 | job not found **or belongs to another tenant** (indistinguishable) |
| 409 | idempotency conflict; invalid state transition |
| 422 | source `Stat` says object definitively does not exist |
| 429 | tenant quota; includes `Retry-After` |
| 503 | state store has no quorum (NFR-2); `retryable: true` |

## 4. Command line

One binary, `veho`, built with `github.com/jessevdk/go-flags` and following
the convention of `AGENTS.md`: `veho <object> <verb>`, where the verb that
displays an object is `show`, and the process commands are `serve`, `migrate`,
`version` and `completion`.

| Command | Purpose |
| :--- | :--- |
| `veho serve` | run a node: the API and the workflow executor (one process, any region) |
| `veho migrate` | apply the schema migrations to the state store |
| `veho bootstrap tenant \| apikey \| entitlement` | the first tenants, API keys and entitlements, written in the database as `veho_migrator` (`veho-cli.md` §1) |
| `veho kek show \| init \| rotate \| rewrap \| rekey` | the key-encryption keys of the secrets, as `veho_migrator` with the unwrap key (`veho-secrets.md` §4) |
| `veho job submit \| show \| list \| wait \| watch \| cancel \| retry` | client of the API; `wait` exits 0 on `SUCCEEDED`, 3 on `FAILED`, 4 on `CANCELLED` |
| `veho policy create \| show \| list \| delete \| validate \| grant \| revoke` | named policies and who may submit them |
| `veho endpoint create \| show \| list \| update \| delete \| test` | tenant endpoints |
| `veho secret set \| show \| list \| delete` | tenant secrets (the value is read from a file or standard input, never from an argument) |
| `veho apikey create \| list \| revoke`, `veho entitlement create \| list \| delete`, `veho settings show \| update` | a tenant admin's administration of its tenant |
| `veho tenant create \| show \| list \| update` | the tenants, for platform administrators |
| `veho version [--verbose]` | the build metadata (name, version, license, author, build and packaging environments) |
| `veho completion bash \| zsh` | shell completions: a function that asks the binary (go-flags), so they always match it |
