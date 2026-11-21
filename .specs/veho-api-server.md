# veho: the API server (chunk C7)

**Status:** approved 2026-11-21. C7a to C7c done; C7c (§5.1) was decided under the overnight delegation of 2026-11-22, for review. Requirements: `veho-api.md` (API-1 to API-14, the endpoints of §2, the codes of §3), `veho-tenancy.md` §1, §4, §5, §6 (SEC-2 to SEC-4, SEC-12, SEC-16, SEC-18, SEC-20, SEC-21, SEC-23), FR-1 to FR-3, FR-14 to FR-16, NFR-2, NFR-4; threats T-03, T-06, T-34 (later, with the UI), T-36. It fixes what those leave open, and amends them where noted (§10). The engine's `Submit`, `Cancel`, `Retry` and `Status` (C6) and the secrets' `Store` (C3) are the handlers' back end.

## 1. Steps

| Step | Scope |
| :--- | :--- |
| **C7a** Core and jobs | `api/veho/v1/*.proto` (jobs, errors), `make proto`; one TLS listener serving gRPC and the REST gateway; the interceptor chain (§3); authentication by **API key**; the identity tables (§4); `veho tenant` and `veho apikey` (bootstrap, §4.3); `JobService`: create, get, transfers, list, cancel, retry, `WatchJob`, REST long poll; `/healthz`, `/readyz`; idempotency with the request's hash |
| **C7b** Identities | OIDC bearer JWTs (`coreos/go-oidc/v3`), SPIFFE X.509-SVIDs over mTLS and JWT-SVIDs (`spiffe/go-spiffe/v2`, as ligo); entitlements for them; streams re-authorised at each refresh (API-11) |
| **C7c** Policies | `PolicyService`: named policies with immutable versions, grants (`submit`, `read`, `manage`), `:validate`; jobs by reference; inline policies gated by `allow_inline_policies` and `submit_inline` (SEC-21) |
| **C7d** Administration | `EndpointService` (CRUD, `:test`), `SecretService` (write-only), `TenantService` and `AdminService` (entitlements, API keys, quotas) for platform and tenant admins; per-tenant and per-principal rate limits (copied from ligo's `internal/ratelimit`); the OpenAPI 3.1 document generated from the protos and linted |

`AuditService` (`GET /v1/audit`) is not built: the audit is the system log (`veho-audit.md`); it comes with backlog 32.

## 2. Transport (C7a)

- **One port**, `api.listen` [`:8443`], TLS 1.3 (`api.tls.cert_file`, `api.tls.key_file`), ALPN `h2` and `http/1.1`. A request whose content type is `application/grpc*` goes to the gRPC server, any other to the REST gateway (`grpc-gateway/v2`, in-process: it calls the same service implementations through their in-process client, so every interceptor runs for REST too, API-8, SEC-23). No plaintext listener.
- gRPC: no reflection, 4 MiB messages, a 30 s deadline when the client gives none (except `WatchJob`), keepalive enforcement (T-36). The gRPC health service and `/healthz` (process up) and `/readyz` (the state store answers, DBOS launched, not fenced).
- REST: JSON with unknown fields refused (SEC-18), bodies at most 1 MiB, errors as `application/problem+json` (API-3).

## 3. The interceptor chain (C7a)

In this order, for unary calls and streams alike:

1. **request ID**: the caller's `X-Request-Id` if it is a UUID, else a new one; logged with every line (`request_id`), returned (API-7);
2. **authentication**: the credential (§4) gives a principal: its ID, tenant (none for a platform admin), role and groups; none or a bad one: `Unauthenticated` (401);
3. **rate limit** (C7d): per tenant and per principal, token buckets; over: `ResourceExhausted` (429) with `Retry-After`;
4. **authorisation**: each method declares the role it needs (`viewer` < `operator` < `admin`, and `platform-admin` for tenants); short: `PermissionDenied` (403); a resource of another tenant is `NotFound` (INV-4);
5. **audit context**: `audit.WithRequest` with the principal and the request ID, so that the back end's events carry them;
6. **errors**: the back end's errors mapped to codes once (§6), with an `ErrorInfo` detail (`reason` = the stable `code`, `metadata.retryable`), sanitised (T-06).

The tenant is the principal's, never the request's (SEC-2, API-2).

## 4. Identities (C7a, C7b)

### 4.1 Tables (in `001_core.sql`)

```sql
veho.principals (id uuid, tenant_id uuid NULL, name text, role text CHECK (role IN ('viewer','operator','admin','platform-admin')),
                 created_at, disabled_at)          -- tenant_id NULL only for platform-admin
veho.api_keys (id text PRIMARY KEY, principal_id uuid, sha256 bytea, created_at, expires_at, revoked_at, last_used_at)
veho.entitlements (id uuid, tenant_id uuid NULL, kind text CHECK (kind IN ('oidc','spiffe')), issuer text, subject text,
                        "group" text, role text, created_at)   -- C7b
```

Row-level security as for the other tenant tables; authentication reads them through a `SECURITY DEFINER` function (`veho.authenticate_key(id)`), because the tenant is not known before.

### 4.2 API keys

- A key is `veho_<id>_<secret>`: `id` 12 random characters (the lookup), `secret` 32 random bytes in base64url. The database keeps `SHA-256(secret)`, compared in constant time. **Amendment to SEC-16**: no argon2id and no pepper, because the secret is 256 random bits: a slow hash protects guessable passwords, not random keys, and costs CPU on every call.
- Shown once, at creation. Expiry optional; revocation immediate (looked up on every call; `last_used_at` updated at most once a minute).

### 4.3 Bootstrap

`veho tenant create <name>` and `veho apikey create --tenant <name> --role <role> --name <name>` (and `--platform-admin`) run as `veho_migrator` with its DSN from the environment, like `veho migrate` and `veho kek`, and print the new key once. They are how the first platform administrator and each tenant's first admin get a key; afterwards the API's `AdminService` (C7d) does the same. Audited.

### 4.4 OIDC and SPIFFE (C7b)

- **Entitlements decide the tenant and the role.** An **entitlement** gives an identity a role in a tenant (or the platform-admin role); the term replaces "identity binding", so as not to clash with ligo's *binding* (§10). An identity is mapped by `veho.entitlements` rows: `(kind, issuer, subject)` for one identity, or `(kind, issuer, group)` for the members of a group, each with a tenant (none for `platform-admin`) and a role. The token's claims never name the tenant (SEC-2): the `tenant_claim` of §7 is dropped (**amendment**). An identity entitled in more than one tenant is refused (`Unauthenticated`, `ambiguous identity`) rather than guessed; within one tenant the highest role of its entitlements applies.
- **OIDC** (`coreos/go-oidc/v3` v3.21.0): the bearer token of an issuer listed in `api.oidc` (`issuer`, `audience`, `groups_claim` [`groups`], `ca_file` for the issuer's TLS), verified by signature (the issuer's JWKS, fetched at start and refreshed by the library), issuer, audience and expiry. Subject is `sub`; groups the configured claim (a list of strings).
- **SPIFFE X.509-SVID** (`spiffe/go-spiffe/v2` v2.9.0): the API's TLS asks for a client certificate (`RequestClientCert`); a certificate given is verified against the X.509 bundle of `api.spiffe.trust_domain` (`api.spiffe.x509_bundle_file`) with `x509svid.Verify`; the SPIFFE ID (its URI SAN) is the subject, the trust domain the issuer. A call without a certificate uses its bearer credential, as before. Over REST the gateway passes the verified ID to the gRPC server in internal metadata, which the server accepts **only from the in-process connection** (its peer is the pipe), so that a client cannot claim an ID by sending the header.
- **JWT-SVID**: a bearer JWT whose `sub` is a `spiffe://` ID of the trust domain, verified against the JWT bundle (`api.spiffe.jwt_bundle_file`, a JWKS) for the audience `api.spiffe.audience` (`jwtsvid.ParseAndValidate`). Other bearer JWTs are OIDC tokens; tokens starting with `veho_` are API keys.
- **Re-authorisation** of a stream (API-11) re-verifies the token (its expiry included) or the certificate's entitlement every `Reauthorise`.
- **Bootstrap**: `veho entitlement create --kind oidc|spiffe --issuer <iss> (--subject <sub> | --group <g>) (--tenant <name> --role <role> | --platform-admin)`, as `veho_migrator` like `veho apikey create`; the API to manage entitlements is C7d. Audited.
- Bundles and issuers are read at start; a bundle file that changes is read again within a minute (SPIRE rotates bundles).

## 5. Jobs (C7a)

`JobService` maps `veho-api.md` §2, with these decisions:

- **Source as endpoint and path** (**amendment to `veho-api.md`**: the body has `"source": {"endpoint": "ingest", "path": "in/big.tar"}`, like the destinations, instead of `source.url`). A URL would need each driver to map a URL back to an endpoint's configuration; naming the endpoint is shorter, and SEC-12 holds by construction: the endpoint must be the tenant's.
- **Idempotency**: the job keeps the SHA-256 of the canonical request (source, policy, labels; `jobs.request_sha256`); the same key with another request is `409 idempotency_key_reuse` (API-4).
- **Labels**: `jobs.labels jsonb`, at most 16 pairs of at most 63 and 255 characters; `List` filters by state, creation time, source endpoint and labels (`label[<k>]=<v>`, the gateway's form of a map in a query; **amendment** of `veho-api.md`'s `label.<k>`), cursor-paginated on `(created_at, id)`, at most 500 per page (API-6).
- **Get**: the job view of `veho-api.md` §2, from `engine.Status`; the transfer's `region` becomes its position (`{"region": "B"}`); `ETag` is a hash of the view; `?wait=` (≤ 60 s) with `If-None-Match` re-reads the view every second until it changes (`304` at the end).
- **`WatchJob`**: the same loop as a server stream, sending the view when it changes, ending when the job is settled; re-authorised every 60 s (API-11). A node streams at most `api.max_watches` [1000] at once.
- **Retry** of chosen transfers (`{"transfers": [...]}`) is not built: `Retry` restarts all the failed and cancelled ones (`veho-engine.md` §4.6). **Amendment.**
- Submission does not `Stat` the source (NFR-4: p99 < 1 s); a missing source fails the job at once in `resolve` (`veho-engine.md` §4.5), so `422` of `veho-api.md` §3 is not returned at submission. **Amendment.**

### 5.1 Named policies and grants (C7c)

Decided under the overnight delegation of 2026-11-22, for review.

- **Policies** (`PolicyService`): `CreatePolicy(name, policy)` stores version 1, or the next version of an existing name, after validating it as a submission would (the tenant's endpoints and topology); versions are immutable. `GetPolicy(name[, version])` (default the latest), `ListPolicies` (the latest version of each), `DeletePolicy(name)`, refused (`409 in_use`) while a job that is not terminal uses one of its versions; a deleted policy's rows stay (finished jobs reference them) with `deleted_at` set, and are invisible; creating the name again continues its numbering. `ValidatePolicy(policy)` returns the first problem, or none, and stores nothing.
- **Grants** (`GetGrants`, `SetGrants`): a grant gives a **grantee** a **permission** on a policy: `submit` (submit jobs under it), `read` (see it), `manage` (new versions, grants, delete). A grantee is one principal, named as the API shows it (`key:<name>` for an API key's principal, `oidc:<sub>`, `spiffe:<id>`), or `group:<name>` for the members of an OIDC group. `SetGrants` replaces all the grants of a policy.
- **Who may do what.** A tenant `admin` may do everything on every policy. Others: `read` (or `submit` or `manage`, which imply it) to see a policy; `submit` to submit a job by reference; `manage` to change it. Creating a new name is for admins. A policy the caller may not see is `404`, as a missing one (SEC-21, INV-4).
- **Inline policies.** The tenant's setting `inline_policies` (`tenants.settings`): `all` (the default: any operator, as in C7a), `granted` (only principals with the tenant-wide permission `submit_inline`, a grant without a policy), or `none`. A refused inline policy is `403 inline_policy_refused`.
- **Jobs by reference.** `CreateJob` takes `policy_ref {name, version}` instead of `policy`; without a version, the latest at acceptance. The job stores the reference and a snapshot of the spec (`veho-distribution-policy.md`); the snapshot is validated again, since the endpoints may have changed since the version was stored.
- **Principals carry their groups** (OIDC), for the grants.

## 6. Errors

| Back end | gRPC | HTTP | `code` | retryable |
| :--- | :--- | :--- | :--- | :--- |
| `engine.ErrNotFound`, `secrets.ErrNotFound` | NotFound | 404 | `not_found` | no |
| `*policy.Error` | InvalidArgument | 400 | its code | no |
| malformed request | InvalidArgument | 400 | `invalid_request` | no |
| idempotency key reused | AlreadyExists | 409 | `idempotency_key_reuse` | no |
| `engine.ErrState`, `secrets.ErrInUse` | FailedPrecondition | 409 | `invalid_state`, `in_use` | no |
| `engine.ErrQuota` | ResourceExhausted | 429 | `quota_exceeded` | yes |
| rate limited | ResourceExhausted | 429 | `rate_limited` | yes |
| `engine.ErrSuspended` | PermissionDenied | 403 | `tenant_suspended` | no |
| the state store unreachable, no quorum, fenced | Unavailable | 503 | `unavailable` | yes |
| anything else | Internal | 500 | `internal` | yes |

The message of a 500 is "internal error" with the request ID; the detail is logged, never returned (T-06).

## 7. Configuration

`veho.yaml` gains `api.{listen, tls.cert_file, tls.key_file, tls.client_ca_file (C7b), max_watches}`, and C7b's `api.oidc` (a list of `{issuer, audience, groups_claim, ca_file}`) and `api.spiffe.{trust_domain, x509_bundle_file, jwt_bundle_file, audience}`. The API starts in `veho serve` after the engine.

## 8. Threat model of C7

| ID | Threat | Mitigation | Residual |
| :--- | :--- | :--- | :--- |
| T-03, INV-4 | A tenant reads or changes another's resources through the API | The tenant comes only from the credential; every handler runs in `store.InTenant`; another tenant's resource is `NotFound`; API-T4 tries each method across tenants | — |
| T-36 | A path that skips authentication, authorisation or audit (REST vs gRPC, streams) | One chain of interceptors; REST goes through the in-process gRPC client; API-T2 calls every method of every service without a credential and with a viewer's | — |
| C7-1 | A stolen API key | Shown once, stored hashed, expiry, immediate revocation, `last_used_at`; keys never logged (only their `id`) | A key in a careless client's hands until revoked |
| C7-2 | Brute force of keys | 256-bit secrets; constant-time compare; rate limit per source address on authentication failures (C7d) | — |
| C7-3 | Resource exhaustion: large bodies, many watches, slow clients | 1 MiB bodies, 4 MiB messages, deadlines, keepalives, `max_watches`, `ReadHeaderTimeout`, rate limits | A flood below the limits from many tenants |
| T-06 | Internal details or secrets in errors | Errors mapped and sanitised once (§6); 500s carry no detail; secret values never in a response (`SecretService` write-only) | — |
| C7-4 | A bootstrap key printed to a terminal history or a log | Printed once to standard output, never logged; the Operator's Guide says to store it in the secret manager at once | The operator's terminal |

## 9. Acceptance

| ID | Test |
| :--- | :--- |
| API-T1 | Over gRPC and REST, against a node on the lab: create a job, get it, list it, watch it until settled; cancel and retry; the same responses through both (FR-1, FR-14 to FR-16) |
| API-T2 | Every method of every service: without a credential `401`; with a revoked or expired key `401`; with a role too low `403` |
| API-T3 | Idempotency: the same key and request return the first job (`202`, same id); the same key with another request `409` |
| API-T4 | Isolation: tenant B's key gets `404` for every resource of tenant A |
| API-T5 | The error table of §6, each row, in both protocols, with `ErrorInfo` and `problem+json` |
| API-T6 | `?wait=` returns at the change or `304` at the end; `WatchJob` streams until settled and ends when the key is revoked (at the next re-authorisation) |
| API-T7 | Limits: a 2 MiB body `413`, unknown JSON fields `400`, more than `max_watches` streams `429` |
| API-T8 | `veho tenant create` and `veho apikey create` bootstrap a working key; the key is not in any log line |
| API-T9 | OIDC (a test issuer serving discovery and JWKS): a token entitled by subject, and one entitled by group, act with their entitlement's tenant and role; a wrong audience, another issuer, an expired token, a bad signature, a subject without an entitlement, an identity entitled in two tenants: `401` |
| API-T10 | X.509-SVID: a client certificate of the trust domain, entitled, works over gRPC and REST; a certificate of another trust domain or signed by another CA: `401`; a REST client sending the internal metadata header gets nothing from it |
| API-T11 | JWT-SVID: entitled and valid works; another audience, another trust domain, expired: `401` |
| API-T12 | A watch whose token expires, or whose entitlement is deleted, ends `Unauthenticated` at the next re-authorisation |
| API-T13 | `veho entitlement create` makes a working entitlement; one with both subject and group, or neither, is refused |
| API-T14 | Policies: create v1 and v2 of a name, get the latest and v1, list, validate a bad one (its problem, nothing stored); an invalid policy is refused at creation; delete refused while a running job uses it, accepted after, and the name hidden |
| API-T15 | Grants: an operator without a grant gets `404` for a policy and its submission; with `read` sees it; with `submit` submits by reference; with `manage` adds a version and sets grants; a group grant works for its OIDC members |
| API-T16 | Inline policies: `all` lets an operator submit one; `granted` refuses it without `submit_inline` and accepts it with; `none` refuses even an admin's |
| API-T17 | A job by reference stores the reference and the snapshot of the version it used; a later version does not change it |
| C7d | its own tests, specified in this document when it starts |

Benchmark: submission latency through the API on the lab with the `rtt40` WAN profile, against NFR-4 (p99 < 1 s). Fuzz: the REST decoding of a job request.

## 10. Amendments to other specifications

- `veho-api.md` §2: the source is `{endpoint, path}` (§5); retry of chosen transfers is not in v1; `422` is not returned at submission.
- `veho-tenancy.md` SEC-16: API keys are hashed with SHA-256 without a pepper (§4.2).
- `veho-tenancy.md` SEC-20: the tenant of an OIDC or SPIFFE identity comes from its entitlements, not from a claim (§4.4); the term *entitlement* replaces SEC-20's *identity binding*, which would clash with ligo's *binding*.
- `veho-api.md`: `AuditService` waits for backlog 32.
