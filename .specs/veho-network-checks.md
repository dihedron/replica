# veho: the network checks of the endpoints

This file specifies backlog item 40: veho records the network flows each endpoint needs and checks them from every executor node, with netcheck, as ligo's client SDK does (`ligo/.specs/ligo-network-checks.md`, `ligo/pkg/client/flowcheck.go`). It is part of `veho_specification.md`; it extends the driver contract (`veho-plugins.md`), the endpoint API (`veho-api.md`) and the command line (`veho-cli.md`).

Status: approved, 2026-11-25, as proposed: the executors answer at their heartbeat (decision 3; LISTEN/NOTIFY was the alternative), and periodic checks go to the backlog (item 41).

## 1. Why

An endpoint works from the node that created it (`endpoint test`, which runs on the API node), yet a transfer fails later on a node in another region whose firewall does not let it reach the storage system. The flows an endpoint needs (each executor to the storage system: Keystone and the services of its catalog, an S3 endpoint, an FTP server and its data connections) are known from its configuration; checking them from every node shows a missing rule before a transfer fails there.

## 2. Decisions (approved 2026-11-25)

| # | Question | Decision |
| :--- | :--- | :--- |
| 1 | Who says what the flows are | The driver, through a new optional interface `plugin.FlowLister`: `Flows(ctx, config, credentials) ([]Flow, error)`, forwarded over gRPC for plugins. A driver without it has no flows (`file`). |
| 2 | Where the flows are found | Once per check, on the API node that receives the request, with the endpoint's credentials (an OpenStack catalog needs a token); the flows are stored with the check. The executors only probe: they never authenticate nor send credentials. |
| 3 | Who probes | Every executor alive when the check is created (`veho.executors`, seen within `lease_ttl`). Each executor polls for checks it has not answered, at its heartbeat, and writes its results. DBOS cannot address one executor (no `Sharding` in the open-source edition), so the checks are rows, not workflows. |
| 4 | When | On demand (`veho endpoint check NAME`, `POST /v1/endpoints/{name}:check`), and after every `endpoint create` and `update`, which do not wait for it. Not periodic (§9). |
| 5 | What a probe is | netcheck's core package (`github.com/dihedron/netcheck/v2/pkg/netcheck`, v2.1.0, tagged): `tcp`; `tls` with the flow's server name and the endpoint's `ca_cert`, or the system roots; `ssh` (the server's banner) for SFTP. One probe per resolved address, at most 4 addresses per host. |
| 6 | The dial guard | Each executor resolves the host itself and checks every address with the driver's guard (`allow_cidrs` of `drivers[].settings`, the dial guard's rules): a refused address is `Skipped`, never probed (SRC-3). The probe dials the address, not the name, so a resolution cannot change between the check and the probe. |
| 7 | What a flow that cannot be probed is | `Skipped` with its reason, never a false pass or fail: FTP's passive data ports (chosen by the server per connection), a port range, a UDP flow (no storage system of veho uses UDP). |
| 8 | What is kept | The last 10 checks per endpoint, each with its flows and its results per node and flow; older ones are deleted by housekeeping. |
| 9 | Relation to `endpoint test` | Unchanged and separate: `test` uses the credentials from one node (open, stat, write, delete); `check` reaches the network from every node, without credentials. |

## 3. Flows

```go
type Flow struct {
	Purpose    string // what it is for: "keystone", "glance", "swift", "s3", "ftp control", "ftp data", ...
	Host       string // a name or an address
	Port       int    // 0 with a range (PortHigh) or when unknown
	PortHigh   int
	Protocol   string // "tcp"
	TLS        bool
	ServerName string // when TLS: the name the certificate must carry
	Handshake  string // "": plain TCP or TLS; "ssh"
}
```

What each driver reports:

| Driver | Flows |
| :--- | :--- |
| `file` | none |
| `s3` | the endpoint's host and port, TLS unless `http` |
| `http` | the base URL's host and port, TLS for `https` |
| `sftp` | the host and port, the `ssh` handshake |
| `ftp` | the control connection (TLS when implicit FTPS); the data connections as a flow `Skipped`: "passive ports chosen by the server" |
| `glance`, `swift` | Keystone's auth URL, then every endpoint of the catalog the driver would use (the service, its region and interface), each with TLS and its host as server name |
| `nexus` | the base URL's host and port |

A driver returns at most 64 flows; more is an error of the check (NC-3).

## 4. Data model

```sql
CREATE TABLE veho.endpoint_checks (
	id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
	tenant_id   uuid NOT NULL,
	endpoint_id uuid NOT NULL,
	flows       jsonb NOT NULL,          -- the flows of §3, or [] with error
	error       text,                    -- why the flows could not be found
	nodes       text[] NOT NULL,         -- the executors expected to answer
	created_at  timestamptz NOT NULL DEFAULT now(),
	FOREIGN KEY (tenant_id, endpoint_id) REFERENCES veho.endpoints (tenant_id, id) ON DELETE CASCADE
);
CREATE TABLE veho.endpoint_check_results (
	check_id    uuid NOT NULL REFERENCES veho.endpoint_checks (id) ON DELETE CASCADE,
	tenant_id   uuid NOT NULL,
	node_id     text NOT NULL,
	region      text NOT NULL,
	results     jsonb NOT NULL,          -- per flow and address: status, reason, latency
	checked_at  timestamptz NOT NULL DEFAULT now(),
	PRIMARY KEY (check_id, node_id)
);
```

Both under the tenants' row-level security, as `veho.endpoints`. A check is **complete** when every node of `nodes` has answered, or **expired** two minutes after its creation: the nodes that did not answer are listed as such. One check runs per endpoint at a time: a request while one is running returns that one.

## 5. API and command line

- `POST /v1/endpoints/{name}:check` → `EndpointCheck` (`id`, `state`: running, complete, expired; `flows`; `nodes`, each with its region and its results; `error`). `GET /v1/endpoints/{name}/checks` lists the last checks, `GET /v1/endpoints/{name}/checks/{id}` reads one. Permission: as `TestEndpoint`.
- `veho endpoint check NAME [--wait]`: starts a check, with `--wait` polls until it is complete or expired, and prints a table (node, region, flow, address, status, reason); the exit status is 1 when a flow is `Unreachable` from some node.
- `veho endpoint show NAME` shows the last check's summary (complete or not, how many unreachable).
- The audit log records each check (`ENDPOINT_CHECKED`, its counts; no addresses).

## 6. Threat model

| ID | Threat | Mitigation |
| :--- | :--- | :--- |
| NC-1 | **Probing as SSRF**: an endpoint's configuration names an internal host, and the checks probe it from every node, telling the tenant which internal ports are open. | Each address is checked with the driver's dial guard before it is probed (decision 6); a refused one is `Skipped` and its reason does not say whether it would have answered. The probe dials the checked address, not the name (no DNS rebinding). |
| NC-2 | **The catalog points elsewhere**: an OpenStack catalog lists hosts the tenant chose. | Its endpoints go through the same guard; the driver already checks that they are https (OS-1). |
| NC-3 | **Amplification**: many checks, many flows, many addresses, many nodes. | At most 64 flows per endpoint, 4 addresses per host, one check in flight per endpoint, the API's rate limits; a probe is a connection and a handshake, nothing is sent after it; 5 s per attempt, 2 attempts. |
| NC-4 | **Credentials exposed by a check.** | The flows are found on the API node with the credentials, which never reach the executors nor the results; a probe sends none. Errors are netcheck's scrubbed messages. |
| NC-5 | **What the results disclose**: the executors' ids and regions, the addresses of the storage system. | Visible only to the tenant (row-level security) and with the permission of `TestEndpoint`; the audit log records counts, not addresses. |
| NC-6 | **Supply chain**: the netcheck module. | A tagged release, only its core package, `make vuln` on every change (as ligo's T-N4). |

## 7. Tests (written first)

| ID | Test | Kind |
| :--- | :--- | :--- |
| NC-T1 | Each driver's `Flows` from its configurations (S3 http and https, FTP and FTPS, SFTP, Nexus, http); Glance and Swift from a fake catalog; at most 64; over gRPC for a plugin | unit |
| NC-T2 | The probe of a flow: reachable and unreachable (a listener and a closed port), TLS with the endpoint's CA and a wrong server name, `ssh`, a refused address `Skipped` and not dialled, a range and FTP's data `Skipped`, at most 4 addresses | unit |
| NC-T3 | The check's life: created with the alive nodes, answered by each, complete; expired with a node that never answers; one in flight per endpoint; the last 10 kept | integration |
| NC-T4 | The API and `endpoint check --wait`: the table, the exit status; a check after `endpoint create` | integration |
| NC-T5 | In the lab: an endpoint on Garage checked from the three regions' nodes, reachable; then a firewall rule in one region (an `nft` rule on one node) makes it unreachable from that node only; removed, reachable again | lab |
| NC-T6 | Fuzz the flow-to-probe mapping; a benchmark of 64 flows | fuzz, bench |

## 8. Documentation

The Operator's Guide (endpoint checks, what `Skipped` means, the firewall rules each driver needs), the Architecture and Design Guide (the checks as rows, the threat rows), `veho-threat-model.md` (NC-1 to NC-6), `veho-api.md`, `veho-cli.md`.

## 9. Out of scope

Periodic checks (a background check of every endpoint: backlog item 41); checks of the flows a destination opens itself (Glance's `web-download` fetches its source from Glance's network, not veho's); ICMP; the flows of the state store and of the notifications (webhooks), which are the platform's, not an endpoint's.
