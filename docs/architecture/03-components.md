# The components of a node

One process, `veho serve`, holds every component.

| Component | Does |
| :------------------- | :---------------------------------------------------------------------- |
| API | gRPC and REST on one TLS 1.3 port. REST is generated from the same protobuf definitions (grpc-gateway) and goes through the same interceptors, so the two cannot disagree. An OpenAPI 3.1 document describes the REST API |
| Interceptors | In order: request ID; rate limit on refused credentials per client address; authentication (API key, OIDC token, SPIFFE X.509- or JWT-SVID); the role each method requires; rate limits per tenant and principal; the audit context; the mapping of errors to stable codes |
| Engine | Submission (the job, its transfers and its workflow in one transaction), the policy's verdict under the job's lock, cancel, retry, status, and the administration of tenants, endpoints, policies, keys and entitlements |
| Workflows | Two durable workflows on DBOS queues: `ReplicateJob` (resolve the source, start a `Transfer` per destination) and `Transfer` (copy one object to one destination, part by part) |
| Heartbeat | Records the node in the executors' table every 10 s and renews its leases; a node that cannot for half its lease fences itself |
| Housekeeping | Every 30 s on one node: adoption of dead nodes' work, the zombie kinds, deadlines, retention |
| Secrets | Envelope encryption of the tenants' credentials, unwrapped in memory only when a transfer opens an endpoint |
| Drivers | The storage back ends behind one interface: `file` and `http` compiled in, the others (`s3`, `sftp`, `ftp`, `glance`, `swift`, `nexus`) as separate plugin processes |
| Endpoint checks | Each executor answers, at its heartbeat, the network checks of the endpoints: it probes the flows the driver reported (resolved here, each address through the driver's dial guard, or through its forward proxy) with netcheck, and records its results |
| Audit | One JSON event per change of state, to the system log |
| Metrics | Prometheus, on a separate listener |

## Queues and concurrency

Workflows run from two DBOS queues, `jobs` and `transfer`. Each node runs at most `engine.worker_concurrency` workflows per queue (default 16), and each queue is partitioned by tenant, with at most `engine.tenant_concurrency` (default 4) per tenant on a node, so that one tenant cannot fill a node. Within a transfer, `engine.parts_in_flight` parts (default 4) are copied at once, and a node-wide budget, `engine.part_memory_mib`, bounds the parts held in memory across all its transfers.

## The API's contract

- **Asynchronous submission.** `POST /v1/jobs` answers `202 Accepted` with the job's ID once the job is committed; the client then polls `GET /v1/jobs/{id}`, long-polls it (`?wait=` with `If-None-Match`, answered when the job changes), or follows `WatchJob`, a server stream. Clients always call veho; veho calls a client back only through a webhook the tenant registered, which is off unless the operator enables it (*Webhooks*, below).
- **Idempotency.** A submission with an idempotency key returns the first job if repeated, scoped by tenant; the same key with a different request is refused.
- **Stable errors.** Every error carries a code (`quota_exceeded`, `invalid_policy`, ...) and whether a retry may succeed, the same over gRPC (`ErrorInfo`) and REST (problem+json).
- **Bounded calls.** Every unary call has a server-side limit of 30 s (plus a long poll's wait); a call cut by it answers `unavailable`, so a node cut off from the state store refuses work instead of hanging.

## Network checks of the endpoints

An endpoint works from the node that created it, but a transfer may run on a node of another region, behind another firewall. `veho endpoint check` (`POST /v1/endpoints/{name}:check`), and every endpoint create and update, record a check: the API node opens the endpoint with its credentials and asks the driver for its **flows** (each connection a client makes: an S3 endpoint; Keystone and the service of its catalog; an SFTP server, with SSH's handshake; an FTP server's control connection, and its data connections, which are not probed), and lists the executors alive. Each executor answers at its heartbeat, through the one function that crosses tenants (`pending_endpoint_checks`): it resolves every host itself, probes at most four addresses each, checks every address with the driver's dial guard first (a refused one is skipped and not named in the result), and dials the address, so a resolution cannot change between the check and the probe; a flow through the forward proxy is probed as a `CONNECT` and the TLS handshake through it. A check is complete when every node has answered, expired after two minutes; one runs per endpoint at a time, the last ten are kept, and only the tenant's administrators read them. No credential reaches the executors or the results.

## The client

The same binary is the command-line client, `veho <object> <verb>`, over gRPC or REST. Its REST requests are built from the protobuf's HTTP annotations, so the client cannot drift from the API. Credentials come from the environment, a file or a client certificate, never from the command line.

## Webhooks

Polling and long polling stay the primary way to follow a job; a webhook is an optional hint to look. A tenant administrator registers HTTPS URLs, each with the events it wants (`job.succeeded`, `job.failed`, `job.cancelled`), and veho posts a small signed document to each when one of the tenant's jobs reaches a terminal state.

The event is written to an **outbox** table by a trigger, in the very transaction that moves the job to its terminal state, whichever code path does it: a job never settles without its event, and a delivery never holds up a job. One dispatcher at a time (an advisory lock, as for housekeeping) takes the oldest event of each webhook when it is due, posts it, and records the outcome. Delivery is at least once: a crash between the post and the record delivers again, with the same event ID, which receivers use to deduplicate. A failed delivery waits 30 s, then doubles up to an hour; after 12 attempts the event is abandoned, and after 12 consecutive failures the webhook is switched off. A stuck webhook delays only itself.

A webhook's secret is in the secret store, under a reserved name, and shown once. Each request carries `Veho-Signature: t=<seconds>,v1=<HMAC-SHA256 of "t.body">`; during a rotation the old secret signs too, for 24 hours.

The URL is the tenant's, so it is guarded (SEC-12, SEC-13): `https` only, no user information, no redirect, no proxy, and the address actually dialled is checked after the name is resolved, so a name that resolves to, or is rebound to, a loopback, private, link-local, multicast or metadata address is refused, unless the operator listed its range (`notifications.allow_cidrs`; the metadata addresses never). A tenant sees only a class of failure (`status`, `forbidden`, `timeout`, `connect`, `tls`), never an address or a body.
