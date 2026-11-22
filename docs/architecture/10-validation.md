# Validation

## Tests

| Level | What | Where |
| :------------------ | :-------------------------------------------------------------- | :---------------------- |
| Unit | The policy's validation and verdict (with fuzzing), retries and back-off, the configuration, the client's request building, the error mapping | `go test ./...` |
| Integration | Against a real PostgreSQL cluster and S3 service: row-level security, the engine's workflows, adoption, failover mid-transfer, the API over gRPC and REST, the secrets, the command line | The lab, `test/lab/lab.sh test` |
| Plugin conformance | The driver contract, on `file` and on S3 (Garage), compiled in and as process plugins | The lab |
| DBOS regression | Every behaviour of DBOS veho relies on (transactional enqueue, resuming a dead executor's workflows, replay, partitioned queues, failover), re-run on every DBOS upgrade | `test/dbos` |
| Acceptance | The failure scenarios of the previous chapters, against six real nodes | The lab, `test/lab/lab.sh acceptance` |

## The lab

The lab is a disposable environment on one machine: an LXD virtual machine holding three "regions", each its own network, with the WAN between them emulated by the kernel's traffic control (one-way latency per pair of regions, jitter, loss) and partitions by firewall rules. Each region runs a PostgreSQL member under Patroni with an etcd member, an S3 service (Garage), a fault-injecting proxy in front of it (S3 errors, corrupted parts, delays), and two veho nodes under the shipped systemd unit. The WAN profile used for the scenarios reproduces the measured inter-region latencies (about 19 ms round trip between the farthest regions).

## Acceptance scenarios

The daytime scenarios, all against the six nodes over the emulated WAN:

| Scenario | Result |
| :---------------------------------------------- | :---------------------------------------------- |
| 1 GiB replicated from region A to B and C | Passes: digests equal, progress never goes down |
| `kill -9` of the executor mid-transfer | Passes: adopted 65 s after the kill, resumed from the last recorded part |
| The accepting node killed right after `202` | Passes |
| Both nodes of a region stopped mid-transfer | Passes (it found that a clean stop cancelled the node's work: fixed) |
| The primary's host killed mid-transfer | Passes: a new primary in about 40 s, every part recorded once |
| A region partitioned | In progress: it found that a submission in the partitioned region hung (fixed: now `unavailable`), and that connections and the housekeeping lock waited minutes for the unreachable member (fixed) |
| An executor frozen past its lease, then resumed | Passes: one correct object |
| A destination answering 503 for 3 minutes | Passes: completed after the outage |
| A destination answering 403 | Passes: `Permanent`, fail fast, retry after the fix |
| A part corrupted in flight | Passes: refused, rewritten, correct |
| The source overwritten mid-copy | Passes: never a mix of versions |
| A region's source down after one copy | Passes: the other copy completes when it returns |
| The same idempotency key on two nodes | Passes: one job |
| A tenant at its quota | Passes: `quota_exceeded`, running jobs unaffected |
| A cancel mid-transfer | Passes: no object, no upload left |
| Nine destinations, `min_copies` and spread | Passes |
| Audit events in each node's journal | Passes |
| No plaintext secret or unwrap key in a database dump | Passes |

Planned, as overnight campaigns: a soak of 50 concurrent jobs under random failures of nodes, the primary and the network for 8 hours; the state store's stability over the emulated WAN for 24 hours; a sweep of latencies and jitter; rolling upgrades with transfers in flight; 10 GiB objects and a 30-minute outage.
