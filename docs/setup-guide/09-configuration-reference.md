# Configuration reference

Every key of `veho.yaml`. Durations are Go durations (`30s`, `168h`). A node refuses unknown keys and invalid values at start, naming each one.

::: landscape

| Key | Default | Meaning |
| :--------------------------------- | :------------------------ | :---------------------------------------------------------------------------- |
| `node.id` | the host name | This node's executor ID: unique and stable for the host's life; it names the node's work in the database |
| `node.region` | (required) | The location of the topology this node is in |
| `database.api.dsn_env` | `VEHO_API_DATABASE_URL` | The environment variable holding `veho_api`'s DSN |
| `database.worker.dsn_env` | `VEHO_WORKER_DATABASE_URL` | The environment variable holding `veho_worker`'s DSN |
| `database.dbos.dsn_env` | `VEHO_DBOS_DATABASE_URL` | The environment variable holding `veho_dbos`'s DSN |
| `secrets.unwrap_key` | (required) | Where the unwrap key comes from: `systemd-credential:<name>` or `file:<path>` |
| `audit.sink` | `syslog` | Where audit events go: `syslog` (the local `/dev/log`, facility `authpriv`) or `stderr` |
| `engine.heartbeat` | `10s` | How often the node records that it is alive and renews its leases |
| `engine.lease_ttl` | `60s` | How long a transfer stays a node's after its last heartbeat; at least three heartbeats. A node unseen for this long has its work adopted |
| `engine.part_size_mib` | `64` | The size of a part, 5 to 512 MiB; larger for objects that would otherwise have too many parts |
| `engine.parts_in_flight` | `4` | Parts of one transfer copied at once, 1 to 64 |
| `engine.part_memory_mib` | `1024` | The node's budget of parts in memory, across all its transfers; at least `part_size_mib` |
| `engine.worker_concurrency` | `16` | Workflows this node runs at once, per queue |
| `engine.tenant_concurrency` | `4` | Workflows one tenant runs at once on this node, per queue; at most `worker_concurrency` |
| `housekeeping.interval` | `30s` | How often housekeeping runs (on one node at a time) |
| `housekeeping.workflow_retention` | `168h` | How long DBOS keeps the records of ended workflows; `0s` keeps them, else at least `24h` |
| `housekeeping.jobs_retention` | `2160h` | How long settled jobs are kept; `0s` keeps them, else at least `24h` |
| `plugins[].name` | (required) | A process plugin's driver name, unique |
| `plugins[].path` | (required) | The plugin's executable |
| `plugins[].sha256` | (required) | The executable's SHA-256: a plugin whose file differs is not started |
| `metrics.listen` | `127.0.0.1:9464` | The Prometheus listener, `host:port`, or `off` |
| `api.listen` | `:8443` | The API's address: gRPC and REST on one TLS port |
| `api.tls.cert_file` | (required) | The API's certificate (PEM, with its chain) |
| `api.tls.key_file` | (required) | The certificate's key |
| `api.max_watches` | `1000` | Streams (`WatchJob`) open at once on this node |
| `api.oidc[].issuer` | none | An OIDC issuer whose tokens the API accepts: an `https` URL, once each |
| `api.oidc[].audience` | (required with an issuer) | The audience the tokens must carry |
| `api.oidc[].groups_claim` | `groups` | The claim listing the user's groups, for entitlements and policy grants to groups |
| `api.oidc[].ca_file` | the system's roots | The CA that verifies the issuer's TLS |
| `api.spiffe.trust_domain` | none (SPIFFE off) | The SPIFFE trust domain whose SVIDs the API accepts |
| `api.spiffe.x509_bundle_file` | none | The trust domain's X.509 bundle: clients authenticate with an X.509-SVID |
| `api.spiffe.jwt_bundle_file` | none | The trust domain's JWT bundle: clients authenticate with a JWT-SVID |
| `api.spiffe.audience` | (required with a JWT bundle) | The audience JWT-SVIDs must carry |
| `api.rate_limit.tenant_per_second` | `50` | Calls per second one tenant may make on this node |
| `api.rate_limit.principal_per_second` | `20` | Calls per second one principal may make on this node |
| `api.rate_limit.burst` | `2` | The burst, as a multiple of each rate; at least 1 |
| `api.rate_limit.failures_per_minute` | `30` | Refused credentials per minute after which a client address is blocked for a while |

:::

## The environment

| Variable | Read by | Meaning |
| :---------------------------- | :---------------------- | :---------------------------------------------------- |
| `VEHO_API_DATABASE_URL`, `VEHO_WORKER_DATABASE_URL`, `VEHO_DBOS_DATABASE_URL` | `veho serve` | The DSNs (names configurable above) |
| `VEHO_MIGRATOR_DATABASE_URL` | `veho migrate`, `veho kek`, `veho bootstrap` | `veho_migrator`'s DSN (`--dsn-env` names another variable) |
| `VEHO_LOG_LEVEL` | every command | `debug`, `info`, `warn`, `error` or `off` (the default when unset; the unit sets `info`) |
| `VEHO_API_URL`, `VEHO_CA_FILE`, `VEHO_API` | the client | The API's URL, its CA, `grpc` or `rest` |
| `VEHO_TOKEN`, `VEHO_TOKEN_FILE`, `VEHO_CERT_FILE`, `VEHO_KEY_FILE` | the client | The credential: an API key, an OIDC token or a JWT-SVID; or a client certificate. A token file readable by group or others is refused |
