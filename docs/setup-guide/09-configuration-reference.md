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
| `notifications.enabled` | `false` | Whether tenants may register webhooks and this cluster delivers them (*Operator's Guide*, Webhooks) |
| `notifications.allow_cidrs` | none | CIDRs a webhook may reach although they are private, such as `10.20.0.0/16`; the cloud metadata addresses are never allowed |
| `notifications.delivered_retention` | `24h` | How long delivered webhook events are kept; `0s` removes them at the next pass, else at least `1m` |
| `notifications.abandoned_retention` | `720h` | How long abandoned and undelivered events are kept; at least `1h` |
| `network_checks.interval` | `24h` | How often each endpoint's network flows are checked again from every node, in the background; `0` turns the periodic checks off; at least `1h` |
| `network_checks.per_pass` | `20` | The most endpoints a housekeeping pass (every 30 s) checks, the oldest first: it bounds the load however many endpoints there are; 1 to 1000 |
| `drivers[].name` | (required) | A driver this node offers, once: `file` and `http` are built in, any other name is a plugin, by default `/usr/libexec/veho/plugins/veho-plugin-<name>` with its signed manifest. A driver not listed is not available |
| `drivers[].key` | `veho` | For an installed plugin, the key file `/etc/veho/plugin-keys/<key>.pub` whose keys must have signed its manifest: `veho` is the release's, another name a third-party publisher's. Not on a built-in driver or with `path` |
| `drivers[].path` | none | A plugin pinned by the operator, without a manifest: its executable, with `sha256` |
| `drivers[].sha256` | none | The SHA-256 of `path`'s file: a plugin whose file differs is not started |
| `drivers[].settings` | `{}` | The driver's settings, a map; the driver refuses keys it does not know (*Operator's Guide*, drivers) |
| `drivers[].settings.allow_cidrs` | none | `s3`, `http`, `sftp`, `ftp`, `glance`, `swift`, `nexus`: CIDRs the driver may reach although they are private, such as `10.20.0.0/16` for an S3 service on the platform's network; the cloud metadata addresses are never allowed |
| `drivers[].settings.proxy.url` | none | `s3`, `http`, `nexus`: an `http://host:port` forward proxy, for the names of `proxy.hosts` (*Operator's Guide*, sources) |
| `drivers[].settings.proxy.hosts` | none | The names whose `https` connections go through the proxy, unresolved by veho: exact names, or `*.` and a suffix of at least two labels (`*.amazonaws.com`); never IP addresses |
| `drivers[].settings.proxy.credentials_file` | none | A file of `user:password` for the proxy's Basic authentication, mode 0600 |
| `drivers[].settings.plaintext` | `false` | `ftp`: allow endpoints with `tls: none`, whose password crosses the network in clear; `glance`, `swift`: allow `http` for Keystone and the services; `nexus`: allow `http` for Nexus (a lab only) |
| `drivers[].settings.max_npm_size` | `104857600` | `nexus`: the largest npm package an upload takes, held in the plugin's memory |
| `drivers[].settings.max_segments` | `1000` | `swift`: the cluster's `max_manifest_segments`, the most parts of an object |
| `drivers[].settings.roots` | none | `file`: the directories an endpoint's root may be or be under; empty, no `file` endpoint can be created |
| `metrics.listen` | `127.0.0.1:9464` | The Prometheus listener, `host:port`, or `off` |
| `api.listen` | `:8443` | The API's address: gRPC and REST on one TLS port |
| `api.tls.cert_file` | (required) | The API's certificate (PEM, with its chain) |
| `api.tls.key_file` | (required) | The certificate's key |
| `api.max_watches` | `1000` | Streams (`WatchJob`) open at once on this node |
| `api.advertise_url` | none (never a forwarding target) | How the other nodes reach this node's API: `https://host:port`, the host named by the API certificate |
| `api.forward.peer_ca_file` | none (forwarding off) | The CA of the nodes' API certificates. Set, the node forwards its API calls to the node nearest the state store, and believes the client address that a node with such a certificate forwards; it needs `api.advertise_url` |
| `api.oidc[].name` | (required with an issuer) | The issuer's short name, `[a-z0-9-]`, unique: what `veho auth login --issuer` takes |
| `api.oidc[].issuer` | none | An OIDC issuer whose tokens the API accepts: an `https` URL, once each |
| `api.oidc[].audience` | (required with an issuer) | The audience the tokens must carry |
| `api.oidc[].groups_claim` | `groups` | The claim listing the user's groups, for entitlements and policy grants to groups |
| `api.oidc[].max_token_lifetime` | `1h` | The longest lifetime of a token, from its `iat`, which is required, to its `exp`; at most 24h |
| `api.oidc[].ca_file` | the system's roots | The CA that verifies the issuer's TLS |
| `api.oidc[].cli_client_id` | none (no CLI login) | The public client of `veho auth login`; it must differ from every issuer's `audience` |
| `api.oidc[].scopes` | `openid offline_access` | The scopes the CLI asks for; `offline_access` gives the refresh token |
| `api.oidc[].step_up.acr_values` | none (no step-up) | The `acr` values one of which a token needs to call a sensitive method (changing secrets, endpoints, webhooks, entitlements, service accounts and keys, grants, settings, tenants) |
| `api.oidc[].step_up.max_age` | (required with `acr_values`) | How old the token's `auth_time` may be for a sensitive method |
| `api.spiffe.trust_domain` | none (SPIFFE off) | The SPIFFE trust domain whose SVIDs the API accepts |
| `api.spiffe.x509_bundle_file` | none | The trust domain's X.509 bundle: clients authenticate with an X.509-SVID |
| `api.spiffe.jwt_bundle_file` | none | The trust domain's JWT bundle: clients authenticate with a JWT-SVID |
| `api.spiffe.audience` | (required with a JWT bundle) | The audience JWT-SVIDs must carry |
| `api.rate_limit.tenant_per_second` | `50` | Calls per second one tenant may make on this node |
| `api.rate_limit.principal_per_second` | `20` | Calls per second one principal may make on this node |
| `api.rate_limit.burst` | `2` | The burst, as a multiple of each rate; at least 1 |
| `api.keys.default_lifetime` | `2160h` (90 days) | The lifetime of an API key created without an expiry |
| `api.keys.max_lifetime` | `2160h` (90 days) | The longest lifetime an API key may be given; at least `default_lifetime` |
| `api.rate_limit.failures_per_minute` | `30` | Refused credentials per minute after which a client address is blocked for a while |
| `checks.primary` | `refuse` | A connection to a standby or through a pooler: `refuse` stops the start, `warn` logs it and starts |
| `checks.synchronous_commit` | `refuse` | `synchronous_commit` off or local: `refuse` or `warn` |
| `checks.quorum` | `refuse` | `synchronous_standby_names` not a quorum over other regions: `refuse` or `warn` |
| `checks.clock` | `refuse` | The node's clock off the database's by more than `checks.max_clock_offset`: `refuse` or `warn` |
| `checks.max_clock_offset` | `1s` | The largest difference between the node's clock and the database's |

:::

## The environment

| Variable | Read by | Meaning |
| :---------------------------- | :---------------------- | :---------------------------------------------------- |
| `VEHO_API_DATABASE_URL`, `VEHO_WORKER_DATABASE_URL`, `VEHO_DBOS_DATABASE_URL` | `veho serve` | The DSNs (names configurable above) |
| `VEHO_MIGRATOR_DATABASE_URL` | `veho migrate`, `veho kek`, `veho bootstrap` | `veho_migrator`'s DSN (`--dsn-env` names another variable) |
| `VEHO_LOG_LEVEL` | every command | `debug`, `info`, `warn`, `error` or `off` (the default when unset; the unit sets `info`) |
| `VEHO_API_URL`, `VEHO_CA_FILE`, `VEHO_API` | the client | The API's URL, its CA, `grpc` or `rest` |
| `VEHO_TENANT` | the client | The tenant to act in, or `platform`; required without an API key |
| `VEHO_TOKEN`, `VEHO_TOKEN_FILE`, `VEHO_CERT_FILE`, `VEHO_KEY_FILE` | the client | The credential: an API key, an OIDC token or a JWT-SVID; or a client certificate. A token file readable by group or others is refused |
