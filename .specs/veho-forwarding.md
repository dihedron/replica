# veho: forwarding API calls to a node near the primary

**Status:** approved 2026-11-27 (requested by the maintainer after the first AC-24 night). Requirements: NFR-4 (submission p99 < 1 s up to a 150 ms round trip), `veho-latency-envelope.md` §3, `veho-latency-campaign.md` §8.

## 1. Why

The first AC-24 night (commit `0d84d64`) measured acceptance p50 ≈ 590 ms at a 25 ms round trip and ≈ 1170 ms at 50 ms. `log_statement` showed the cause:
- one `CreateJob` sends **26 statements, one after another**, to the primary;
- each one is a WAN round trip from a node outside the primary's region.

The envelope had assumed one round trip, the commit. So NFR-4 fails from 50 ms.

**Decided with the maintainer (2026-11-27):** a node far from the primary forwards its API calls to a node near it. There the statements are local; a call costs one round trip between the nodes plus the commit's wait for the standby (≈ 2R), whatever the statement count.

**Not chosen now (backlog):**
- trimming and pipelining the acceptance's statements;
- a per-node cache of principals, invalidated by NOTIFY (under forwarding the far node does not authenticate, so the cache saves little);
- forwarding streams and calls authenticated by an X.509-SVID.

## 2. Design

### 2.1 Which node is near

- Every heartbeat (`engine.heartbeat`, 10 s), a node measures its round trip to the database: the least of three `select 1`.
- It writes the result to `veho.executors.db_rtt_us`, with `api_url`, its `api.advertise_url`. It also exports `veho_db_rtt_seconds`.
- Each refresh (every heartbeat), the API reads the executors. A **peer** is an executor other than itself that is live (`last_seen` within 3 heartbeats) and has an `api_url` and a `db_rtt_us`.
- The node **forwards** when:
  - its own `db_rtt` is over 5 ms;
  - the best peer's, the lowest (ties by id), is under a quarter of it.
- No configuration names the primary's region: the measurement finds it.
- After a failover the choice follows within a heartbeat. Meanwhile, a call may reach a node that is now far: still correct, only slower.

### 2.2 What is forwarded

**Every unary call**, read or write, by an interceptor placed after the deadline and before authentication. A forwarded call touches no database on the far node. These stay local:
- streams (`WatchJob`);
- the public methods;
- calls carrying `x-veho-forwarded` (a receiver never forwards again);
- calls whose credential is a client certificate that the node verifies as an X.509-SVID (it cannot be relayed);
- every call, when forwarding is off.

**The call:**
- It goes over a gRPC connection per peer, kept open (TLS verified against `api.forward.peer_ca_file`, with the node's API certificate as client certificate).
- The incoming metadata goes with it, without:
  - the transport's own keys (`:authority`, `content-type`, `user-agent`, `grpc-*`);
  - the in-process pipe's metadata (`x-forwarded-for`, `x-veho-peer-chain`);
  - `x-veho-forwarded-for`.
- The forwarder adds:
  - `x-veho-forwarded` (its node ID);
  - `x-veho-forwarded-for` (the client's address);
  - `x-request-id` (the call's, made if missing).

**The answer:**
- The receiver's headers and trailers are passed back, so REST keeps `Location`, `ETag`, `X-Request-Id` and the HTTP code.
- Its error is returned as it is, details included (`ErrorInfo`, step-up).
- The receiver authenticates, authorises, rate-limits and audits the call as any other.

**Authentication of a forwarded call:** a call carrying `x-veho-forwarded` authenticates by its bearer token, never by a client certificate. The receiver thus never takes the peer node's certificate for an X.509-SVID. The marker grants nothing: a client that sets it only chooses its own bearer over its own certificate, and is not forwarded.

### 2.3 Failures

- **Peer not ready:** a call is forwarded only when the peer's connection is `READY`. Otherwise it is served locally, and the connection is asked to connect.
- **`CreateJob` without an idempotency key:** the forwarder gives it `fw-<request ID>`, so that a local retry after an ambiguous failure (the peer may have committed) finds the job instead of making a second one.
- **Any other failure** to reach the peer during a call:
  - `CreateJob` is served locally with that key;
  - any other call returns `unavailable` (the client retries, as after a database failover).

### 2.4 The client's address

- The receiver takes `x-veho-forwarded-for` as the client's address (rate limits of refused credentials, audit) only from a **peer node**. That is a TLS client certificate that verifies against `api.forward.peer_ca_file` (client authentication) and names, in a SAN, the host of a live peer's `api_url`.
- Otherwise the header is ignored and the address is the TCP peer's.

### 2.5 Configuration

- `api.advertise_url` (`https://host:port`) is how peers reach the node. A node without one is never a target.
- `api.forward.peer_ca_file`: the CA of the nodes' API certificates. Set, forwarding is on. Empty, the node neither forwards nor believes `x-veho-forwarded-for`.
- The nodes' API certificates must allow client authentication (`extendedKeyUsage = serverAuth, clientAuth`), and name the host of `advertise_url`.
- Checked at load: `advertise_url` is an `https` URL with a host and no path; a `peer_ca_file` needs an `advertise_url`.

### 2.6 Observability

- `veho_api_forwarded_total{outcome}`, where the outcome is `forwarded`, `fallback` (served locally after a failure) or `failed` (unavailable returned).
- `veho_db_rtt_seconds`.
- A forwarded call is logged at debug by the forwarder. The receiver's audit names the principal and request ID as usual.

## 3. Threat model

| ID | Threat | Mitigation | Residual |
| :--- | :--- | :--- | :--- |
| FW-1 | A bearer token relayed to a rogue peer | Peers come only from `veho.executors`, which only `veho_worker` writes, trusted as the database is. TLS is verified against `peer_ca_file` and the host of `api_url`. A token is already valid on every node of the cluster. | An attacker who writes the database and holds a certificate of the nodes' CA |
| FW-2 | A spoofed client address, to dodge the limit on refused credentials or to mislead the audit | `x-veho-forwarded-for` is believed only from a peer node's certificate (§2.4) | None beyond FW-1 |
| FW-3 | A forwarding loop | The marker is never forwarded again; setting it grants nothing | None |
| FW-4 | A job submitted twice after an ambiguous failure | Fallback is local only when nothing was sent, or for `CreateJob` with its idempotency key, own or given | A non-idempotent call other than `CreateJob` returns `unavailable`, as after a database failover |
| FW-5 | The nodes near the primary overloaded: every call lands in its region | The target load is small. Forwarding is switched off by leaving `peer_ca_file` empty. The metric shows the shift. | Capacity planning: size the primary's region for the API |
| FW-6 | A stale choice after a failover | Corrected within a heartbeat; correct meanwhile | Slower calls for up to a heartbeat |
| FW-7 | X.509-SVID callers lose their identity if forwarded | They are not forwarded | They pay the round trips |
| FW-8 | A forwarded call outlives its deadline | The deadline goes with the call (gRPC); the receiver applies its own too | None |

## 4. Tests (written first)

| ID | Test | Kind |
| :--- | :--- | :--- |
| FW-T1 | `choose`: the 5 ms and quarter thresholds, dead, self, no URL and no RTT excluded, ties by ID | unit |
| FW-T2 | Two servers on one database. A's RTT is set to 50 ms, B's to 0.3 ms. `CreateJob`, `GetJob` and `ListJobs` through A, over gRPC and REST, run on B. Answers are those of B called directly: `202` with `Location`, `ETag`, `X-Request-Id`; `permission_denied` with the same `ErrorInfo` and message. Step-up's `WWW-Authenticate` is made by the REST side from the `ErrorInfo`, which comes back whole; `TestStepUp` covers the header. | integration |
| FW-T3 | A call with the marker is not forwarded | integration |
| FW-T4 | B down: A serves locally. `CreateJob` failing after it was sent: one job, by the key | integration |
| FW-T5 | A call with an X.509-SVID is not forwarded | integration |
| FW-T6 | `x-veho-forwarded-for` is believed from a peer's certificate only: not without one, nor with an unknown SAN or another CA | integration |
| FW-T7 | The configuration's checks | unit |
| FW-T8 | Fuzz: the metadata filter keeps no transport, pipe or forwarded-for key | fuzz |
| FW-T9 | Lab: `CreateJob` through `veho-b1` at a 50 ms round trip takes about 2R; then AC-24 phase A at 25 and 50 ms (overnight), published | lab |

## 5. Implemented (2026-11-27)

**Code:**
- `internal/api/forward.go`: `choose`, the forwarder, the `forward` interceptor between `deadline` and `unary`, and `fromPeer`.
- `internal/engine/heartbeat.go`: `dbRTT`, and the `api_url` and `db_rtt_us` columns written by the heartbeat.
- `cmd/veho/serve.go`: the configuration, and `api.Register` for the metric.
- The lab issues the nodes' certificates with `clientAuth` (again for older ones, at every deploy) and configures `advertise_url` and `peer_ca_file`.

**What reading the code showed:**
- **The response type.** An interceptor has no empty response to fill, so the forwarder finds the method's output type in the protobuf registry (`responseOf`).
- **Errors arrive in trailers.** An error alone comes back as trailers only, so "the peer answered" means headers *or* trailers. With headers only, a `permission_denied` from the peer would have become `unavailable`.

**Tests:**
- unit: FW-T1, FW-T5, FW-T7 and FW-T8 (fuzzed, about 800k executions);
- integration on the CI database: FW-T2, FW-T3, FW-T4 and FW-T6.

**FW-T9 in the lab** (leader db-b; `CreateJob` p50 through each node, 10 calls each after 2 of warm-up):

| WAN | b1, b2 (primary's region) | c1, c2 | a1, a2 |
| :--- | ---: | ---: | ---: |
| fiber1 (RTT a-b 19.2 ms, b-c 8.6 ms) | 17 ms | 27 ms | 38 ms |
| 50 ms on every link | 59 ms | 110 ms | 110 ms |

A far node pays one round trip more than a near one, as designed (2R + about 10 ms of local work). The first night measured p50 1170 ms at 50 ms through a far node.
