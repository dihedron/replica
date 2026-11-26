# veho: authentication

This file specifies backlog items 37 and 38: how people (Web UI, CLI) and applications authenticate to veho, what their identities may do, and how veho takes part in the ligo ecosystem. It is part of `veho_specification.md`; it extends `veho-api-server.md` §3 and §4, `veho-cli.md` §2 and `veho-tenancy.md` §6, which it amends (§12). The pages of the Web UI are backlog 19; the local users of the development mode are `veho-development-mode.md`.

Status: approved, 2026-11-25, with the open points of §15; step A1 implemented (§13), the rest not. The decisions of §2 were taken with the maintainer the same day.

## 1. Today

API keys, OIDC bearer tokens, X.509-SVIDs and JWT-SVIDs are implemented (`veho-api-server.md` §4). Reading the code for this design found these gaps, which it closes:

| # | Gap | Closed in |
| :--- | :--- | :--- |
| G-1 | **Cross-tenant lockout.** An identity entitled in two tenants is refused, and a tenant admin may entitle any issuer and subject. The admin of one tenant can therefore lock out the users of another, or a platform administrator who uses OIDC or SPIFFE. | §4 |
| G-2 | Grantees carry no issuer (`oidc:<sub>`, `group:<name>`): with two OIDC issuers, equal names share their grants within a tenant. | §3.1 |
| G-3 | A job does not record its creator, and job access is by role only: any `operator` reads, cancels and retries every job of its tenant. | §3.3 |
| G-4 | Idempotency keys are unique per tenant, not per caller: a caller that repeats another's key and request gets that caller's job. | §3.3 |
| G-5 | An API key cannot be rotated: each key creates a principal, whose name is unique, so a replacement needs a new name and new grants. A key may never expire. | §8.3 |
| G-6 | The principal of an OIDC or SPIFFE identity is its entitlement, so all the members of a group share one rate limit. | §3.1 |
| G-7 | A node does not start when an OIDC issuer is unreachable, although API keys, SVIDs and running transfers do not need it (NFR-3). | §5 |
| G-8 | T-01 promises a bounded token lifetime; only the expiry is checked. | §5 |
| G-9 | T-32 says that every authentication failure is audited; `admit` logs it but emits no audit event. | §10 |
| G-10 | Infrastructure as code cannot provision a tenant: a platform administrator creates it, but only `veho bootstrap` (the database) can give it its first admin. Entitlements have no unique key, so a repeated create makes a duplicate. Locations exist in SQL only, and tenants cannot be deleted. | §8.4, §8.5 |
| G-11 | `Submit` reads the tenant's state without a lock and `Retry` does not read it. Today both are covered by authentication, which refuses a suspended tenant, except for calls already admitted; a tenant that drains stays able to authenticate. | §8.5 |

## 2. Decisions (2026-11-25)

| # | Question | Decision |
| :--- | :--- | :--- |
| 1 | An identity entitled in several tenants | OIDC, SPIFFE and local callers always name the tenant, or `platform`. Entitlements still decide; the name selects among them. |
| 2 | Web UI session | Server-side: an opaque cookie, its hash and the envelope-encrypted OIDC tokens in PostgreSQL. The UI sends the access token through the API's interceptors. |
| 3 | CLI login | `veho auth login`: authorization code with PKCE on a loopback port; `--device` for hosts without a browser. |
| 4 | Applications | Workload identity first (SPIFFE, then OIDC). API keys for callers with neither: mandatory expiry, several active keys per service account. veho issues no tokens by exchange. |
| 5 | Job access of a ligo-bound workload | A new lowest role `member`, with no tenant-wide right; a new grant `view`; jobs record their creator. |
| 6 | SPIFFE matching | RE2, full match, as ligo's sources. |
| 7 | The right of ligo's plugin | A narrow role `integrator`. |
| 8 | MFA | At the identity provider, with an optional step-up per issuer (RFC 9470). |

## 3. Principals

### 3.1 Identity names

An identity has one canonical name, used in grants, job creators, rate limits and audit events:

| Kind | Name |
| :--- | :--- |
| API key | `key:<service account>` |
| OIDC | `oidc:<issuer>#<sub>` (an issuer URL has no fragment, so the first `#` separates) |
| SPIFFE | `spiffe://<trust domain>/<path>` |
| Local (development mode) | `local:<username>` |

A grantee is an identity name, `group:<issuer>#<group>`, or `entitlement:<id>`, which is every identity that the entitlement matches. `policy_grants.entitlement_id` references the entitlement and cascades on its deletion. Rate limits are keyed by tenant and identity name, not by entitlement.

### 3.2 Roles

| Role | May |
| :--- | :--- |
| `member` | What its grants give (§3.3), and nothing else: no endpoints, secrets, policies without a grant, or other jobs |
| `viewer` | Read everything in the tenant but secrets and administration, as today |
| `operator` | Also cancel and retry every job, and submit under the grants, as today |
| `admin` | Everything in the tenant, as today |
| `integrator` | Only `ManagedAccessService` (§9) and reading policies. Not ordered with the others, and never combined: an identity whose matching entitlements in a tenant include `integrator` and another role is refused in that tenant |
| `platform-admin` | Tenants and platform settings; no tenant |

Within the named tenant, the highest of the matching entitlements' roles applies, and the grants of all of them.

### 3.3 Jobs

- `jobs.created_by` is the creator's identity name, immutable like the other columns of `veho-store.md` §3.
- A `member` submits by reference under a policy on which it holds `submit` (inline only with `submit_inline` and the tenant's setting). It reads, watches, cancels and retries its **own** jobs: those with its identity name. A retry also needs `submit` on the job's policy at that time (SEC-21).
- The grant `view` on a policy lets a `member` read and watch every job submitted under any version of that policy. `submit` does not imply `view`, nor `view` `submit`; `manage` implies neither.
- A job that the caller may not see is `404` (INV-4), and `ListJobs` omits it. The filter is in the query, not applied after the page is read.
- The unique key of an idempotency key becomes `(tenant_id, created_by, idempotency_key)`.
- The check that `WatchJob` repeats (API-11) tests the visibility of this job, not only the role.

## 4. The tenant of a call

- `X-Veho-Tenant` (gRPC metadata `x-veho-tenant`) names the tenant, or `platform` for the `platform-admin` role. `platform` is a reserved tenant name.
- It is **required** for OIDC, SPIFFE and local identities, also with a single entitlement. If it were optional, a new entitlement elsewhere would again break a caller that omits it. Absent: `400 tenant_required`.
- An API key belongs to its service account's tenant. The header is optional, and one that names another tenant is refused.
- A tenant that does not exist, is suspended, or in which the identity has no entitlement: `403 not_entitled`, the same answer for all three, after the credential has been verified. It counts as a failure for the address rate limit.
- `veho.entitlements_for` takes the tenant, so other tenants' entitlements can neither change the result nor slow it down.
- `IdentityService.WhoAmI` returns the identity, its tenants and roles. It is the one method that needs no tenant, and is refused without one to SPIFFE identities, whose patterns it would have to match in every tenant.

## 5. OIDC tokens

- **Access tokens only, as JWTs.** `aud` must contain the issuer's configured `audience`, which must differ from every client ID in the configuration (checked at load), so that an ID token is refused as a bearer token. Opaque tokens are not supported. Only asymmetric algorithms.
- `iat` is required, `exp − iat` at most `max_token_lifetime` [1h], a clock skew of 1 minute accepted.
- **An issuer that is down does not stop a node.** Discovery is tried at start and retried in the background with back-off. Until it succeeds, the tokens of that issuer get `503 unavailable`; everything else works.
- **Step-up.** With `step_up: {acr_values, max_age}` on an issuer, the methods marked *sensitive* need a token whose `acr` is in the list and whose `auth_time` is at most `max_age` old. Otherwise the answer is `401 step_up_required` with `WWW-Authenticate: Bearer error="insufficient_user_authentication"` and the values (RFC 9470); the UI and the CLI then sign in again with them. It applies to every token of that issuer, client credentials included: automation that administers veho uses SPIFFE or an API key, or an issuer without step-up.
- Sensitive: creating or changing secrets, endpoints, webhooks, entitlements, API keys, grants, settings, tenants, local users. Deleting a tenant is sensitive too. Revoking, disabling and deleting an access are **not**, so that containment is never delayed.

## 6. Web UI sessions

### 6.1 Login

1. A `/ui/*` page without a session redirects to `/ui/login`; with several issuers the person picks one.
2. veho creates `state`, `nonce` and a PKCE verifier (S256) and redirects to the issuer. They travel in the cookie `__Host-veho_login` (`Secure; HttpOnly; SameSite=Lax; Path=/`, 10 minutes), sealed with AES-256-GCM under a cluster key stored wrapped by the KEK. Nothing is written to the database before a login succeeds. The cookie is `Lax` because the callback is a cross-site navigation, on which a `Strict` cookie is not sent.
3. The issuer redirects to `/ui/callback/<issuer name>`. One path per issuer, and `iss` (RFC 9207) when present, prevent a mix-up of issuers.
4. veho checks `state`, redeems the code with the verifier and the client secret, and validates the ID token (with `nonce`) and the access token (§5); both must have the same `sub`.
5. An identity without any entitlement gets no session, and a page that says so.
6. veho creates the session, sets `__Host-veho_session` (`Secure; HttpOnly; SameSite=Strict; Path=/`), clears the login cookie and answers `200` with a page that navigates to the destination. A redirect would still belong to the cross-site navigation, and the new cookie would be missing. The destination is a path under `/ui/` (no `//`, no scheme), else the dashboard.
7. The person picks the tenant, which is stored in the session and shown on every page. It is preselected only when it is the only one, and never changes by itself.

### 6.2 The session

`ui_sessions(id_sha256, kind, issuer, subject, sid, tokens, wrapped_dek, kek_version, csrf_sha256, tenant, created_at, last_seen_at, refreshed_at, revoked_at)`, reached only through `SECURITY DEFINER` functions, since a session precedes its tenant. The tokens are envelope-encrypted (SEC-24), the associated data binding them to the session; housekeeping re-wraps and prunes the rows.

- Idle 30 minutes, absolute 8 hours (`ui.session`).
- The UI's handlers call the API in process, with the access token as the bearer and the session's tenant, so authorisation, rate limits and audit are the API's (SEC-22, SEC-23).
- A token near its expiry is refreshed under a row lock (5 s), since nodes share sessions and refresh tokens may rotate. A **refused** refresh (`invalid_grant`) ends the session. An **unreachable** issuer does not: the pages answer "identity provider unavailable" once the access token has expired, until the issuer answers or the session times out.
- Cookies are never accepted on `/v1` or gRPC, nor bearer credentials on `/ui`.
- Each non-`GET` request carries `X-CSRF-Token`, compared in constant time, and `Sec-Fetch-Site: same-origin` or an `Origin` equal to `ui.external_origin`. Both are checked before the session is touched (as ligo §7.3).
- `POST /ui/logout` deletes the row, revokes the refresh token (RFC 7009) and goes to the issuer's end-session endpoint. `/ui/backchannel-logout/<issuer name>` accepts OIDC back-channel logout by `sid` or `sub`.
- People list and end their own sessions; a platform admin, anyone's.

## 7. CLI login

- `veho auth login [--issuer <name>] [--device]`, `veho auth logout`, `veho auth status`. A public client with an ID of its own.
- The default flow listens on `127.0.0.1` at a random port and accepts one request with the right `state`. `--device` is the device flow (RFC 8628), which prints the issuer's host and a warning never to enter a code received from somebody else.
- `AuthService.GetConfig` (public) gives the issuers, the CLI client IDs and the scopes. The CLI prints the issuer's host before it opens the browser.
- Tokens are cached per API URL in `$XDG_CONFIG_HOME/veho/credentials.json` (0600, its directory 0700, refused otherwise as C8-3), written by rename, and refreshed under a file lock. They are sent to that API URL only.
- `VEHO_TOKEN`, `--token-file` and a client certificate take precedence over the cache. `--tenant` (`VEHO_TENANT`) names the tenant, and `auth login --tenant` stores a default.
- On `step_up_required` in a terminal, the CLI signs in again with the required values and repeats the call once. Elsewhere it fails with the code.

## 8. Applications

### 8.1 SPIFFE

`api.spiffe.trust_domains` is a list of `{name, x509_bundle_file, jwt_bundle_file}`: the node's own and the federated ones. An SVID is verified against the bundle of its own trust domain only.

An entitlement of kind `spiffe` has either an exact `subject` or a `subject_pattern`:

- RE2, matched in Go against the whole SPIFFE ID, at most 512 bytes (ligo's limit). PostgreSQL's regular expressions are another dialect and are not used.
- `issuer`, the trust domain, is compared before the pattern, so a pattern cannot reach another domain.
- Only with the role `member`, so that a pattern that is too wide cannot make administrators. An exact `subject` may have any role (§8.4).
- At most 1000 patterns per tenant. Compiled patterns are cached.

### 8.2 OIDC

Client credentials and Kubernetes projected service-account tokens (`sub` = `system:serviceaccount:<namespace>:<name>`) are entries of `api.oidc` like any other. A workload's entitlement is by subject.

### 8.3 Service accounts and API keys

- A service account (today's `principals` row) has a name, a tenant, a role and a state. Grants name it, so they survive the rotation of its keys. `veho serviceaccount create|list|show|disable|enable|delete`; `veho apikey create --service-account <name>`.
- At most 3 active keys each. The expiry is mandatory: 90 days by default and at most (`api.keys.{default_lifetime, max_lifetime}`). Bootstrap keys too; a new one is made from the database.
- Disabling a service account refuses all its keys at once.

### 8.4 Automation that administers veho

An application may hold any role, `platform-admin` and `admin` included, by an entitlement with its **exact** SPIFFE ID (or OIDC subject). Terraform (backlog 35) and Ansible (backlog 36) are the first cases:

| Level | Entitlement | Provisions |
| :--- | :--- | :--- |
| Platform | `platform-admin`, made once with `veho bootstrap entitlement` | Tenants, their quotas and first admins, locations |
| Tenant | `admin` in that tenant | Endpoints, secrets, policies, grants, entitlements, service accounts, webhooks, settings |
| Workload | `member` | Nothing: it submits jobs under its grants |

- **First admin.** `CreateTenant` takes `initial_admins`: 1 to 10 exact identities or groups, entitled as `admin` in the same transaction, and audited. It is accepted at creation only: a platform administrator cannot later enter a tenant that exists (SEC-4), short of the break-glass of SEC-4 or `veho bootstrap` with the database.
- One identity may be `platform-admin` and `admin` of tenants, since each call names its tenant (§4). Separate IDs per level, and per tenant, are recommended: a stolen pipeline identity then has one level only.
- The pipeline's SPIFFE ID must be stable across runs, since a pattern cannot have these roles.
- **What the API needs for plan, apply and import**: a unique key on an entitlement (tenant, kind, issuer, subject or pattern or group: one role each, changed by an update) with `Get`; `LocationService` (create, get, list, delete if unused; `platform-admin`); `DeleteTenant` (§8.5). Secret values stay write-only: their version and time detect drift.
- Step-up (§5) does not concern SPIFFE identities.

### 8.5 Deleting a tenant

Decided with the maintainer on 2026-11-25: a soft delete that drains and cannot be undone. `DeleteTenant` (`platform-admin`) marks the tenant `deleting`; its accepted jobs run to their end and no new job is accepted. Once it is drained, the tenant is removed for good. A grace period with a restore was considered and not adopted: Terraform deletes a tenant's resources one by one before the tenant, so it would protect only a part of them.

- **Asynchronous and idempotent.** The answer is `202` with the tenant: its state, its unsettled jobs and its pending webhook deliveries. A repeated call answers the same. After the removal, `GetTenant` is `404`. A suspended tenant can be deleted too. Its name is taken until the removal: creating it again is `409 tenant_deleting`. `UpdateTenant` cannot change it back (`409`).
- **`cancel_jobs: true`** also cancels the jobs at once, and may be sent later by a second call. Without it, the drain lasts as long as the longest job: at most until the jobs' deadlines, since a retry, which renews a deadline, is refused.
- **While `deleting`**, callers still authenticate, unless the tenant was suspended when it was marked: deleting it does not give access back. They may read, watch, cancel jobs, revoke keys and delete entitlements, grants and managed access, and replace the value of an **existing** secret, so that a job whose credential expires can still end. Everything else that changes the tenant is `409 tenant_deleting`: jobs, retries, endpoints, policies, new secrets, keys, entitlements, grants, webhooks, settings.
- **No race with a submission.** `Submit` and `Retry` take a shared advisory transaction lock on the tenant before they read its state; `DeleteTenant` takes it exclusively. A job is therefore either accepted before the mark, and drained, or refused.
- **Drained** means all of: every job has `settled_at` (not only a terminal state: after the rule is satisfied, transfers still run); every DBOS workflow of its jobs has ended (a job past its deadline is settled while a step may be in flight, and an upload is aborted with the tenant's credentials); no webhook delivery is pending (delivered or abandoned, `veho-webhooks.md`). Since nothing new is accepted, a drained tenant stays drained.
- **Removal**, by housekeeping under its lock, in the next pass [30 s]: first entitlements, grants, keys and service accounts, so that no caller is admitted any more; then the DBOS workflows, the jobs in batches, webhooks, policies, endpoints and secrets, and the tenant's row last. Each batch is its own transaction and can be repeated: a node that dies, or a call that was already admitted and wrote a row, leaves the rest to the next pass. The work is in `SECURITY DEFINER` functions that refuse a tenant that is not `deleting` and drained.
- **Destination data is never touched** (`veho-tenancy.md` §7). The job history and the secrets go with the tenant, whatever `jobs_retention` says; the audit log keeps the events `TENANT_DELETE`, `SYSTEM_TENANT_DRAINED` and `SYSTEM_TENANT_REMOVED`.
- **ligo**: unbind first. While `deleting`, the plugin can still delete its managed access; after the removal its entitlement is gone too, and its cleanup gets `403 not_entitled`. `DeleteTenant` answers with the number of managed accesses that remain.
- **Terraform** destroys a tenant's resources before the tenant. An endpoint that a retained job references refuses its deletion (`409 in_use`), so a destroy of a tenant that has been used stops before it reaches the tenant. The provider (backlog 35) waits for `404`, with a timeout. The guide recommends `prevent_destroy` on tenants, which is the protection against an accidental destroy, and a timeout or `cancel_jobs` that suits the longest job.
- `DeleteTenant` is *sensitive* for step-up (§5).

## 9. ligo

veho knows nothing of ligo. A veho `PlatformProvider` plugin in ligo (its own specification, there) calls veho's API.

| ligo | veho |
| :--- | :--- |
| Platform | A veho deployment |
| Jurisdiction | A tenant (the map is in the plugin's configuration) |
| Target | One named policy of the tenant |
| Access level `submit` | Permission `submit`: submit, and handle one's own jobs |
| Access level `submit+view` | `submit` and `view` |
| Access level `view` | `view` |
| Source: trust domain and SVID regex | The managed access's trust domain and pattern |
| Binding ID | The managed access's `external_ref` |
| Annotations | Endpoint and policy names, for the workload to use |

`ManagedAccessService` (roles `integrator` and `admin`):

- `PutManagedAccess(external_ref, {trust_domain, pattern, policy, permissions})` creates or replaces, in one transaction, an entitlement of role `member` and its grants. Permissions: `submit` and `view` only. The same call again changes nothing. This is `EnsureTarget`.
- `DeleteManagedAccess(external_ref)` removes both; deleting a missing one succeeds. This is `CleanupTarget`.
- `GetManagedAccess`, `ListManagedAccess`: for an `integrator`, its own only.
- **Ownership.** The creator's identity name is the owner, and `external_ref` is unique per owner. Nobody else changes it, and the ordinary entitlement and grant calls refuse it (`409 managed`); `SetGrants` replaces only the unmanaged grants of a policy.
- **Containment.** A tenant admin may `SuspendManagedAccess` and resume it: suspended, it matches nobody, and a `Put` does not lift it, so reconciliation cannot undo it. An admin may also delete it; the owner's next `Put` makes it again.
- **Revocation** applies to the next call, and to a running `WatchJob` within `Reauthorise` [60 s]. Jobs already accepted run to their end.
- A policy with managed grants cannot be deleted (`409 in_use`).

## 10. Audit, limits, metrics

- Audited: logins and logouts, session ends with their reason, refused credentials (per address and kind, summed per minute so that a flood does not fill the log), `not_entitled`, step-up refusals, service accounts, keys and managed access.
- Never in a log, an error or an event: tokens, codes, cookies, verifiers, client secrets, passwords.
- Metrics: `veho_auth_total{kind,result}`, `veho_ui_sessions`, `veho_oidc_issuer_up{issuer}`. No label carries a subject.

## 11. Threat model

| ID | Threat | Mitigation | Residual |
| :--- | :--- | :--- | :--- |
| AU-1 | A tenant admin locks out or redirects an identity of another tenant by entitling it | The tenant is always named, and the lookup is limited to it (§4) | A person may see an unexpected tenant in the switcher and pick it: it is shown on every page and never preselected among several |
| AU-2 | Confusion of token types or audiences (an ID token, a token of another service) | API audience distinct from all client IDs; asymmetric algorithms; `sub` equal in both tokens (§5, §6.1) | An issuer that puts the API audience in other services' tokens |
| AU-3 | Login CSRF, code injection, issuer mix-up | `state` and `nonce` in a sealed cookie, PKCE, a callback per issuer, `iss` | — |
| AU-4 | Open redirect after login | Only a path under `/ui/` | — |
| AU-5 | Theft of a session cookie (T-34) | `__Host-`, `HttpOnly`, `Strict`, strict CSP, timeouts, listing and ending sessions, step-up | Malware on the person's host |
| AU-6 | A database dump gives tokens or sessions (T-28) | Only hashes of cookies; tokens envelope-encrypted and bound to their session | — |
| AU-7 | CSRF on the UI | Strict cookie, CSRF token, origin checks | — |
| AU-8 | Device-code phishing | Not the default; a warning; the issuer may disable the grant | A person who enters an attacker's code |
| AU-9 | A hostile API URL makes the CLI sign in elsewhere | TLS verified; the issuer's host is printed; tokens are cached per API URL | A person who ignores the host |
| AU-10 | A pattern that is too wide, or expensive | RE2 is linear; 512 bytes; 1000 per tenant; role `member` only; trust domain compared first | `.*` still gives a whole trust domain the grants: ligo's regex policy and the audit |
| AU-11 | A stolen plugin identity | `integrator`: `member`, `submit` and `view` only; own objects only; suspend; tenants mapped one by one | It can grant any SVID of the configured trust domains `submit` on any policy of those tenants, until it is suspended or its entitlement is deleted |
| AU-12 | A federated trust domain asserts another's IDs | Each SVID is verified against its own domain's bundle | — |
| AU-13 | An identity provider that is down, or slow | Background discovery; cached keys; bounded refresh; other credentials unaffected | No new UI logins meanwhile: a CLI with an API key |
| AU-14 | A long-lived key leaks (C7-1) | Mandatory expiry, rotation without downtime, disabling the account | Until it is revoked. An administrator's API key also bypasses step-up: prefer OIDC for people |
| AU-15 | Reading or repeating another caller's job by its idempotency key | Keys are per creator | — |
| AU-16 | A stolen administration pipeline identity | Exact IDs only, short-lived SVIDs, an ID per level and tenant, `initial_admins` at creation only, audit, deleting its entitlement | Until it is deleted: the whole tenant, or the creation of tenants |
| AU-17 | A tenant is deleted by mistake or by a stolen platform identity | `platform-admin` only, step-up, audit, jobs drained rather than cut; the removal re-checks its conditions; `prevent_destroy` in Terraform | It cannot be undone: a tenant without jobs is gone at the next pass, with its history and secrets, short of a restore of the database; old backups still hold its wrapped secrets |

## 12. Amendments to other specifications

Applied on 2026-11-25, at the approval; the amended texts say that the implementation is to come.

- `veho-tenancy.md`: SEC-2 (the tenant is one that the caller names among those its entitlements give), SEC-4 (`member`, `integrator`), SEC-16 (expiry), SEC-21 (`view`), SEC-22 (the login cookie is `Lax`; the session's tokens), §7 (deletion drains the jobs instead of cancelling them, and removes the tenant once it is drained instead of after the retention).
- `veho-api-server.md`: §3 (the tenant step), §4 (principals, tables, ambiguous identities, fail-fast issuers), §5 (idempotency, own jobs), §5.1 (grantees, `view`), §6 (`tenant_required`, `not_entitled`, `step_up_required`, `managed`, `tenant_deleting`).
- `veho-cli.md`: §1 and §2 (`auth`, `serviceaccount`, `--tenant`, the cache).
- `veho-threat-model.md`: T-01 (the tenant no longer comes from a claim), T-32, T-34; AU-1 to AU-17.
- The schema is edited in place in `migrations/001_core.sql` (`veho-store.md` §5).

## 13. Steps

| Step | Scope | Needs |
| :--- | :--- | :--- |
| **A1** | Security fixes of what exists: tenant naming (G-1), identity names (G-2, G-6), job creators and idempotency (G-3, G-4), issuer start-up and token lifetime (G-7, G-8), audit (G-9). Implemented 2026-11-25 with AU-T1, AU-T2, AU-T3, AU-T5, AU-T6, AU-T7 (but the client IDs, A4) and the audit of AU-T16; G-1 was first proven by its test | — |
| **A2** | Service accounts and keys (G-5) | A1 |
| **A3** | `member`, `view`, patterns, several trust domains, `integrator`, `ManagedAccessService` | A1 |
| **A3b** | Administration by automation: `initial_admins`, entitlement keys, `LocationService`, `DeleteTenant` (G-10, G-11); before backlog 35 | A1 |
| **A4** | `GetConfig`, `WhoAmI`, CLI login, step-up | A1 |
| **A5** | UI login and sessions | A4; with backlog 19 |

## 14. Acceptance

| ID | Test |
| :--- | :--- |
| AU-T1 | Tenant B entitles a subject of tenant A, and a platform admin's: their calls that name A and `platform` still succeed (G-1); without a tenant `400`; an unknown tenant, a suspended one and an unentitled one give the same `403` |
| AU-T2 | Two issuers with the same `sub` and group: a grant to one gives nothing to the other |
| AU-T3 | Two members of one group have separate rate limits |
| AU-T4 | A `member` with `submit`: submits, sees, cancels and retries its own job; another's job is `404` and not listed, over gRPC and REST, pages included; after `submit` is revoked a retry is refused; with `view` it reads the policy's jobs and cannot cancel them |
| AU-T5 | The same idempotency key and request by two callers: two jobs |
| AU-T6 | A node starts with an unreachable issuer: API keys work, the issuer's tokens get `503`, and work once it answers |
| AU-T7 | Refused: an ID token, an HMAC or `none` signature, no `iat`, a lifetime over the maximum; a configuration whose audience is a client ID |
| AU-T8 | Step-up: a sensitive method without and with the right `acr` and `auth_time`; a revocation needs none |
| AU-T9 | Keys: rotation with two active keys keeps the grants; a fourth key, and an expiry over the maximum, are refused; disabling the account refuses all keys at once |
| AU-T10 | Patterns: full match (a prefix and a suffix around the ID do not match), trust domain, 512 bytes, a role other than `member` refused, the 1001st refused |
| AU-T11 | Two trust domains: an SVID of each works; one signed by the other's CA is refused |
| AU-T12 | Managed access: `Put` twice changes nothing; `Delete` twice succeeds; another integrator and an admin cannot change it; `SetGrants` keeps it; suspended, it survives a `Put`; deleting it ends a `WatchJob` within `Reauthorise` |
| AU-T13 | An `integrator` is refused by every other method (as API-T2), and an identity with it and another role is refused |
| AU-T14 | UI login against a test issuer: success; wrong `state`, `nonce`, `iss`, another issuer's callback, a replayed login cookie, an expired one; no entitlement; a destination outside `/ui/` |
| AU-T15 | Session: idle and absolute timeouts, refresh by two nodes at once, refused refresh, unreachable issuer, logout, back-channel logout; a cookie on `/v1` and a bearer on `/ui` refused; CSRF refused without touching the session |
| AU-T16 | A canary token, cookie, code and client secret appear in no log, audit event, error or clear-text database column (INV-5) |
| AU-T17 | CLI: loopback and device logins, refresh by two processes, a cache with mode 0644 refused, a token not sent to another API URL |
| AU-T18 | Lab: a ligo binding lets a workload submit, its revocation refuses the next call and ends its watch (with ligo's plugin) |
| AU-T19 | With SVIDs only: a `platform-admin` creates a location and a tenant with an `initial_admins` entry; that identity creates a secret, an endpoint, a policy, and a `member` entitlement with its grant; the member's job succeeds. The platform identity is refused inside the tenant; an identity with both roles works in each by naming it; a second create of the same entitlement is `409` |
| AU-T20 | Deletion: a running job ends with its copies intact and the tenant is then removed; a submission and a retry are `409`; an existing secret can be replaced, a new one not; with `cancel_jobs` the uploads are aborted before the secrets go; no row of the tenant is left in any table, DBOS's included, and the destination objects are |
| AU-T21 | Deletion under stress: submissions from many callers while the tenant is marked, 1000 times: each is refused or drained, none lost; the node is killed at each step of the removal and another finishes it; the name is free only afterwards |

Fuzz: the sealed login cookie, the return path, the tenant header, the identity-name parser. Benchmark: authentication with 1000 patterns in the tenant, against the submission latency of NFR-4. Lab (an overnight run, AGENTS.md rule 14): sessions and tokens while the issuer is stopped, slowed and restarted under load.

## 15. Open

- **Deleting the endpoints of a tenant that has been used.** An endpoint that a retained job references cannot be deleted (`409 in_use`, 90 days by default), and Terraform deletes endpoints before their tenant, so even an intended `terraform destroy` stops there (§8.5). To be analysed with the maintainer when `DeleteTenant` is implemented (A3b), and before backlog 35.
- Which identity providers must be supported, and whether they give JWT access tokens with a custom audience, refresh tokens, `acr` and `auth_time`. The design depends on the first two; step-up on the last two.
