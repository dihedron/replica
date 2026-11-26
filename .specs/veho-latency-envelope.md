# veho: latency and jitter envelope

This file states what inter-region latency and jitter the control plane can support, how it degrades as they grow, and how to tune it. It is part of `veho_specification.md`.

Status: draft, 2026-11-21, for review. **The figures are design estimates from the documented behaviour of etcd, Patroni, PostgreSQL and DBOS, not measurements.** AC-24 measures them in the lab and replaces the estimates; until then none of them is a support promise.

## 1. Position

- The test setup (`veho-architecture.md` §3.3: one-way 4.3 to 12 ms, RTT up to 24 ms, 39 ms on a detour) is a case in which everything must work perfectly. It is not the limit of the product.
- The product should support larger latencies where it can. The envelope is therefore a **set of documented profiles**: the operator picks the one that matches the worst round trip of the deployment, and the timeouts follow from it (§4). Beyond some latency automatic cross-region failover stops being sensible, and the product says so instead of flapping (§5).
- The system is CP: whatever the latency, it never accepts a job without the 2-of-3 quorum. Latency costs time (acceptance latency, failover detection), never consistency.

## 2. What depends on latency

Let R be the worst round trip between two control-plane members, R_q the round trip to the nearest synchronous standby (the quorum is `ANY 1`, so only the nearest one counts) and J the largest stall (a delay beyond the usual one, including a path change between sessions) that a message sees.

| Mechanism | Depends on | Effect of growth |
| :--- | :--- | :--- |
| Job acceptance (one commit) | R_q (the synchronous standby) | Acceptance latency is about R_q plus the local and remote flush (a few ms): linear in R_q. NFR-4: p99 < 1 s up to 150 ms; best effort beyond. |
| DBOS step and part checkpoints | R_q per commit | Each step is a commit. Part checkpoints are batched (`veho-job-lifecycle.md` §4, `copy_batch`), so the copy itself is not on the commit path; the checkpoint rate per transfer is at most 1/R_q per batch. |
| etcd (Raft) | R and J | A write needs a majority round trip. A follower that hears no heartbeat for the election timeout E starts an election: J above E - H causes spurious elections, and the cluster has no leader for about E + R. |
| Patroni (leader key in etcd) | the etcd outage, R | If etcd is unavailable for longer than the retry window, Patroni demotes the primary: a needless failover. Needed: `E + 2R <= retry_timeout` and `loop_wait + 2 x retry_timeout <= ttl`. |
| PostgreSQL streaming | R_q | Only metadata is written, so the WAL rate is low; `wal_sender_timeout` and `wal_receiver_timeout` (default 60 s) are far above any realistic R. |
| Audit log | the commit it shares, or nothing | A state change's audit entry shares its commit (no extra cost); buffered entries and the sealer are asynchronous (`veho-audit-housekeeping.md` §1.1), so latency only delays the sealing, never a request. |
| Secrets | the state store, and the key service if remote | Credentials are read from the state store (a local read). With a loaded key they are unwrapped in memory: no cross-region round trip. With a remote key (Vault Transit, HSM) each attempt makes one unwrap call, which must stay within the region (a cluster or HSM per region); an unwrap that crosses a region adds its round trip to the start of every attempt, and a Vault cluster that uses Raft needs the same timeout tuning as etcd (§4). |
| Executor heartbeat and lease | commit latency | Heartbeat 10 s, lease TTL 60 s, self-fence at TTL/2 (`veho-job-lifecycle.md` §6): safe while the commit p99 stays well below 10 s. A database that is unreachable for more than TTL/2 stops every transfer; they resume afterwards (a cost in time, not in correctness). |

Mean latency and jitter act differently. The mean sets the timeouts (§4); the jitter and stalls decide whether a given set of timeouts holds. A **different baseline per session** (paths that differ between connections) is a one-off shift at reconnect: size the timeouts on the worst baseline, not the mean.

## 3. How it degrades (estimates)

R is the worst round trip between members; the profile is the row whose range holds R.

| Profile | R (ms) | Acceptance (p99) | Failover detection | Notes |
| :--- | :--- | :--- | :--- | :--- |
| `regional` | up to 50 | R_q + a few ms | about 30 s (Patroni `ttl`) | etcd and Patroni defaults. The test setup is here. |
| `continental` | 50 to 150 | up to about 200 ms | about 30 to 60 s | Defaults hold to about 100 ms; from there heartbeat = R and the election timeout 10 x heartbeat; Patroni 60/15/15. NFR-4 holds. |
| `wide` | 150 to 400 | 0.2 to 0.5 s | 1 to 2 min | Heartbeat = R, election timeout 10 x R (1.5 to 4 s); Patroni `retry_timeout` of at least 40 x R, `ttl` of at least `loop_wait + 2 x retry_timeout`. NFR-4 relaxed above 150 ms. NFR-3 (5 minutes with failover) still holds. |
| `extreme` | 400 to 1000 | 0.4 to 1 s | 2 to 5 min or more | Automatic cross-region failover discouraged: NFR-3 is at risk. Use the fallback of `veho-architecture.md` §3.3 option 4 (automatic failover only inside a region, operator-approved promotion across regions). |
| beyond | above 1000 | above 1 s | not automatic | Acceptance still works (it is a synchronous commit) but costs more than a second; etcd allows an election timeout of 50 s at most. Not claimed: the measurement of AC-24 will say. |

Jitter. With the heartbeat H = R and the election timeout E = 10 H, a stall J up to about 9 H (to about 9 x R) is absorbed without an election. Persistent jitter above that, or loss of several heartbeats in a row, causes elections; each costs about E of write unavailability in etcd, which Patroni rides out while E + 2R fits in `retry_timeout`.

## 4. Tuning rules

From the worst round trip R (milliseconds) and the largest stall J:

| Parameter | Rule |
| :--- | :--- |
| etcd `heartbeat-interval` | about R, never below the default 100 ms |
| etcd `election-timeout` | at least 10 x heartbeat, and at least heartbeat + J; maximum 50 000 ms |
| Patroni `retry_timeout` | at least `E + 2R`, never below the default 10 s |
| Patroni `loop_wait` | at least 10 s |
| Patroni `ttl` | at least `loop_wait + 2 x retry_timeout`, with a margin |
| `synchronous_standby_names` | `ANY 1` over the two other regions (acceptance waits for the nearest) |
| Executor lease TTL | at least 6 x heartbeat; raise it if the commit p99 exceeds 5 s |

A rule of thumb for the operator: **tune on the worst figure, never on the mean**, and add the largest stall seen in the baseline change between sessions. A wrong guess in the low direction causes flapping; in the high direction it only slows failover detection.

## 5. Beyond the envelope

When R or J make automatic cross-region failover unsafe, the product degrades in this order, each step a documented configuration:

1. Raise the timeouts (§4): slower detection, no flapping.
2. Move the third etcd member (a witness is enough) next to the closest pair.
3. Keep automatic failover inside the primary's region and make cross-region promotion an operator decision (`veho-architecture.md` §3.3 option 4). Acceptance and RPO are unchanged: the synchronous standby is still in another region.

Never: accept a job on fewer than the quorum, or promote automatically across regions on a measurement the product cannot trust.

## 6. Validation: AC-24

A lab sweep (the lab's emulated WAN between the three regions, `veho-lab.md` §3) measures this file's estimates:

- RTT: 25, 50, 100, 200, 400 and 800 ms;
- jitter: none, +/- 10 %, +/- 25 %, +/- 50 % of the RTT, plus a baseline shift at reconnect;
- for each cell: the profile's timeouts, 1 hour of light load, recording spurious etcd elections, Patroni demotions, acceptance latency (p50, p99), the time to detect and recover from a killed primary, and the time a transfer takes to be adopted.

Pass: no spurious election or demotion in every cell the profile claims to support; a cell outside the claim is documented with what happened. The result replaces §3 with measurements and is published in the Operator's Guide. It is an overnight campaign (AGENTS.md rule 14): run over several nights, as `veho-latency-campaign.md` specifies (48 cells of an hour, with packet loss added).

## 7. Requirements

- **LAT-1** The product documents the maximum latency and jitter it supports per profile and how it degrades above it, from measurements (AC-24).
- **LAT-2** The timeouts of etcd, Patroni and the lease follow from the worst round trip by the rules of §4; the Operator's Guide gives them per profile.
- **LAT-3** Under no latency does the product accept a job without the quorum (NFR-1).
- **LAT-4** Beyond the envelope the product degrades in the order of §5, never by flapping.
