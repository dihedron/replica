# veho: tenancy and security

This file specifies multitenancy and security: isolation, secrets, quotas and URL safety. It is part of `veho_specification.md`. The threat model is `veho-threat-model.md`.

Status: draft, 2026-11-21, for review.

## 1. Tenant model

- **SEC-1** Every persistent record and every workflow carries `tenant_id`.
  Workflow IDs and idempotency keys are namespaced by tenant.
- **SEC-2** Tenant is derived **only** from the authenticated principal.
- **SEC-3** A tenant owns: endpoints (with credential references), named
  policies, jobs/transfers, quotas, API keys / OIDC mapping.
- **SEC-4** Roles per tenant: `admin` (endpoints, policies, keys, quotas
  within limits), `operator` (jobs: submit/cancel/retry/read), `viewer`
  (read-only). A role is the default; the right to submit a job under a given
  policy is granted per policy (SEC-21). A global `platform-admin` role manages tenants and global
  limits, and cannot read tenant data without an explicit audited
  break-glass action.

## 2. Data isolation

- **SEC-5** PostgreSQL **row-level security** on all tenant tables:
  `USING (tenant_id = current_setting('veho.tenant_id')::uuid)`.
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

- **SEC-9** **Secrets live in PostgreSQL, envelope-encrypted** (OI-16). The
  credentials needed to authenticate to transfer sources and destinations are
  stored as ciphertext in the state store, in a `secrets` table under row-level
  security. An endpoint holds only a reference (`credential_ref`:
  `db://<name>`). Plaintext secrets MUST NOT be stored in `endpoints.config`
  and MUST NOT be returned by the API: the secret API is write-only (a secret
  is created or replaced, never read back; only its name, version and times are
  shown). The core uses secrets through a small `secrets` interface, so that
  another backend (Vault, backlog 26) can be added without touching it.
- **SEC-10** Credentials MUST be fetched just-in-time and kept in memory only
  for the duration of an attempt, then zeroed; rotation is picked up on the
  next attempt. An attempt that cannot unwrap a secret fails as `Transient`
  and is retried (`veho-job-lifecycle.md` §7).
- **SEC-11** Preferred auth for cloud sources/destinations is workload
  identity / role assumption (e.g. STS `AssumeRole` with external ID per
  tenant) over static keys, so that fewer static secrets exist at all.
- **SEC-24** Key hierarchy. Each secret is encrypted with AES-256-GCM under its
  own random **data key** (DEK); the DEK is wrapped by a **key-encryption key**
  (KEK). The associated data binds the ciphertext to its tenant, secret name and
  version, and the wrapped DEK to the same and to the KEK version, so a blob
  moved to another tenant or name does not decrypt, and a KEK rotation
  re-wraps the DEKs without re-encrypting the values (`veho-secrets.md` §1).
  KEKs are versioned.
- **SEC-25** The KEK is stored in the database **only wrapped** by an
  *unwrap key* that is **never in the database** (a database dump or a backup
  must give an attacker nothing: T-28). The wrapped KEKs are in a `kek` table
  (version, wrapped value, state `active` or `retired`, times).
- **SEC-26** Unwrap at start, automatically (the v1 default). The unwrap key is
  supplied to the node by the platform through a configurable strategy: a
  root-owned file of mode 0400 (also a mounted Kubernetes Secret), a systemd
  encrypted credential (`LoadCredentialEncrypted`, optionally bound to the
  node's TPM), Barbican, or an external key service, all behind the same
  interface (`veho-architecture.md` §7.2).
  On OpenStack VMs the protection depends on the platform (`veho-architecture.md` §7): without a vTPM, a copy of the VM's disk carries both the credential and its host key, so encrypted volumes and a separation of volumes are required. The node fails fast at start if it cannot unwrap the active KEK. Restarts and
  automatic adoption of work need no human (NFR-3, G2). An alternative
  *unseal* strategy with Shamir shares and peer hand-off is planned
  (backlog 25); it plugs into the same interface.
- **SEC-27** Rotation. A new KEK version is created by an administrator; data
  keys are re-wrapped in the background by the housekeeping loop
  (`veho-audit-housekeeping.md` §3) and the old KEK is retired after the
  last one; a change of the unwrap key re-wraps the KEK rows only. Both are
  audited.
- **SEC-28** The signing key of the audit checkpoints is reached through a
  `Signer` interface with three strategies: `local` (a key pair whose private
  key is stored wrapped by the KEK, held by the sealer nodes), `vault-transit`
  (a non-exportable key, as in ligo) and `hsm`. Public keys (every version) are
  published in the database and printed at installation so that an auditor takes
  them from outside the database. With a remote signer a compromised node cannot
  steal the key, only ask for signatures while it is allowed to.
- **SEC-29** KEK providers. The core reaches the KEK through one interface with
  two kinds of provider, chosen by configuration (`veho-architecture.md`
  §7.2.1): a **loaded** provider hands the node the KEK at start (file,
  systemd credential, Barbican, Vault KV, Shamir); a **remote** provider keeps
  the KEK in Vault Transit or in an HSM and unwraps data keys on request,
  per attempt, so the node never holds it. Both give the same ciphertext
  format; a deployment can move from one to the other by re-wrapping the data
  keys (SEC-27). A remote provider is local to the region, with the same key
  in each regional cluster or HSM.

## 4. Quotas and noisy-neighbour control

Per tenant (enforced in veho-job-lifecycle.md §8): max active jobs, max bytes in flight, max
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
- **SEC-23** The gRPC interface authenticates every call and every stream (API-11); authorization, quotas, rate limits and audit are the same interceptors for gRPC and for the REST gateway, so there is no path that skips them (T-36).
- **SEC-20** Identity mechanisms accepted by the API, each mapped to a tenant
  and a role by an identity binding configured by a tenant admin (never by the
  caller): **OIDC** bearer JWT (issuer, audience and signature verified; tenant
  from a configured claim); **API key** (SEC-16); **SPIFFE** identities, as an
  X.509-SVID presented over mTLS or as a JWT-SVID, verified against the
  trust bundle of the configured trust domain; the SPIFFE ID is bound to a
  tenant and role. The same mechanisms authenticate node-to-node calls.
- **SEC-21** Policy permissions. A named policy carries grants: a principal or
  a group has `submit` (trigger jobs under it), `read` or `manage` (change it
  and its grants). Submitting by reference requires `submit` on that policy;
  submitting an ad-hoc (inline) policy requires the tenant setting
  `allow_inline_policies` and the permission `submit_inline`, so a tenant can
  force every job through governed policies. Authorization is checked on every
  request and on retries; an unauthorized policy is indistinguishable from a
  missing one (INV-4).
- **SEC-22** Web UI. The UI is a client of the REST API only, served from the
  `veho` binary, with no privilege of its own. Login by OIDC authorization
  code with PKCE; session cookie `HttpOnly`, `Secure`, `SameSite=Strict`;
  CSRF protection on every state-changing call; strict Content-Security-Policy
  (no inline script), `frame-ancestors 'none'`, no third-party assets; it never
  displays a secret or a credential value.
- **SEC-17** Audit log, specified in `veho-audit-housekeeping.md` (append-only table, hash chain, signed checkpoints, log stream): tenant, principal,
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
