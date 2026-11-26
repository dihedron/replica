# Tenancy, security and secrets

## Identities

The API accepts three kinds of credential, all over TLS 1.3:

| Credential | Checked | Tenant and role from |
| :---------------------- | :------------------------------------------------------ | :---------------------------------- |
| API key | `veho_<id>_<secret>`; veho stores the SHA-256 of the 256-bit secret, compares in constant time, honours the mandatory expiry, revocation and the disabling of its service account | `key:<service account>`: grants name the account, so its keys (at most 3 active) rotate under them |
| OIDC bearer token | Issuer, audience, signature, expiry and lifetime, against the issuers in `api.oidc` | An entitlement of the subject, or of one of its groups |
| SPIFFE X.509-SVID or JWT-SVID | The trust domain's bundle (`api.spiffe`); a JWT-SVID's audience | An entitlement of the SPIFFE ID |

An API key belongs to one tenant. The others name the tenant of each call, and an entitlement in that tenant must map them: the name only selects among the entitlements, which a tenant's administrators give, so that an entitlement in one tenant cannot change or block what an identity does in another. An identity has one name, with its issuer (`oidc:<issuer>#<subject>`, the SPIFFE ID, `key:<name>`), in grants, rate limits, jobs and the audit. An OIDC issuer that is down stops neither the node nor the other credentials: its own tokens are answered `unavailable` until it is reached.

People sign in from the CLI with the authorization code flow and PKCE on a loopback port, or the device flow, against the issuer that `AuthService.GetConfig` (public) names; the tokens are cached per API URL and refreshed under a file lock, so that two processes never present a rotated refresh token twice. `IdentityService.WhoAmI` answers who the caller is and its roles in every tenant, without naming one. An issuer with `step_up` makes the sensitive methods, those that change a tenant's configuration or access, require a token whose `acr` is one of its values and whose `auth_time` is recent (RFC 9470); ending an access never does, so that containment is never delayed.

## Authorisation

- **Roles** per tenant: `viewer` (reads), `operator` (also submits, cancels, retries), `admin` (also manages endpoints, secrets, policies, keys, entitlements and the tenant's settings). **Platform administrators** manage tenants and their quotas, and have no access to what is inside a tenant.
- **Policy grants**: a named policy can be used (`submit`), read (`read`) or changed (`manage`) by the principals and groups it is granted to; tenant administrators need no grant. A policy the caller may not see answers `not_found`. Whether inline policies may be submitted is a tenant setting: by everyone, by principals with a grant, or by nobody.
- **Quotas**: destinations per job, active jobs, and the bounds of a policy's retry back-off. **Rate limits** per tenant and per principal, and a block on client addresses that present many refused credentials.

## Isolation in the database

Every tenant's row carries its tenant, and PostgreSQL's row-level security admits only the rows of the tenant a transaction set: a session that set none sees nothing. The API sets the tenant from the authenticated principal; the executor sets it from the transfer it runs. The platform's own service accounts and keys, which have no tenant, are visible only in a transaction that set the platform's scope instead. The few operations that must cross tenants (housekeeping, authentication, the platform administrators' views) are reviewed `SECURITY DEFINER` functions with fixed purposes, not a role that bypasses the policies. A job, its source and its destinations are immutable once accepted (enforced by triggers), so a compromised API cannot redirect an accepted job. IDs are random.

Jobs name endpoints, never URLs: a tenant can only reach the storage locations it registered, which keeps a node from being used to read internal services.

## Secrets

Tenants store their storage credentials in veho as **secrets**, write-only: no API returns a value.

- Each secret is encrypted with its own **data key** (AES-256-GCM), whose additional data binds it to its tenant, name and version: a ciphertext moved to another tenant or name fails to decrypt.
- Data keys are wrapped by the **key-encryption key** (KEK), versioned (`active`, `retiring`, `retired`), stored in the database, wrapped.
- The KEK is wrapped by the **unwrap key**, which never enters the database: each node reads it at start from a systemd credential or a root-owned file.

A dump, backup or replica of the database therefore holds no usable secret. A node unwraps a secret only when a transfer opens its endpoint, passes it to the driver, and zeroes its copy. Credentials never enter DBOS's workflow inputs or outputs, logs, audit events or API answers.

The KEK is rotated with `veho kek rotate` and `veho kek rewrap`, and the unwrap key replaced with `veho kek rekey`, without downtime.

## Audit

Every change of state, every key operation and every repair by housekeeping emits an event: who, what, on which resource, with which outcome, after the commit. Events go to the node's system log, to be forwarded to the organisation's SIEM, which holds the record of authority, out of the reach of the database's roles. Each event carries the node, a random ID of the process and a sequence number, so a lost or removed event shows as a gap. This lightweight audit is accepted for now: events can be lost (a crash between commit and write) and carry no cryptographic tamper evidence. A tamper-evident audit log in the database, with a hash chain and signed checkpoints, is planned.

## The node

The systemd unit runs veho as its own user with no capabilities, a read-only system, private devices and temporary files, a system-call filter and no core dumps. Where AppArmor runs (Ubuntu, Debian), the package's profiles add mandatory access control: `veho` reads only its configuration, the CA roots and its plugins, and writes nothing but go-plugin's sockets and the system log; each plugin runs under `veho-plugin`, which denies `/etc/veho` and the systemd credentials, so a compromised plugin cannot read the unwrap key, the TLS key or the database DSNs (PK-1). The SELinux policy for Rocky Linux and AlmaLinux is planned. The node logs the SHA-256 of its configuration at start, so that a change shows.
