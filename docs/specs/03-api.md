# 03 — API

REST/JSON over HTTPS, versioned under `/v1`. OpenAPI 3.1 document is
generated from / checked against the implementation (API-1).

## 1. Conventions

- **API-2** Auth: `Authorization: Bearer <JWT>` (OIDC) or API key; tenant
  resolved from token claim `tenant` (05). Tenant is never taken from the
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
      { "region": "B", "endpoint": "b-archive", "path": "replicas/big.tar" },
      { "region": "C", "endpoint": "c-archive", "path": "replicas/big.tar" }
    ],
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
- `source.url`: resolved to a registered endpoint by scheme+authority (05 §5).
- `overwrite`: `fail` (default; error if exists with different digest, succeed
  if identical), `replace`, `skip`.
- A named policy MAY be used instead: `"policy": { "ref": "geo-3x" }`
  (policies stored per tenant; `POST /v1/policies`). Inline policy is the v1
  minimum; named policies are SHOULD.

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
  "progress": 0.75
}
```
`bytes_done` advances at part granularity (staleness ≤ one batch).
Clients SHOULD poll with `ETag`/`If-None-Match`; response includes
`Retry-After` hinting the next poll.

### List jobs
`GET /v1/jobs?state=&since=&source=&label.ticket=&limit=&cursor=`

### Cancel job
`POST /v1/jobs/{id}:cancel` → `202`; idempotent. Terminal jobs → `409`.

### Retry failed transfers
`POST /v1/jobs/{id}:retry` body `{ "transfers": ["…"] }` (default: all
`FAILED`) → `202`.

### Endpoints (tenant admin)
`POST|GET|PUT|DELETE /v1/endpoints[/{name}]` — driver, region, config,
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

## 4. CLI

`replicad` (server) and `replicactl` (client) built from the same module,
using the existing go-flags scaffolding: `replicactl submit|get|wait|cancel|retry|endpoints …`.
`wait` polls until terminal and exits non-zero on `FAILED`.
