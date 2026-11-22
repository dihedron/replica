# The threat model

The threats veho is designed against, how it meets them in this version, and what remains. Trust boundaries: the clients and the API; the nodes and the state store; the nodes and the storage systems; the regions; the plugins; the operators.

::: landscape

| Threat | Mitigation in this version |
| :---------------------------------------------- | :---------------------------------------------------------------------------------- |
| A stolen or forged credential acts as a tenant | OIDC issuer, audience, signature and expiry checked; SPIFFE bundles; API keys hashed, expiring, revocable; credentials never on the command line; a block on addresses that present many refused credentials |
| A tenant reads or acts on another tenant's jobs or endpoints | Row-level security on every tenant row, the tenant taken from the credential; random IDs; another tenant's object answers `not_found` |
| The executor or housekeeping, which touch every tenant, act on the wrong one | Each transaction sets the tenant of the transfer it runs; cross-tenant work only in reviewed functions with fixed purposes |
| Server-side request forgery: a job makes a node read an internal service | Jobs name registered endpoints, never URLs; endpoints are created by tenant administrators |
| A job uses another tenant's endpoint or credentials (confused deputy) | Endpoints resolved within the job's tenant; secrets bound to tenant and name by their encryption |
| Credentials leak through workflow records, logs, errors or answers | Credentials fetched when an endpoint is opened, never in DBOS inputs or outputs; redacted from logs and audit; the secret API is write-only |
| Eavesdropping or tampering in transit | TLS 1.3 to the API and to PostgreSQL, verified (`verify-full` required); TLS to the storage systems at the operator's choice |
| A corrupted copy reported as done | Per-part SHA-256 on the read path, the digest compared across destinations; the S3 service checks each part's MD5 |
| The source changes during the copy | Every read is conditional on the version first seen: `SourceChanged` |
| A partial object visible under its final name | Staged uploads, atomic idempotent commit, uploads aborted on cancel |
| One tenant exhausts the service | Quotas (destinations, active jobs, retry bounds); concurrency per tenant per node; rate limits per tenant and principal |
| A hostile storage endpoint stalls workers | Deadlines on storage calls; leases and adoption; per-node memory budget for parts |
| The state store loses its quorum | Consistency chosen: submissions refused, nothing forked |
| Split brain in PostgreSQL | Patroni with a three-member etcd; only a synchronous standby can be promoted |
| A zombie executor writes after a takeover | Fences on every state write; idempotent part writes |
| A poison job crashes every executor | Adoptions counted per transfer; failed after 20 |
| Someone with database write access alters an accepted job | Jobs' identity, source and policy, and transfers' destinations, immutable by triggers; the application roles cannot alter them |
| A user denies an action | Audit events for every change, forwarded to the SIEM; gaps visible by sequence numbers (not tamper-evident: planned) |
| A malicious or compromised plugin | Process plugins pinned by SHA-256, out of the core's memory; only reviewed drivers compiled in |
| A poisoned release or dependency | Releases signed (checksums, `.deb`, `.rpm`); dependencies checked with govulncheck; secret scans in CI |
| The configuration redirects a node | Configuration files root-owned; their SHA-256 logged at start; DSNs only from the environment file |
| Theft of the key that protects the secrets | Unwrap key outside the database, in a systemd credential; KEK rotation and unwrap-key replacement without downtime; no core dumps |
| Abuse of the API or its streams | Size limits, strict decoding, pagination, rate limits, a cap on open streams, no gRPC reflection, keepalive enforcement |

:::

## Residual risks

- veho replicates what it is given: an object already corrupt or malicious at its source is copied faithfully.
- A compromise of an application database role can alter mutable state across tenants within that role's privileges; accepted jobs stay immutable, and the audit trail lives outside the database.
- A tenant's storage credential is as powerful as the tenant made it.
- A zombie can finish one part after a takeover: bandwidth spent twice, the object unaffected.
- A partitioned region cannot accept jobs, by design.
- A node holds the unwrapped KEK in memory: a compromised node can read every tenant's secrets. Vault, HSM and Shamir-unseal strategies, which remove or narrow this, are planned.
- Someone who can write the state store can put back an older, authentic version of a secret.
- The audit is lightweight: events can be lost on a node crash and are not tamper-evident until the full audit log exists.
- Releases are as trustworthy as the custody of the packaging key.
