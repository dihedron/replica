# 05 — Multi-tenancy & security

## 1. Tenant model

- **SEC-1** Every persistent record and every workflow carries `tenant_id`.
  Workflow IDs and idempotency keys are namespaced by tenant.
- **SEC-2** Tenant is derived **only** from the authenticated principal.
- **SEC-3** A tenant owns: endpoints (with credential references), named
  policies, jobs/transfers, quotas, API keys / OIDC mapping.
- **SEC-4** Roles per tenant: `admin` (endpoints, policies, keys, quotas
  within limits), `operator` (jobs: submit/cancel/retry/read), `viewer`
  (read-only). A global `platform-admin` role manages tenants and global
  limits, and cannot read tenant data without an explicit audited
  break-glass action.

## 2. Data isolation

- **SEC-5** PostgreSQL **row-level security** on all tenant tables:
  `USING (tenant_id = current_setting('replica.tenant_id')::uuid)`.
  The API sets it per transaction (`SET LOCAL`). The app DB role is not a
  superuser and does not have `BYPASSRLS`; the reconciler and workflow
  executors use a separate role with an explicit, audited scope.
- **SEC-6** Cross-tenant references (a job pointing to another tenant's
  endpoint) are rejected by composite foreign keys `(tenant_id, id)`.
- **SEC-7** DBOS workflow inputs contain IDs only — no credentials, no
  tokens, no presigned URLs.
- **SEC-8** Data-plane isolation: credentials are resolved per transfer
  attempt and scoped to that tenant's endpoints; a driver `Client` is never
  reused across tenants.

## 3. Secrets

- **SEC-9** Endpoint credentials are stored as references
  (`credential_ref`): `vault://…`, `aws-sm://…`, `k8s-secret://…`, or
  `enc:` (envelope-encrypted blob in DB with a KMS-wrapped data key per
  tenant) as the minimal built-in. Plaintext secrets MUST NOT be stored in
  `endpoints.config` and MUST NOT be returned by the API.
- **SEC-10** Credentials MUST be fetched just-in-time and kept in memory only
  for the duration of an attempt; rotation is picked up on the next attempt.
- **SEC-11** Preferred auth for cloud sources/destinations is workload
  identity / role assumption (e.g. STS `AssumeRole` with external ID per
  tenant) over static keys.

## 4. Quotas and noisy-neighbour control

Per tenant (enforced in 02 §8): max active jobs, max bytes in flight, max
object size, max destinations per job, allowed regions, allowed endpoints,
bandwidth ceiling (token bucket). Defaults come from platform config; tenant
overrides are bounded by platform maximum. Metrics are labelled by tenant
(bounded cardinality).

## 5. SSRF and URL safety

Because clients submit URLs, the system could be abused to read internal
resources with the node's network position.

- **SEC-12** A submitted `source.url` MUST resolve to a **registered endpoint
  of that tenant** (match by driver scheme + authority/bucket). Arbitrary
  URLs are rejected. `http(s)` sources require a registered endpoint with an
  allowlisted host.
- **SEC-13** For `http` driver: resolve DNS and reject
  loopback/link-local/private/metadata ranges unless the endpoint is
  explicitly flagged `allow_private`, re-check at connect time (anti DNS
  rebinding), no redirects across hosts.
- **SEC-14** Path/key normalisation to prevent traversal in the `file` driver
  and in destination templates.

## 6. Transport and API security

- **SEC-15** TLS 1.2+ everywhere (API, Postgres, etcd, inter-node); mTLS for
  Postgres/etcd.
- **SEC-16** API keys stored hashed (argon2id/sha256+pepper), shown once.
- **SEC-17** Audit log (append-only table + log stream): tenant, principal,
  action, resource, result, request ID — for submissions, cancellations,
  endpoint and credential changes, break-glass.
- **SEC-18** Request size limits, rate limits per tenant/principal, strict
  JSON decoding.
- **SEC-19** Container: non-root, read-only FS, no cgo static binary;
  dependency scanning (existing GitHub/dependabot config), signed releases
  (existing goreleaser config).

## 7. Tenant lifecycle

Create → active → suspended (no new jobs; running jobs continue or pause per
flag) → deleted (cancel jobs, abort uploads, purge rows and secrets after
retention). Destination data is never deleted by tenant deletion.
