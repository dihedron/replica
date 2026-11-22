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
| Drivers | The storage back ends behind one interface: `file` and `s3` compiled in, others as separate plugin processes |
| Audit | One JSON event per change of state, to the system log |
| Metrics | Prometheus, on a separate listener |

## Queues and concurrency

Workflows run from two DBOS queues, `jobs` and `transfer`. Each node runs at most `engine.worker_concurrency` workflows per queue (default 16), and each queue is partitioned by tenant, with at most `engine.tenant_concurrency` (default 4) per tenant on a node, so that one tenant cannot fill a node. Within a transfer, `engine.parts_in_flight` parts (default 4) are copied at once, and a node-wide budget, `engine.part_memory_mib`, bounds the parts held in memory across all its transfers.

## The API's contract

- **Asynchronous submission.** `POST /v1/jobs` answers `202 Accepted` with the job's ID once the job is committed; the client then polls `GET /v1/jobs/{id}`, long-polls it (`?wait=` with `If-None-Match`, answered when the job changes), or follows `WatchJob`, a server stream. Clients always call veho; veho never calls clients back.
- **Idempotency.** A submission with an idempotency key returns the first job if repeated, scoped by tenant; the same key with a different request is refused.
- **Stable errors.** Every error carries a code (`quota_exceeded`, `invalid_policy`, ...) and whether a retry may succeed, the same over gRPC (`ErrorInfo`) and REST (problem+json).
- **Bounded calls.** Every unary call has a server-side limit of 30 s (plus a long poll's wait); a call cut by it answers `unavailable`, so a node cut off from the state store refuses work instead of hanging.

## The client

The same binary is the command-line client, `veho <object> <verb>`, over gRPC or REST. Its REST requests are built from the protobuf's HTTP annotations, so the client cannot drift from the API. Credentials come from the environment, a file or a client certificate, never from the command line.
