# veho: the lab

This file specifies the lab: a disposable, self-contained test environment that simulates three regions on one machine, for the integration tests, the acceptance scenarios (`veho-acceptance-matrix.md` §4) and the overnight runs (AGENTS.md rules 10 and 14). It is part of `veho_specification.md`, and replaces the docker-compose environment that §4 of the acceptance matrix sketched.

Status: approved 2026-11-21; L1 and L2 built; L3's nodes and fault proxies built 2026-11-22 (`veho-lab-l3.md`, C9a).

## 1. Goals

- **One host, any host.** The lab runs on any Linux x86-64 machine with LXD 5.21 or later and KVM. It needs nothing else installed on the host (no PostgreSQL, no Docker, no `tc` rules), changes nothing on the host outside the LXD objects it creates, and removes them on `down`.
- **Three regions, the real control plane.** Each region is its own network; between them, the lab applies the measured one-way latencies of `veho-architecture.md` §3.3, or any other profile, jitter, loss and partitions. Each region runs one etcd member and one PostgreSQL member under Patroni, configured as the reference deployment is (synchronous quorum `ANY 1`).
- **Destinations in every region.** An S3 service per region, independent of the others, as the storage systems of different sites or clouds would be.
- **Reproducible.** Every download is pinned and verified; a baseline snapshot lets every suite start from the same empty state.
- **Built in steps.** The lab grows with the chunks (§8): the state store first, because C2 needs it.

Not goals: performance figures for production hardware (the lab shares one host's disk and CPU), clock-skew tests (the instances share the lab VM's clock), and region-level placement of the veho binaries before C6.

## 2. Architecture

```
host (LXD 5.21+, KVM)
└── project veho-lab, bridge veho-lab 10.254.0.0/24, pool veho-lab
    └── VM "lxd" 10.254.0.20  (Ubuntu 24.04; inner LXD; the WAN between the regions)
        ├── bridge region-a 10.60.1.0/24 ── db-a .10   s3-a .20   (veho-a1 .31  veho-a2 .32)
        ├── bridge region-b 10.60.2.0/24 ── db-b .10   s3-b .20   (veho-b1 .31  veho-b2 .32)
        └── bridge region-c 10.60.3.0/24 ── db-c .10   s3-c .20   (veho-c1 .31  veho-c2 .32)
```

- **The lab VM** (`lxd`) is a KVM virtual machine of the host's LXD, in its own project, network and storage pool, each tagged `user.veho-lab=yes`. It runs an inner LXD (snap, channel pinned, refreshes held), whose instances are the servers of the lab. The VM keeps every lab-specific kernel setting (queueing disciplines, nftables, forwarding) away from the host, and one disk snapshot of it captures the whole lab.
- **The inner instances are system containers.** Containers suffice for every scenario (processes can be killed, stopped with `SIGSTOP`, their services stopped and their networks cut); nested KVM is therefore not required. The preflight reports whether it is available, for a later scenario that needs a VM.
- **The regions** are three bridges of the inner LXD, each with its own subnet. Traffic inside a region stays on its bridge; traffic between regions is routed by the lab VM, which is where the WAN is emulated (§3). The bridges do not NAT: LXD's own NAT would masquerade the traffic between regions too, and the WAN classifies packets by their source address (found when building L1: only the reply direction was delayed). The lab VM masquerades only what leaves by its uplink, for package downloads.
- **Addresses are static** and listed in `lab.env`'s comments; instances address each other by IP (the inner LXD's DNS is per bridge and does not resolve across regions).
- **From the host**, the lab VM publishes the services through LXD proxy devices on its address (10.254.0.20), for the tests that run on the host (§6). This traffic does not cross the emulated WAN; a test that needs it runs inside a region (§8, L3).

| Instance | Runs | Memory | Published on 10.254.0.20 |
| :--- | :--- | :--- | :--- |
| `db-a`, `db-b`, `db-c` | etcd member, Patroni, PostgreSQL 16 | 1 GiB | PostgreSQL 5441, 5442, 5443; Patroni REST 8001, 8002, 8003 |
| `s3-a`, `s3-b`, `s3-c` | Garage, one node, its own cluster | 512 MiB | S3 API 9001, 9002, 9003 |
| `veho-R1`, `veho-R2` (L3) | veho, two per region | 1.5 GiB | API 8441 to 8446 (`veho-lab-l3.md` §2) |

Sizes: lab VM 8 vCPU, 20 GiB, 80 GiB disk by default (at 40 GiB the daytime suite filled it) (L1 and L2 use about 5 GiB of memory; the veho nodes of L3 add up to 9 GiB, which the VM's page cache gives up; the optional Nexus node 3 GiB). `lab.sh stop` then `start` applies a new size (a disk only grows).

## 3. The WAN

The lab VM routes between the region bridges; on each bridge's egress it classifies packets by their **source** region and delays them, so that each direction of each pair of regions has its own one-way figures.

- **Mechanism.** On bridge `region-X`: a `prio` root qdisc whose default band carries everything without delay (intra-region traffic never reaches this qdisc, since bridged frames do not pass the bridge device's egress; traffic from the lab VM itself, such as package downloads, is not delayed), plus one band per other region, each with a `netem` child (`delay D J distribution normal`, `loss L`), selected by a `u32` filter on the source subnet. The round trip between two regions is the sum of the two one-way delays.
- **Partitions** are nftables rules in a table of the lab's own (`inet veho_wan`), at the forward hook, that drop traffic between two subnets in both directions. Established connections then stall until their own timeouts, as on a real cut.
- **Profiles** (`lab.sh wan set PROFILE`), defined in `lab.env`:

| Profile | A–B | A–C | B–C | Use |
| :--- | :--- | :--- | :--- | :--- |
| `none` | 0 | 0 | 0 | functional tests |
| `fiber1` (default) | 9.6 | 9.6 | 4.3 | the measured links, fiber 1 (`veho-architecture.md` §3.3; the AZ makes no difference) |
| `fiber2` | 11.6 | 12.0 | 7.4 | the measured links, fiber 2 (the slower one) |
| `detour` | 9.6 | 13.9 | 4.3 | A–C through B after a fiber cut (9.6 + 4.3 ms one way; RTT 28 ms) |
| `rtt20`, `rtt30`, `rtt40` | 10, 15, 20 | same | same | the AC-18 baselines, uniform |

  Figures are one-way milliseconds, the same in both directions. `lab.sh wan set --link A-B=DELAY[/JITTER][/LOSS] ...` sets any link by hand; `--jitter` and `--loss` apply to all. AC-24's sweep (RTT 25 to 800 ms, jitter to ±50 %) uses these options.
- **Known limits.** `netem` jitter reorders packets once the jitter exceeds the gap between them; TCP absorbs this, and it is how a real path with variable delay behaves, but it is not a model of a fiber. The emulation applies one baseline per pair of regions to every connection at once: AC-18's "different baseline per session" is approximated by changing the profile during a run (`fiber1` to `fiber2` and back), which shifts every session at the same moment. A per-connection baseline (a flow-hash classifier that sends each connection to one of the two fibers) is left for the AC-18 chunk, if the approximation proves too gentle.

## 4. The state store

- **Packages.** PostgreSQL 16 and Patroni come from the PGDG repository for Ubuntu 24.04, whose signing key is fetched once and checked against the pinned fingerprint before it is trusted. **Ubuntu's own Patroni (3.2.2) cannot be used:** quorum-based synchronous replication (`synchronous_mode: quorum`, which manages `ANY 1 (...)`) arrived in Patroni 4.0, and with 3.2 the primary waits for one named standby, not the nearest. This is a constraint on the reference deployment too (backlog 27). etcd is the upstream release (3.6, decided here for backlog 28), verified against the release's `SHA256SUMS`.
- **etcd.** One member per region on `db-X`, client and peer URLs on the region address, the v3 API only; heartbeat and election timeout from `lab.env` (defaults 100 ms and 1000 ms, as decided in `veho-architecture.md` §3.3). Plain HTTP inside the lab (TLS for etcd belongs to the Setup Guide, not to what the lab tests).
- **Patroni.** One cluster (`scope: veho`) over the three members, `etcd3` DCS. Bootstrap DCS settings: `ttl` 30, `loop_wait` 10, `retry_timeout` 10 (from `lab.env`), `synchronous_mode: quorum`, `synchronous_node_count: 1`, `use_pg_rewind`, data checksums. Tag `failover_priority`: 2 for `db-b` and `db-c`, 1 for `db-a`, so that the primary prefers B or C (`veho-architecture.md` §3.3, reading); `db-b` bootstraps the cluster. The REST API is plain HTTP on port 8008.
- **PostgreSQL.** TLS 1.3 only for clients and replication, server certificates from the lab CA with the region address and the lab VM's address as SANs. `pg_hba`: the replication user from the region subnets; the veho roles from the region subnets and from the lab network (the proxies keep the client's address); the superuser over the Unix socket only. TCP keepalives as in ligo (a lost host's sessions end in about two minutes).
- **Database and roles.** The lab runs `packaging/postgres/roles.sql` (`veho-store.md` §2), as an operator would, with random passwords kept in the state directory; `lab.sh env` exports a DSN per role (`VEHO_TEST_DATABASE_URL` for the migrator, `VEHO_TEST_API_`, `_WORKER_` and `_DBOS_DATABASE_URL`). The objects in the database come from `veho migrate`: the lab does not duplicate the schema.

## 5. The S3 destinations

- **Garage** (Deuxfleurs, AGPL-3.0, S3-compatible, maintained and released regularly), a static musl binary, pinned by version and by a SHA-256 recorded in `lab.env` when the version is chosen: Garage publishes no checksums for its binaries, so the first download is checked by hand and the pin protects every later one.
- **One independent cluster per region:** `s3-X` is a single Garage node (`replication_factor = 1`, LMDB metadata), its S3 API on port 3900 with region name `region-x`, its admin API on 3903 with a random token. The three clusters know nothing of each other, as the storage systems of different sites do; the copies between them are veho's work.
- **Buckets and keys.** Each cluster has the buckets `source`, `dest-1`, `dest-2` and `dest-3` and one key `veho-lab` with read, write and owner rights on them. Nine destinations (three per region) are what AC-23 needs. The keys are kept in the state directory and printed by `lab.sh env`.
- **What Garage does not do** and what that means here: no object versioning, no object lock, no server-side encryption with customer keys, no bucket policies. veho's S3 driver must not depend on them; the conformance suite (`veho-plugins.md` §4) runs against Garage, and a feature it cannot exercise there is listed as untested. S3 over plain HTTP inside the lab; TLS for S3 is the destination's concern, tested with a TLS endpoint in the plugin chunk if needed.
- **Faults that need an S3 answer** (AC-8's 503, AC-9's 403, AC-10's corrupt part) come from a small fault-injecting HTTP proxy in front of Garage, built in L3 (§8); 403 can also be produced by revoking the key's rights (`garage bucket deny`).

## 6. Interface: `test/lab/lab.sh`

Settings in `test/lab/lab.env`, overridden by `lab.local.env` (git-ignored) and by `LAB_*` variables in the environment, as in ligo. State (CA, passwords, keys, `env.json`) in `test/lab/.state/` (git-ignored, mode 0700); downloads cached in `~/.cache/veho-lab`.

| Command | Does |
| :--- | :--- |
| `preflight` | checks the host: Linux x86-64, `/dev/kvm`, LXD 5.21+ reachable, that the lab's names and subnet are free or the lab's own, CPU, memory, disk, `jq`, `openssl`, `gpg`, Go as in `go.mod`; reports KVM nesting |
| `up` | builds the lab, or completes a partial one (idempotent), then takes the baseline snapshot when there is none |
| `down` | destroys the VM, its snapshots, the project, network, pool and the state, after confirmation |
| `status` | the instances, the Patroni cluster, the WAN profile, what `env.json` recorded |
| `env` | the environment of the tests on the host, as `export` lines: `VEHO_TEST_DATABASE_URL` and `VEHO_TEST_{API,WORKER,DBOS}_DATABASE_URL` (multi-host DSNs over the three published ports, `target_session_attrs=read-write`, `sslmode=verify-full`), and per region `VEHO_TEST_S3_X_ENDPOINT`, `_REGION`, `_ACCESS_KEY`, `_SECRET_KEY` |
| `test [-run R] [pkg...]` | `go test -tags integration -p 1` with that environment |
| `wan show`, `wan set PROFILE`, `wan set --link ...`, `wan partition X[-Y]`, `wan heal` | the WAN (§3) |
| `psql [--region X] [args]` | psql as `veho_migrator` over TLS (default: the primary) |
| `patronictl [args]`, `etcdctl [args]`, `garage X [args]` | the tools, run in the right instance |
| `snapshot`, `reset`, `stop`, `start` | the baseline: disk only, taken with the inner instances stopped, and a reset back to it |
| `acceptance [-run R] [--no-deploy]` | `nodes deploy`, then `go test -tags acceptance ./test/acceptance` with that environment and the WAN at `fiber1` (`veho-lab-l3.md` §4) |
| `nodes up\|down\|deploy`, `node start\|stop\|restart\|kill\|pause\|resume NAME`, `db kill\|start X`, `fault X ...` | L3: the veho nodes, their control, and the fault proxies (`veho-lab-l3.md` §2) |
| `ssh NAME [cmd]`, `logs NAME [unit]` | a shell or a command in the lab VM or an instance; cloud-init log or a unit's journal |

The tests take their environment only from `VEHO_TEST_*` variables and skip when they are unset, so the same integration tests run against the lab, a developer's own PostgreSQL or a CI service.

## 7. Requirements

- **LAB-1** `lab.sh up` on a host that passes the preflight builds the whole lab without manual steps; a second `up` changes nothing.
- **LAB-2** `lab.sh down` removes everything `up` created and nothing else: every LXD object it deletes carries `user.veho-lab=yes`.
- **LAB-3** Every download is pinned and verified (signature, published checksum, or a recorded SHA-256) before use.
- **LAB-4** With profile `fiber1`, the round trip measured between regions (ping between `db-X` instances) is within 1 ms of the profile's figures; inside a region it is below 1 ms.
- **LAB-5** `wan partition A` stops all traffic between A and the other regions, and `wan heal` restores it; neither touches traffic inside a region.
- **LAB-6** After `up`, Patroni reports one leader in B or C and two quorum standbys; `synchronous_standby_names` on the leader is `ANY 1 (...)` over the two others; a commit on the leader returns only after one of them has confirmed it.
- **LAB-7** `VEHO_TEST_DATABASE_URL` connects from the host to the primary over TLS with verification, and follows a `patronictl switchover`.
- **LAB-8** Each Garage cluster accepts a put and a get with the lab key on each of its buckets, through the published endpoint.
- **LAB-9** `lab.sh reset` returns the lab to the baseline (empty database `veho`, empty buckets, profile `fiber1`, no partition) in minutes, not a rebuild.
- **LAB-10** The lab's scripts pass `shellcheck` (run it from its container if it is not installed: `docker run --rm -v "$PWD:/mnt:ro" -w /mnt koalaman/shellcheck:v0.10.0 -x lab.sh lib/*.sh` in `test/lab`); `lab.sh selftest` checks LAB-4 to LAB-8 on a running lab.

## 8. Steps

| Step | With chunk | Builds | Requirements |
| :--- | :--- | :--- | :--- |
| **L1** | C2 | the lab VM, the regions, the WAN, the state store; preflight, snapshot, `env`, `selftest` | LAB-1 to LAB-7, LAB-9, LAB-10 |
| **L2** | C5 | Garage per region | LAB-8 |
| **L3** | C6 to C9 | the veho nodes (two per region, systemd, the built binary pushed), the fault-injecting S3 proxy (no test runner per region: `veho-lab-l3.md` §3), the acceptance suite AC-1 to AC-26, AC-18 and AC-24 as overnight campaigns | `veho-acceptance-matrix.md` §4, `veho-lab-l3.md` |

L1 and L2 are built together now: Garage is small, and the plugin chunk can start against it.

## 9. Copied from ligo

From ligo at commit `9f11da9`, `test/lab/`: the settings loader and output helpers (`lib/common.sh`), the LXD objects (`lib/lxd.sh`), the inner LXD (`lib/inner.sh`), the preflight (`lib/preflight.sh`), the verified downloads (`lib/fetch.sh`), the CA (`lib/pki.sh`) and the snapshot and reset (`lib/snapshot.sh`). Divergences: the names and the tag (`veho-lab`, `user.veho-lab`), three region bridges instead of one inner bridge, no scenarios, and the start-up waits for Patroni and Garage. `lib/wan.sh`, `lib/db.sh` and `lib/s3.sh` are new.
