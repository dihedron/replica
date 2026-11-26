# veho: the development mode

This file specifies backlog item 43: running veho on one node and one PostgreSQL, with local users and passwords, to experiment with it without the machinery of a real deployment (Patroni, an identity provider). It is part of `veho_specification.md` and builds on `veho-authentication.md`.

Status: approved, 2026-11-25; not implemented. The decisions of §2 were taken with the maintainer the same day.

**This mode is not secure or reliable enough for real data.** One database is one point of failure, and a password is the only factor.

## 1. Today

veho already runs on a single PostgreSQL with `checks: {quorum: warn}`: CI runs the integration tests that way (`test/ci/postgres.sh`), with a full node that runs jobs (`TestCLI`). The setup is long (roles, four DSNs, TLS, KEK, locations in SQL, bootstrap), undocumented, and no test kills a standalone node during a transfer.

## 2. Decisions (2026-11-25)

| # | Question | Decision |
| :--- | :--- | :--- |
| 1 | Configuration | One setting, `mode: development` (default `production`). It relaxes the quorum check and permits local users; neither local users nor their login exists without it. |
| 2 | Database | PostgreSQL, set up by one command. Not SQLite: DBOS supports it, but veho's store rests on row-level security, four roles, `SECURITY DEFINER` functions, advisory locks and about 174 statements written for PostgreSQL. A second store would isolate tenants in another way than production. |
| 3 | Where local users sign in | The Web UI and the CLI. |

## 3. Guards

- **The database is marked.** `veho migrate --development`, on an empty database only, marks it as a development one, for good. `veho serve` refuses to start when its `mode` differs from the mark, so a production database is never served in this mode, nor a development one in production.
- **One node.** A node refuses to start while another executor is live (`executors.last_seen` within `engine.lease_ttl`).
- **No OIDC**: `api.oidc` with this mode is a configuration error. SPIFFE is allowed, so that ligo can be tried.
- **Loopback.** `api.listen` must be a loopback address, unless `development.allow_remote: true`. TLS is always on.
- **Visible.** A warning at start and every hour, a banner on each UI page, `veho version --verbose` and the metric `veho_development_mode`.
- Only `checks.quorum` is relaxed, to `warn`. The primary, synchronous commit, schema and clock checks keep their severity.

## 4. Local users

- `local_users(id, username, password_hash, must_change, disabled_at, created_at, changed_at)`. Usernames: `[a-z0-9][a-z0-9._-]{0,62}`. No tenant column and no direct grant on the table: access is through `SECURITY DEFINER` functions, which fail on a database not marked as development.
- The identity is `local:<username>`, of issuer `local`. Entitlements of kind `local` give the tenant and the role, as for any identity, and the caller names the tenant (`veho-authentication.md` §4).
- A `platform-admin` creates, lists, disables, enables and deletes users, and sets or resets passwords: `LocalUserService`, `veho localuser create|list|disable|enable|delete|set-password`, and the UI. A password is read from a file or standard input, never an argument. A reset sets `must_change` and ends the user's sessions and tokens, as do disabling and deleting.
- `veho bootstrap localuser <name> --platform-admin` makes the first one, and prints a random password once.
- Passwords: Argon2id (19 MiB, 2 passes, 1 lane; `golang.org/x/crypto`, already a dependency), 12 to 128 characters, no composition rules. At most 4 hashes at once. An unknown user costs the same hash as a known one.
- Failures are limited per address and per username. The username's limit slows attempts down; it does not lock the account.
- A user whose `must_change` is set can only change the password. Changing it needs the current one and ends the other sessions.

## 5. Login

- A login gives an access token: `vehol_` and 32 random bytes, its SHA-256 stored, 8 hours, revocable. It is a bearer credential of the API, through the same interceptors.
- **Web UI**: a form at `/ui/login/local`, with the double-submit pre-login cookie of ligo §7.3. The session then holds the token as it would hold an OIDC access token (`veho-authentication.md` §6.2), without refresh.
- **CLI**: `veho auth login` asks for the username and the password on the terminal, without echo, and caches the token. `AuthService.GetConfig` says that the mode is local.
- `AuthService.LocalLogin` and `ChangePassword` answer `Unimplemented` in production. Step-up does not apply.

## 6. One command

`test/dev/dev.sh up [DIR]` builds veho, starts PostgreSQL in a container with a persistent volume, and prepares TLS, roles, the marked schema, the KEK, one location, a tenant and a first user. It writes `veho.yaml`, starts the node, and prints the URL and the first password. `down` stops it and keeps the data; `reset` removes everything after a confirmation; `status` shows it. It shares its PostgreSQL part with `test/ci/postgres.sh`. Container name, port and directory are its own, so it does not touch the lab or CI's database. The Setup Guide gets a chapter for it, and for a PostgreSQL that already exists.

## 7. Threat model

| ID | Threat | Mitigation | Residual |
| :--- | :--- | :--- | :--- |
| DM-1 | The mode is enabled on a production deployment | The database's mark; one node; no OIDC; loopback; visible | An operator who builds a development database and then puts real data in it |
| DM-2 | Password guessing | Argon2id, limits, 12 characters | Weak passwords within the rules; no second factor |
| DM-3 | Login as a denial of service (Argon2id's memory) | 4 hashes at once, limits before the hash | Logins slow down under attack |
| DM-4 | Finding which usernames exist | The same answer and cost | — |
| DM-5 | A stolen token or cookie | Hashed in the database, 8 hours, revocable, TLS | Malware on the person's host |
| DM-6 | A password in a log, the shell history or the process list | Read from the terminal, a file or standard input; a canary test | — |
| DM-7 | Local users survive into production | The mark; the functions refuse | — |

## 8. Acceptance

| ID | Test |
| :--- | :--- |
| DM-T1 | `serve` refuses each mismatch of mode and mark, a live second executor, `api.oidc`, and a non-loopback address without `allow_remote`; `migrate --development` refuses a database that is not empty |
| DM-T2 | In production: `LocalLogin` and `LocalUserService` are `Unimplemented`, the form is `404`, and a `vehol_` token is refused |
| DM-T3 | Users: create, sign in, forced change, reset (the sessions end), disable, enable, delete; a tenant admin is refused |
| DM-T4 | Login through the UI and the CLI; entitlements, tenants and roles behave as for OIDC |
| DM-T5 | Wrong password and unknown user: the same answer, and a comparable time; the limits; the right password still works from another address |
| DM-T6 | Canary: no password or token in a log, audit event, error or database column |
| DM-T7 | One node and one PostgreSQL: the node is killed (`SIGKILL`) during a transfer, then PostgreSQL is; after each restart the job ends and its copies are intact |
| DM-T8 | `dev.sh up` from nothing to a job that succeeds; `down`, `up` keeps the data; `reset` leaves nothing |

Fuzz: the username and token parsers.

## 9. Open

- `dev.sh` needs Docker or Podman. Without one, the Setup Guide's chapter for an existing PostgreSQL applies.
