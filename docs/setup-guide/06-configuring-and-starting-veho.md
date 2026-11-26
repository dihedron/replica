# Configuring and starting veho

Each node reads two files from `/etc/veho` (owned by root, group `veho`, mode 0640):

- `veho.yaml`, the configuration: this node, the API, the engine's tuning. It holds no password.
- `veho.env`, the environment of the service: the three database DSNs, which carry passwords.

The service's unit reads both: `veho serve` reads `/etc/veho/veho.yaml` (`--config` names another file), and systemd loads `veho.env` into its environment.

## veho.yaml

A node of region A, with every setting it must have; the others have defaults (chapter *Configuration reference*):

```yaml
node:
  id: veho-a1               # unique and stable: it names this node's work in the database
  region: region-a          # a location created in the topology
secrets:
  unwrap_key: "systemd-credential:unwrap-key"
drivers:                    # the storage drivers this node offers: no other
  - name: s3                # the plugin of its package (veho-plugin-s3)
    settings:               # an S3 service on a private network: its range,
      allow_cidrs: [10.20.0.0/16]  # which the dial guard refuses otherwise
api:
  listen: ":8443"
  tls:
    cert_file: /etc/veho/tls/server.crt
    key_file: /etc/veho/tls/server.key
```

Every node must list the same drivers: a transfer runs on any node.

`node.id` must never change for a host and never be shared: housekeeping uses it to tell a dead node's work from a live one's. It defaults to the host name.

The node logs the SHA-256 of `veho.yaml` at start, so that an unexpected change of configuration shows in the logs. Unknown keys are refused, and so are invalid values, each named in the error.

### An identity provider

People sign in with an OIDC identity provider (`veho auth login`, the Operator's Guide). veho accepts its access tokens, as JWTs whose audience is veho's. With Keycloak, in the realm of your users:

1. A client scope with an **Audience** mapper adding `veho-api` to access tokens, assigned to the client below.
2. A **public** client `veho-cli`: standard flow and **OAuth 2.0 Device Authorization Grant** on, PKCE method `S256`, valid redirect URI `http://127.0.0.1/*`.
3. For step-up, the realm's **ACR to LoA mapping** (for example `1` a password, `2` a password and a one-time code) and an authentication flow that honours it.

```yaml
api:
  oidc:
    - name: corp
      issuer: https://keycloak.example/realms/corp
      audience: veho-api
      cli_client_id: veho-cli
      step_up: {acr_values: ["2"], max_age: 15m}   # optional
```

Then entitle people or groups: `veho bootstrap entitlement --kind oidc --issuer https://keycloak.example/realms/corp --group storage-admins --tenant acme --role admin`.

## veho.env

```bash
VEHO_API_DATABASE_URL=postgres://veho_api:<password>@db-a:5432,db-b:5432,db-c:5432/veho?sslmode=verify-full&sslrootcert=/etc/veho/tls/ca.crt&target_session_attrs=read-write
VEHO_WORKER_DATABASE_URL=postgres://veho_worker:<password>@...
VEHO_DBOS_DATABASE_URL=postgres://veho_dbos:<password>@...
```

The variable names are the defaults of `database.api.dsn_env`, `database.worker.dsn_env` and `database.dbos.dsn_env`. The unit also sets `VEHO_LOG_LEVEL=info`; `veho.env` may change it (`debug`, `warn`, `error`, `off`).

## The files

```bash
sudo install -d -o root -g veho -m 0750 /etc/veho/tls
sudo install -o root -g veho -m 0644 ca.crt server.crt /etc/veho/tls/
sudo install -o root -g veho -m 0640 server.key /etc/veho/tls/
sudo install -o root -g veho -m 0640 veho.yaml veho.env /etc/veho/
```

plus the unwrap key's credential and its drop-in (chapter *The unwrap key*).

## Starting

```bash
sudo systemctl enable --now veho
journalctl -u veho -f
```

A node starts in this order, and stops at the first step that fails, saying why: the configuration; the audit sink; the database pools and the checks of *The state store*; the unwrap key and the KEK; the topology (its region must exist); the drivers, each plugin's manifest checked and the plugin started with its settings; one heartbeat, so that the node is registered before it takes any work; the workflow engine; the API.

Check each node:

```bash
curl --cacert ca.crt https://veho-a1.example:8443/healthz   # 200: the process is up
curl --cacert ca.crt https://veho-a1.example:8443/readyz    # 200: it can serve
```

`/readyz` answers 200 only when the node reaches the state store, its workflow engine is running and it is not fenced (a node that cannot write its heartbeat for half its lease stops its own work).

Start the nodes of all three regions. Any node can take any request and copy any object; there is no leader to configure.

## Stopping

`systemctl stop veho` stops taking new work, lets the copies in flight finish their current step for up to 30 seconds, and exits. The node's unfinished transfers stay in the database and another node adopts them once the node's lease has expired (about a minute).
