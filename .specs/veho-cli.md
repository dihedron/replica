# veho: the command line client (chunk C8)

**Status:** decided under the overnight delegation of 2026-11-22, for review; implemented 2026-11-22 (`internal/client`, `cmd/veho/cli*.go`). Requirements: `veho-api.md` §4 (the command table, API-9), `AGENTS.md` (the `veho <object> <verb>` convention, `show` to display), `veho-api-server.md` (the services the client calls).

**Amended** 2026-11-25 by `veho-authentication.md`; `veho serviceaccount` implemented with A2 and `veho auth login|logout|status` with A4a (2026-11-26), `veho tenant delete` to be: §1 (`veho auth login|logout|status`, `veho serviceaccount`, `veho tenant delete`), §2 (the sign-in, the token cache, `--tenant`), its §7, §8.

## 1. Commands

| Command | Service calls |
| :--- | :--- |
| `veho job submit \| show \| list \| wait \| cancel \| retry \| watch` | `JobService` |
| `veho policy create \| show \| list \| delete \| validate \| grant \| revoke` | `PolicyService` |
| `veho endpoint create \| show \| list \| update \| delete \| test` | `EndpointService` |
| `veho secret set \| show \| list \| delete` | `SecretService` |
| `veho serviceaccount create \| list \| show \| disable \| enable \| delete`, `veho apikey create --service-account \| list \| revoke`, `veho entitlement create \| list \| delete`, `veho settings show \| update` | `AdminService` (a tenant admin) |
| `veho tenant create \| show \| list \| update` | `TenantService` (a platform administrator) |
| `veho bootstrap tenant \| apikey \| entitlement` | the database, as `veho_migrator` (`veho-api-server.md` §4.3, §4.4) |

- **Bootstrap moves under `veho bootstrap`** (**amendment** of `veho-api-server.md` §4.3 and §4.4): `veho tenant create`, `veho apikey create` and `veho entitlement create` are now the API's; the commands that write the database directly, for the first administrator, are `veho bootstrap tenant <name>`, `veho bootstrap apikey …` and `veho bootstrap entitlement …`, with the same options as before.
- `veho migrate`, `veho kek`, `veho serve` and `veho version` stay; `veho completion bash|zsh` prints a function that asks the binary for its completions (go-flags' `GO_FLAGS_COMPLETION`), so they always match it.

## 2. Connection and credentials

- `--api-url` (`VEHO_API_URL`): `https://host:port` of a node or a balancer. `--ca-file` (`VEHO_CA_FILE`): the CA of the API's certificate; the system's roots without it. No option turns verification off.
- **The credential is never on the command line** (the process list shows it): `VEHO_TOKEN` (an API key, an OIDC token or a JWT-SVID) or `--token-file` (`VEHO_TOKEN_FILE`); or a client certificate, `--cert-file` and `--key-file` (an X.509-SVID).
- `--api grpc|rest` (`VEHO_API`), default `grpc` (API-9). Both transports go through one generic call: for REST the client builds each request from the method's `google.api.http` annotation (the path's variables from the request, the rest in the body or the query) and decodes the answer, so the two cannot disagree on a field, and a new method needs no client code beyond its command.
- `--timeout` [30s] per call.

## 3. Output

- Default: a short human form (a table for lists, `key: value` lines for one object). `--output json` (`-o json`): the API's JSON (proto field names), one document, for scripts.
- Errors: `error: <code>: <message>` on standard error, and the request ID when the server gave one.
- **Exit codes**: 0 success; 1 an error (the API's or the client's); for `job wait`: 0 `SUCCEEDED`, 3 `FAILED`, 4 `CANCELLED`, 5 the wait's own timeout (`--timeout-wait`, default none).

## 4. Inputs

- `job submit --source <endpoint>:<path> (--policy-file <file> | --policy <name>[@<version>]) [--label k=v]... [--idempotency-key k]`; a policy file of `-` is standard input.
- `policy create <name> --file <file>`, `policy validate --file <file>`, `policy grant <name> <grantee> <permission>`, `policy revoke <name> <grantee> <permission>` (read, change and set the grants).
- `endpoint create <name> --driver d --location l [--config-file f] [--credential-ref db://s]`; `update` takes the same, the driver excepted.
- `secret set <name> (--from-file f | --stdin)`: never from an argument.
- `job wait <id>` polls with the API's long poll (`GetJob` with `wait` and `If-None-Match`, over both transports) until the job is settled.

## 5. Threat model

| ID | Threat | Mitigation |
| :--- | :--- | :--- |
| C8-1 | A credential or a secret value in the process list or the shell history | Credentials from the environment, a file or a client certificate only; secret values from a file or standard input only |
| C8-2 | A man in the middle | TLS verified against the system's roots or `--ca-file`; no option to skip verification |
| C8-3 | A token file readable by others | The client refuses a token file accessible by group or others, as the unwrap-key file (`veho-secrets.md` §3) |

## 6. Acceptance

| ID | Test |
| :--- | :--- |
| CLI-T1 | Against `veho serve` on the lab, over gRPC and over REST: bootstrap a tenant and its admin; create a secret, an endpoint, a policy and its grants; submit a job by reference and inline; `job wait` exits 0; `job list -o json` parses; cancel and retry answer as the API does |
| CLI-T2 | `job wait` exits 3 on a job that fails |
| CLI-T3 | Without a credential, with a wrong one: exit 1 and `unauthenticated`; a token file of mode 0644 refused |
| CLI-T4 | The REST request built from each annotation: path variables, body and query, for every method of every service (unit test against the descriptors) |
