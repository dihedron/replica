# veho: lab L3 and the acceptance suite (chunk C9)

**Status:** approved 2026-11-22; C9a and C9b implemented 2026-11-22 (`test/lab/lib/nodes.sh`, `test/lab/faultproxy`, `test/labtest`, `test/acceptance`); C9c next. Requirements: `veho-lab.md` §8 (L3), `veho-acceptance-matrix.md` §4 (AC-1 to AC-26), `veho-latency-envelope.md` §6 (AC-24), AGENTS.md rules 10 and 14 (lab and overnight runs).

## 1. Scope and steps

C9 puts veho itself in the lab and turns the acceptance matrix into tests. It comes in three steps, each its own commit series, each with this spec's tests passing before the next:

| Step | Builds | Runs |
| :--- | :--- | :--- |
| **C9a** Nodes | Six veho nodes (two per region) under systemd, deployed from the built binary; node control in `lab.sh`; a fault-injecting S3 proxy per region | `lab.sh selftest` grows the L3 checks (L3-T1 to L3-T5) |
| **C9b** Daytime suite | The Go helpers (§2.3), with the first tests that use them; `test/acceptance`: the scenarios that fit in a working session, each test named after its `AC-nn` | `lab.sh acceptance [-run R]`, under 45 minutes in total |
| **C9c** Long runs | AC-20 (rolling upgrade, about 20 minutes, daytime), and the campaigns: AC-17 (soak and chaos), AC-18 (WAN stability, 24 h), AC-24 (latency envelope), the long variants of AC-1 and AC-8 | `lab.sh campaign NAME`; each overnight campaign is a written plan, started only with the user's go-ahead (rule 14) |

Not in C9: AC-21 (already done in C6e: `test/dbos`), AC-22 (release verification, C11), a web UI (C10).

## 2. The nodes (C9a)

```
region-a 10.60.1.0/24   db-a .10   s3-a .20 (Garage :3900, proxy :3910)   veho-a1 .31   veho-a2 .32
region-b 10.60.2.0/24   db-b .10   s3-b .20 (Garage :3900, proxy :3910)   veho-b1 .31   veho-b2 .32
region-c 10.60.3.0/24   db-c .10   s3-c .20 (Garage :3900, proxy :3910)   veho-c1 .31   veho-c2 .32
```

- **Containers.** `veho-X1` and `veho-X2` in each region, Ubuntu 24.04 like the others, 1.5 GiB (`LAB_VEHO_MEMORY`). The nodes run `engine.worker_concurrency` 4 and `parts_in_flight` 2, so their parts need at most 512 MiB (`veho-engine.md`, memory); the defaults would need 4 GiB a node, more than the lab VM has for six. Created by `lab.sh nodes up`, not by `up`. L1 and L2 tests do not need them, and nodes would make the baseline snapshot stale on every rebuild. `lab.sh nodes down` removes them.
- **Deployment** (`lab.sh nodes deploy`) runs `make` and pushes `dist/…/veho` to `/usr/bin/veho` on every node. It then installs `packaging/veho.service` unchanged and restarts the nodes one at a time. The lab runs the shipped unit, so a unit that cannot run veho fails here. `veho serve` reads `/etc/veho/veho.yaml` by default, and the unit's `/etc/veho/veho.env` carries the DSNs that `veho.yaml` names with `dsn_env`. The unit's comments, which still describe the environment file as the whole configuration, are corrected in C9a.
- **Configuration**, written per node to `/etc/veho/veho.yaml`:
  - `node.id`: the container name. `node.region`: the region.
  - The three DSNs are multi-host over the three `db-X` region addresses, with `target_session_attrs=read-write` and `sslmode=verify-full` against the lab CA. Their passwords come from the state directory, into `/etc/veho/veho.env` (0640, root:veho).
  - Each node's database traffic crosses the emulated WAN; the tests' traffic to the API does not (§3).
- **The unwrap key** is one random 32-byte key per lab, kept in the state directory. Each node receives it as a systemd credential (`LoadCredential=` in a drop-in), the source `systemd-credential:unwrap-key`. This is the path the Setup Guide recommends. `lab.sh nodes up` runs `veho kek init` once, from the host, against the lab database.
- **TLS.** One server certificate per node from the lab CA, with SANs for its region address and the lab VM's address. The API listens on `:8443` on its region address. Metrics stay on their default port inside the node.
- **From the host**, each node's API is published on the lab VM through an LXD proxy device: port `LAB_API_PORT_BASE` (8440) + 2 × (region − 1) + index, so `veho-a1` is 8441 and `veho-c2` is 8446.
- **Audit** goes to the node's journal (syslog sink, `/dev/log`). The tests read it with `lab.sh logs veho-X1 veho`.
- **Destinations.** The endpoints the tests register point at the proxy (`http://10.60.X.20:3910`), not at Garage directly. With no fault set, the proxy only forwards.

### 2.1 Node control

| Command | Does |
| :--- | :--- |
| `nodes up`, `nodes down`, `nodes deploy` | §2 |
| `node start\|stop\|restart NAME` | `systemctl` in the node |
| `node kill NAME [--stay-down]` | `SIGKILL` to the service's main process, as a crash (`--kill-whom=main`: signalling the whole cgroup fails inside an LXD container). With `--stay-down`, the restart is blocked until `node start`, so another node must adopt the work. Without it, systemd restarts the node after `RestartSec`. |
| `node pause NAME` / `node resume NAME` | `SIGSTOP` / `SIGCONT` to the service (AC-7) |
| `db kill X` / `db start X` | stop the `db-X` container with `--force`, then start it again (AC-5) |
| `fault X MODE [args]` / `fault X clear` | the proxy of region X (§2.2) |

### 2.2 The fault-injecting proxy

`test/lab/faultproxy` is a small Go program (standard library only), built by `nodes deploy` and run under systemd in each `s3-X`. It forwards to Garage on `127.0.0.1:3900` and keeps the client's signed headers, so SigV4 still verifies. It listens on `:3910` (Garage's RPC has `:3901`). Its control API is on `127.0.0.1:3911`, reached only through `lxc exec`. For the tests on the host, each proxy is also published on the lab VM at `LAB_FAULT_PORT_BASE` (9010) + region: 9011 to 9013. Modes:

| Mode | Effect | For |
| :--- | :--- | :--- |
| `status CODE [--for D]` | answer CODE with an S3 XML error (503 `SlowDown`, 403 `AccessDenied`) to every request, or for D | AC-8, AC-9 |
| `corrupt [--count N]` | flip one byte in the body of the next N part uploads, keeping `Content-MD5` | AC-10 |
| `delay D` | add D before each response | AC-16 (a transfer long enough to cancel) |
| `bucket B` | apply the mode only to bucket B | a fault on one destination, not every destination in the region |

The proxy logs each request it changed, so a test can check that the fault was hit.

### 2.3 Go helpers

`test/labtest` gains `Lab` with `Nodes()`, `Node(name)`, `Kill`, `Pause`, `Resume`, `KillDB`, `Fault`, `WAN(profile)` and `Partition`. They run `lab.sh` and wait for the effect, for example a node's `/healthz` failing after `Kill`. `Client(node)` returns an `internal/client` connected through the published port with an admin key of a fresh tenant. Each test creates its own tenant, endpoints and buckets' prefixes, so tests do not see each other's jobs.

## 3. Where the tests run

The tests run on the host and reach the nodes through the published ports. That traffic does not cross the emulated WAN; the traffic that matters does: node to database, node to the S3 destinations of another region, and database replication. `veho-lab.md` §8 planned a test runner per region for the tests that must cross the WAN. No acceptance scenario needs one: acceptance latency (AC-24) is measured on the node, from the `veho_db_commit_seconds` histogram and from the job's `created_at`, not from the client. The runner is dropped (**amendment** of `veho-lab.md` §8).

## 4. The acceptance scenarios

Each scenario's test is named `TestAC<nn>…` and lists the requirement IDs it covers. "Covered" means an existing test already proves the expected result. The lab test then adds only what the lab changes: real regions, S3 and several nodes.

| ID | Where | How, in short |
| :--- | :--- | :--- |
| AC-1 | C9b; C9c long | 1 GiB (`LAB_AC1_SIZE`) from `s3-a` to B and C: `SUCCEEDED`, digests equal, progress never goes down. 10 GiB in a campaign (§6) |
| AC-2 | C9b | Kill the executor of a running transfer with `--stay-down`. Another node adopts within lease + 30 s. Parts committed before the kill are not copied again (the part checkpoints in the database). Final digest correct |
| AC-3 | C9b | Kill the node that answered `202`, at once. The job still settles `SUCCEEDED` |
| AC-4 | C9b | Stop both nodes of region A while jobs from `s3-a` run. Nodes in B and C finish them |
| AC-5 | C9b | `db kill` the Patroni leader during transfers. A new leader within 5 min; no job lost; transfers resume and succeed |
| AC-6 | C9b | Partition A. A node in A answers submits with `unavailable` (503 over REST). B and C keep going. Once their lease expires, A's in-flight steps are refused by the fence. After `wan heal`, one final object per destination |
| AC-7 | C9b | `node pause` the executor past its lease, then `node resume`. Its writes are fenced; one final object |
| AC-8 | C9b; C9c long | `fault status 503` on one destination for 3 min (`LAB_AC8_OUTAGE`): retries with back-off; completes after the fault clears. 30 min in a campaign |
| AC-9 | C9b, **amended** | `fault status 403` on one destination: that transfer `FAILED` with class `Permanent`. With `min_copies` 1 the other destination is unaffected and the job `SUCCEEDED`; with both required the job `FAILED` at once and the other transfer `CANCELLED` (fail fast, `veho-distribution-policy.md` DP-6, decided 2026-11-22), and `job retry` after `fault clear` succeeds |
| AC-10 | C9b | `fault corrupt`: the destination refuses the part (`BadDigest`, class `Integrity`); retried; final digest correct |
| AC-11 | covered | `internal/engine` (source changed mid-copy). Lab: one S3 variant (overwrite the source object mid-copy) |
| AC-12 | C9b, **amended** | FR-10's alternative source is not in v1: every transfer reads the original source (`veho-distribution-policy.md` §5, backlog 16). Amended expectation: with A's S3 down after C completed, B's transfer retries (`Transient`) and completes when A returns |
| AC-13 | covered | `internal/api` (idempotency). Lab: the same key submitted to two different nodes gives one job |
| AC-14 | covered | `internal/api` (tenant isolation, 404 on another tenant's objects); SEC-12 (a source must be a registered endpoint) is enforced by the API's validation. Lab: none |
| AC-15 | C9b | A tenant at its quota of active jobs: `resource_exhausted` (429); running jobs unaffected. This replaces the weak API test |
| AC-16 | C9b | `fault delay` slows a 256 MiB transfer; `job cancel` mid-copy: `CANCELLED`, no object under the final name, no multipart upload left (`ListMultipartUploads` on Garage) |
| AC-17 | C9c overnight | §6 |
| AC-18 | C9c overnight | §6 |
| AC-19 | C9b, engine | A poison pill cannot be produced through a real node without a test hook in the binary, which C9 does not add. An `internal/engine` integration test sets a transfer's workflow to `MAX_RECOVERY_ATTEMPTS_EXCEEDED` and checks the housekeeping result: transfer `FAILED`, job decided, audited. The code used `Unknown`, the matrix says `Internal`; C9b aligned the code with the matrix (`TestPoisonPill`) |
| AC-20 | C9c daytime, **done** 2026-11-22 (`TestAC20RollingUpgrade`; `engine.WorkflowVersion`, `lab.sh nodes deploy --workflow-version`) | Build the current version and a second build with a changed `ApplicationVersion`. Upgrade one node at a time with transfers in flight. Compatible version: the work moves on. Incompatible version: old executors drain their work before they stop; no workflow left `PENDING` |
| AC-23 | covered + C9b | `internal/policy` and `internal/engine` cover the verdicts. Lab: 9 destinations (3 buckets in each region), `min_copies` 7 with source in B; `min_copies` 5 with spread `region` 3 |
| AC-24 | C9c overnight | §6 |
| AC-25 | covered + C9b | v1 scope: AUDL-1 to AUDL-5 (`veho-audit.md`), covered; the tamper-evident log is backlog 32. One housekeeping pass at a time is HK-1 in `internal/engine`: the lock is held only for a pass, so the lab cannot see two nodes overlap from outside. Lab: audit events reach the journal of the node that handled the request |
| AC-26 | covered + C9b | `internal/secrets` covers the cryptography. Lab: a `pg_dump` of the database contains no canary secret and no unwrap key; a node without the credential refuses to start |

`lab.sh acceptance` runs C9b with `-p 1` and the WAN at `fiber1`, after `nodes deploy`. A failed test leaves its evidence in `test/lab/.evidence/<test>/`: the job as JSON, the nodes' journals for the test's time window, `patronictl list`, and the proxy's log. `lab.sh reset` restores the baseline. The nodes are not in the snapshot, so `nodes up` runs again after a reset.

## 5. Threat model

The lab is a test environment on one machine. Its threats concern the developer's host and the repository, not production.

| ID | Threat | Mitigation |
| :--- | :--- | :--- |
| L3-1 | The lab's unwrap key, database passwords or API keys reach the repository | All in `test/lab/.state/` (git-ignored, 0700); `make secrets` before every commit; keys are random per lab and never reused elsewhere |
| L3-2 | The nodes' APIs or the fault proxy reachable from outside the host | Published only on the lab's host-only bridge (10.254.0.0/24); the proxy's control port is on `127.0.0.1` in its container |
| L3-3 | The lab tests a different setup from the shipped one | The nodes run the shipped unit and binary, confined as in production; the unwrap key goes through the same systemd credential path |
| L3-4 | Destructive lab commands run against the wrong machine | Every command acts only inside the `veho-lab` project; `nodes down` removes only containers tagged `user.veho-lab=yes`, as `down` does (LAB-2) |

## 6. Long runs (C9c)

Each campaign records its results in `test/lab/.evidence/campaign-<name>-<date>/` and ends with a summary: pass or fail per check, and the measurements. Durations are estimates.

| Campaign | Duration | What runs | Pass |
| :--- | :--- | :--- | :--- |
| `soak` (AC-17) | 8 h | 50 concurrent jobs, kept at that level (sizes 64 MiB to 2 GiB, random policies over the 9 destinations). A chaos loop every 2 to 10 min at random: kill a node (half with `--stay-down`, for 5 min), pause a node past its lease, kill the Patroni leader, partition a region for 2 min, a 503 fault for 5 min | Every job `SUCCEEDED`; every object's digest equals the source's; no duplicate final write (the proxy's log); no transfer left `RUNNING` without a live executor |
| `wan24` (AC-18) | 24 h | Idle, plus 1 job a minute, profile `fiber1`, switched to `fiber2` and back every 2 h; then 1 h each at `rtt20`, `rtt30`, `rtt40` | No etcd leader change and no Patroni failover in the logs |
| `envelope` (AC-24) | 3 nights of about 10 h | 6 RTTs × 5 jitter levels = 30 cells of 1 h of light load, 10 cells a night | Per cell: elections, demotions, acceptance p50 and p99, primary-kill recovery and adoption time, written to `veho-latency-envelope.md` §6 |
| `long` (AC-1, AC-8) | 2 h | AC-1 at 10 GiB; AC-8 with a 30 min outage | As in §4 |

AC-1 at 10 GiB writes 30 GiB in the lab VM (the source and two copies). The VM's disk is 40 GiB by default, so `long` needs `LAB_LXD_DISK=80GiB`, which `lab.sh` grows with the VM stopped.

## 7. Tests of C9 itself

| ID | Test |
| :--- | :--- |
| L3-T1 | `nodes up` then `nodes deploy` on a running L2 lab: six nodes active; each answers on its published port with the lab CA; a second `nodes up` changes nothing |
| L3-T2 | Each node's database connection follows a `patronictl switchover` without a restart |
| L3-T3 | `node kill --stay-down` keeps the node down until `node start`; without `--stay-down`, systemd restarts it |
| L3-T4 | The proxy: no fault forwards a signed PUT and GET unchanged; `status 503 --for 5s` answers 503 and then forwards again; `corrupt` makes Garage refuse the part with `BadDigest`; `clear` restores forwarding |
| L3-T5 | A node without the unwrap-key credential fails to start, and its journal says why |
| L3-T6 | `faultproxy` unit tests (the modes, the bucket filter, the body corruption keeping the length) |

## 8. Decisions to review

1. Nodes outside the baseline snapshot, built by `nodes up` (§2).
2. No per-region test runner (§3, amends `veho-lab.md` §8).
3. AC-12 amended to v1's behaviour (§4).
4. AC-19 tested in the engine, not through a node, and its class aligned to `Internal` (§4).
5. Daytime variants of AC-1 (1 GiB) and AC-8 (3 min); the full figures in the `long` campaign (§6).
6. AC-24 over three nights of 10 cells each (§6). The alternative is one night with 20-minute cells, which reduces how much each cell's measurements can be trusted.
