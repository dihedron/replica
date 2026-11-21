# veho: secrets (chunk C3)

**Status:** approved and implemented 2026-11-21 (`internal/secrets`, `migrations/002_secrets.sql`, `veho kek`). Requirements: `veho-tenancy.md` §3 (SEC-9, SEC-10, SEC-24 to SEC-27, SEC-29); providers: `veho-architecture.md` §7.2 and §7.2.1; threats: `veho-threat-model.md` T-06, T-28, T-30, T-37, R-12. The audit signer (SEC-28) comes with the audit chunk (C4), remote providers (`vault-transit`, `hsm`) and the other loaded ones (`barbican`, `vault-kv`, `shamir`) later (backlog 24, 25).

Copied from ligo's `internal/sealing` (`sealing.go`, `localkek.go`, ligo commit 9f11da9) and adapted: ligo seals one envelope under one KEK; veho adds a versioned KEK stored wrapped in the database, an unwrap key from the platform, tenant-bound associated data and rotation.

## 1. Key hierarchy

```
unwrap key (32 bytes, never in the database; from the platform: file or systemd credential)
  └─ wraps ─ KEK version n (32 bytes; table veho.kek, column wrapped)
               └─ wraps ─ DEK of one secret version (32 bytes; veho.secrets.wrapped_dek)
                            └─ encrypts ─ the secret's value (veho.secrets.ciphertext)
```

Every layer is AES-256-GCM with a random 96-bit nonce stored before the ciphertext (`nonce || sealed`). Each layer has its own associated data (AAD), built by length-prefixing every field (a 2-byte big-endian length, then the bytes) after a fixed label, so no two field lists encode alike:

| Layer | Key | AAD |
| :--- | :--- | :--- |
| KEK row | unwrap key | `veho-kek/v1`, KEK version |
| DEK | KEK version n | `veho-dek/v1`, tenant ID (16 bytes), secret name, secret version, KEK version n |
| Value | DEK | `veho-secret/v1`, tenant ID, secret name, secret version |

- **Amendment to SEC-24.** SEC-24 puts the KEK version in the AAD of the value. Here it is in the AAD of the DEK instead, so that a KEK rotation re-wraps the 32-byte DEK and never touches the value (SEC-27: "data keys are re-wrapped"). The binding SEC-24 asks for still holds: a value moved to another tenant, name or version does not decrypt, and neither does a DEK moved to another secret or relabelled with another KEK version. SEC-24's text is to be changed to match.
- A DEK encrypts one value, once: every `Put` makes a new DEK. A KEK wraps one DEK per secret version, so random nonces stay far below GCM's bound of 2^32 messages per key for any realistic number of secrets; a KEK is rotated long before that.
- The unwrap key's **ID** is the first 8 bytes of HMAC-SHA256(unwrap key, `veho-unwrap-key-id`), in hex. It is stored with each KEK row so that a node with the wrong key says so instead of failing with a bare GCM error, and so that an operator sees which key a row needs. It reveals nothing about the key.

## 2. Schema (migration `002_secrets.sql`)

```sql
CREATE TABLE veho.kek (
  version        int PRIMARY KEY CHECK (version > 0),
  wrapped        bytea NOT NULL,           -- nonce || AES-GCM(unwrap key, KEK)
  unwrap_key_id  text NOT NULL,
  state          text NOT NULL CHECK (state IN ('active', 'retiring', 'retired')),
  created_at     timestamptz NOT NULL DEFAULT now(),
  retired_at     timestamptz
);
CREATE UNIQUE INDEX kek_one_active ON veho.kek ((true)) WHERE state = 'active';

CREATE TABLE veho.secrets (
  tenant_id    uuid NOT NULL REFERENCES veho.tenants (id),
  name         text NOT NULL CHECK (name ~ '^[a-z0-9][a-z0-9._-]{0,62}$'),
  version      int NOT NULL CHECK (version > 0),
  kek_version  int NOT NULL REFERENCES veho.kek (version),
  wrapped_dek  bytea NOT NULL,
  ciphertext   bytea NOT NULL,             -- nonce || AES-GCM(DEK, value)
  created_at   timestamptz NOT NULL DEFAULT now(),
  updated_at   timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, name)
);
```

- **KEK states.** `active`: new DEKs are wrapped with it; exactly one (the partial unique index). `retiring`: a former active version, still used to unwrap the DEKs not yet re-wrapped. `retired`: no secret references it (`retired_at` set); its row is kept, so that the history of versions stays visible, and it is never loaded. The foreign key from `secrets.kek_version` stops a version from being deleted while it is used.
- **One row per secret.** A replace overwrites the row and increments `version`; old versions are not kept, because nothing reads them (SEC-10: the next attempt uses the new one).
- **Row-level security** on `veho.secrets` as on the other tenant tables (`veho-store.md` §4): `tenant_id = veho.current_tenant()`.
- **Grants.** `veho_api`: `SELECT, INSERT, UPDATE, DELETE` on `veho.secrets` (it writes them; reading the ciphertext gives it nothing without the KEK, which it holds anyway in the same process). `veho_worker`: `SELECT` on `veho.secrets`. Both: `SELECT` on `veho.kek`. Only `veho_migrator` writes `veho.kek`, and only it reads the secrets of all tenants (it owns the table and RLS is not forced, `veho-store.md` §4), which the re-wrap needs.
- **Delete of a used secret is refused.** A `BEFORE DELETE` trigger on `veho.secrets` raises SQLSTATE 23503 (foreign-key violation) when an endpoint of the same tenant has `credential_ref = 'db://' || name`. `credential_ref` is text, so a real foreign key is not possible; the trigger closes the race that a check in the service would leave.

## 3. Packages and interfaces (`internal/secrets`)

```go
// UnwrapKey sources give the node the unwrap key (SEC-26).
type UnwrapKeySource interface {
    Name() string                         // "file:/etc/veho/unwrap.key", "systemd-credential:veho-unwrap"
    Load(ctx context.Context) ([]byte, error)
}

// KEK wraps and unwraps data keys (SEC-29). The loaded provider holds the
// KEKs in memory; a remote one (later) asks a service.
type KEK interface {
    WrapDEK(ctx context.Context, version int, aad, dek []byte) ([]byte, error)
    UnwrapDEK(ctx context.Context, version int, aad, wrapped []byte) ([]byte, error)
}

// Store is the secret service: write-only to callers, Fetch for attempts.
type Store struct{ /* pools, KEK */ }

func (s *Store) Put(ctx, tenant uuid.UUID, name string, value []byte) (Meta, error)
func (s *Store) List(ctx, tenant uuid.UUID) ([]Meta, error)
func (s *Store) Show(ctx, tenant uuid.UUID, name string) (Meta, error)
func (s *Store) Delete(ctx, tenant uuid.UUID, name string) error
func (s *Store) Fetch(ctx, tenant uuid.UUID, ref string) (*Secret, error)   // ref "db://<name>"

type Meta struct { Name string; Version, KEKVersion int; CreatedAt, UpdatedAt time.Time }

// Secret is a plaintext value held for one attempt (SEC-10).
type Secret struct{ /* value []byte */ }
func (s *Secret) Bytes() []byte
func (s *Secret) Zero()               // clears the bytes; the caller defers it
func (s *Secret) String() string      // "[redacted]"; also MarshalJSON, LogValue, Format
```

- **Unwrap-key sources in C3.** `file`: a path; the file must be a regular file, hold exactly 32 bytes, be owned by root or by the process's own user, and not be readable or writable by group or others (mode 0400 or 0600); with systemd the operator uses `LoadCredential=` so that a root-owned 0400 file reaches a service running as `veho`. `systemd-credential`: a name, read from `$CREDENTIALS_DIRECTORY/<name>` (systemd decrypts a `LoadCredentialEncrypted=` credential, optionally TPM-bound, and sets the mode); it fails if the variable is unset. The key is 32 raw bytes in both: `head -c 32 /dev/urandom` makes one, `systemd-creds encrypt` encrypts it.
- **The loaded provider** (`secrets.LoadKeyring(ctx, pool, source)`, a `Keyring`) reads every `active` and `retiring` row of `veho.kek`, unwraps each with the unwrap key and holds the KEKs in memory. It **fails** (the node does not start, SEC-26) when there is no active row, when a row's `unwrap_key_id` is not the loaded key's (the error names both IDs), or when a row does not decrypt. The unwrap key is cleared once the KEKs are unwrapped; the KEKs live as long as the process.
- **Rotation is picked up without a restart.** A node that meets a `kek_version` it does not hold (a KEK created after it started) reloads the KEK rows once and tries again. `Put` takes `ROW EXCLUSIVE` on `veho.secrets` first and then reads the active version in its own transaction, and `rotate` takes `SHARE` on `veho.secrets`: a `rotate` waits for the `Put`s that saw the old version (the re-wrap that follows finds their DEKs) and the `Put`s after it see the new one, so a node that has not yet seen a rotation does not wrap with a version that has become `retiring`; a `retiring` version is still accepted, so nothing breaks while nodes catch up.
- **Errors.** Every failure of `Fetch` to produce the plaintext (no such secret, the KEK missing, a row that does not decrypt, the database unreachable) wraps `secrets.ErrUnavailable`, and the job lifecycle (C6) makes the attempt `Transient` (SEC-10). A missing secret also wraps `secrets.ErrNotFound`, for the API's 404. Errors never carry the value or the key; a decryption failure says which layer failed, for which tenant, name and versions.
- **Values.** Opaque bytes up to 64 KiB, so that credentials of any driver fit (the endpoint layer, C6, decodes them into `plugin.Credentials`). An empty value is refused.
- **Zeroing is best effort.** `Zero` and the `clear` calls overwrite the buffers veho owns; Go may have copied them (a `string` conversion, an `append` that grows a slice, a buffer inside the TLS or database driver), and the driver's own copies are its own (PLG-16). Core dumps are disabled by the service unit (T-19). This limit is stated in the Operator's Guide.
- **The secret API and its CLI** (`PUT /v1/secrets/{name}`, `veho secret set|show|list|delete`) are transport over `Store` and come with the API (C7). C3 delivers `Store`, its tests and the operator commands of §4.
- **Audit.** SEC-27 and T-37 want every use of the secret API and every KEK change audited. The audit log comes with C4; until then `Store` and the `veho kek` commands log each change with `slog` (tenant, name, versions; never the value), and C4 turns those points into audit entries.

## 4. Operator commands (`veho kek`)

These act on the KEK rows and need the unwrap key and the migrator's DSN (`VEHO_MIGRATOR_DATABASE_URL`, or `--dsn-env`, as `veho migrate`). The unwrap key is given with `--unwrap-key file:<path>` or `--unwrap-key systemd-credential:<name>`.

| Command | What it does |
| :--- | :--- |
| `veho kek show` | Lists the KEK versions: state, unwrap-key ID, times, and the number of secrets each wraps. Needs no unwrap key |
| `veho kek init` | Creates version 1, `active`, if the table is empty; refuses otherwise |
| `veho kek rotate` | Creates version n+1 `active` and makes the old active one `retiring`, in one transaction |
| `veho kek rewrap [--batch N]` | Re-wraps, in batches of N (default 500) with one transaction each, the DEKs that are not under the active version; when a `retiring` version wraps no DEK any more, marks it `retired`. Safe to interrupt and to run again; safe while nodes write, because each update is conditional on the row's `version` and `kek_version` (a secret replaced meanwhile is skipped, and already has a new DEK under the active KEK) |
| `veho kek rekey --new-unwrap-key <source>` | Re-wraps every non-retired KEK row with a new unwrap key, in one transaction (SEC-27); the nodes then need the new key at their next start. The old key is not needed afterwards |

- The housekeeping loop (`veho-audit-housekeeping.md` §3) will run the re-wrap of `veho kek rewrap` in the background; it comes with the chunk that builds the loop, and then needs a `SECURITY DEFINER` function so that it does not run as the migrator. Until then an operator runs `veho kek rewrap` after `veho kek rotate`.
- `rotate` and `rekey` print the new state; nothing they print or log carries a key.

## 5. Threat model of C3

The threats of the threat model that C3 mitigates, and the ones the design adds.

| ID | Threat | Mitigation in C3 | Residual |
| :--- | :--- | :--- | :--- |
| T-28, T-37 | A dump, a backup or a replica of the database is stolen | The database holds only the wrapped KEKs, the wrapped DEKs and the ciphertexts; the unwrap key is never in it (SEC-25); test SECR-2 scans a dump for a canary | Whoever also has a node's unwrap key reads every secret (R-12) |
| C3-1 | A row is moved to another tenant or name, or a DEK to another secret, by someone who can write the database | The AADs of §1 bind value and DEK to tenant, name, version and KEK version: the moved row does not decrypt (SECR-1) | — |
| C3-2 | **Rollback**: someone who can write the database puts back an older row of the same secret (its ciphertext, DEK and version together) | Not prevented: the row is authentic, and only a counter kept outside the database would detect it | Accepted: a database writer can already redirect endpoints (T-30); recorded as R-13 |
| C3-3 | A node runs with the wrong unwrap key, or with a KEK it cannot unwrap, and fails at the first attempt instead of at start | The loaded provider unwraps every non-retired KEK at start and names the unwrap-key IDs on a mismatch (SECR-7) | — |
| C3-4 | The unwrap-key file is readable by other users of the host | `file` refuses a file accessible by group or others, or owned by another user (SECR-7) | A root compromise reads it (R-12) |
| C3-5 | The plaintext reaches a log, an error, a DBOS checkpoint or the API | `Secret` redacts in `String`, `Format`, `MarshalJSON` and `LogValue`; errors carry names and versions only; workflow inputs carry the reference, never the value (SEC-7, C6); no API returns a value (SEC-9); SECR-9 checks the redaction | Copies made by Go or by the driver (§3, zeroing) |
| C3-6 | Nonce reuse under a key | A fresh DEK per value; random nonces under the KEKs and the unwrap key, far below the GCM bound (§1) | — |
| C3-7 | A failed or interrupted re-wrap loses a DEK | Each batch is one transaction; an update is conditional on the row's versions; the old KEK stays loaded until no DEK uses it (SECR-5) | — |
| C3-8 | Two operators rotate at once and leave two active KEKs | The partial unique index allows one `active` row; the second `rotate` fails and changes nothing (SECR-11) | — |

R-13 is added to the threat model with this chunk.

## 6. Acceptance

Unit tests run without a database; integration tests run on the lab's state store (`veho-lab.md`) and are skipped without it, as C2's.

| ID | Test |
| :--- | :--- |
| SECR-1 | Round trip of a value; changing the tenant, the name, the version or the KEK version of a row (value or DEK) makes `Fetch` fail with `ErrUnavailable`; a flipped bit in any of the three layers fails likewise |
| SECR-2 | After `Put` of a canary value, the text of every row of `veho.secrets` and `veho.kek` contains neither the canary nor the KEK (integration; a `pg_dump` of the lab, which needs the PostgreSQL client tools, is the L3 step's) |
| SECR-3 | `List` and `Show` return metadata only; no function of `Store` other than `Fetch` returns the value |
| SECR-4 | Row-level security: a tenant cannot read, list, replace or delete another tenant's secret (integration) |
| SECR-5 | Rotation: after `rotate`, old secrets still `Fetch`; new ones use the new version; `rewrap` moves every DEK and marks the old version `retired`; a secret replaced during the re-wrap is left as written; an interrupted `rewrap` resumes (integration) |
| SECR-6 | `rekey`: the old unwrap key no longer loads, the new one does, every secret still `Fetch`es (integration) |
| SECR-7 | Fail fast: a wrong unwrap key (the error names both IDs), a key file with group or other permissions, of the wrong size, a directory; no active KEK; `systemd-credential` without `$CREDENTIALS_DIRECTORY`, or naming a file outside it. A file of another owner needs root to make and is not tested |
| SECR-8 | `Fetch` of a missing secret wraps `ErrNotFound` and `ErrUnavailable`; with the database unreachable, `ErrUnavailable` |
| SECR-9 | `Secret` prints `[redacted]` with `%v`, `%s`, `%+v`, `%#v`, through `slog` (text and JSON) and `json.Marshal`; `Zero` clears its bytes |
| SECR-10 | Deleting a secret referenced by an endpoint fails with SQLSTATE 23503; unreferenced, it succeeds (integration) |
| SECR-11 | Two concurrent `rotate`: one succeeds, the other fails, one `active` row remains (integration) |
| SECR-12 | A node that started before a `rotate` `Fetch`es a secret written after it (the reload of §3) (integration) |
| Fuzz | `FuzzOpen`: arbitrary bytes as wrapped DEK or ciphertext never panic and never decrypt |

## 7. Documentation

The guides are not written yet (backlog 7); until then this section lists what they take from this chunk, and this specification is the reference.


- Setup Guide: making the unwrap key (file and systemd credential, with and without TPM), `veho kek init`.
- Operator's Guide: rotation (`rotate`, `rewrap`), a change of the unwrap key (`rekey`), the errors at start and what they mean, the limits of zeroing.
- Architecture and Design Guide: the key hierarchy of §1 and the AADs.
