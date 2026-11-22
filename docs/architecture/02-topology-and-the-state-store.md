# Topology and the state store

![The reference deployment: three regions, symmetric nodes, one state store.](topology.pdf){width=90%}

## Symmetric, stateless nodes

Every `veho serve` node runs the same code: the API and the executor. Any node can accept a job, and any node can execute any transfer. Nodes keep no durable state of their own: everything is in PostgreSQL, and in the storage systems. A node is therefore replaced by starting another; there is no leader election among nodes and nothing to fail over between them.

The only singleton activity, housekeeping, is guarded by a PostgreSQL advisory lock, and every one of its tasks is idempotent: the lock saves duplicate work, it is not needed for correctness.

## One PostgreSQL cluster across three regions

- **One writable primary**, and a standby in each of the other two regions.
- **Synchronous quorum commit**: `synchronous_standby_names = 'ANY 1 (b, c)'` and `synchronous_commit = on`. A commit returns only once at least one other region has flushed it. Losing any one region, the primary's included, loses no acknowledged commit.
- **Failover by Patroni**, with etcd (one member per region) as its store. A new primary needs an etcd majority and must be one of the synchronous standbys, so no committed transaction is lost and no two primaries accept writes.
- **Consistency over availability.** A region cut off from the other two cannot commit, so it cannot accept jobs; the majority side fails over if needed and continues. This is deliberate: a job that is acknowledged is never lost and never forked.

The cost is one inter-region round trip per commit. veho keeps commits few: it records progress per batch of parts, never per read, and a job's acceptance is a single transaction. At the target load (tens of transfers, a commit every few seconds per transfer) the cost is negligible; submission latency is about one round trip plus local work.

veho checks this setup at every start: a node refuses to run against a standby, a pooler, a cluster without synchronous commit, or one whose standbys are not a quorum over other regions.

## Consensus over the WAN

etcd's Raft is sensitive to latency and jitter between its members. It does not affect commit latency (commits wait for the PostgreSQL standby, not for etcd); it affects failure detection: with default timeouts over a long or jittery WAN, spurious elections make Patroni demote a healthy primary.

The reference deployment measured 30 ms ± 10 ms between regions. etcd's heartbeat (100 ms) and election timeout (1000 ms) are tuned for round trips up to about 50 ms, with Patroni's `ttl` 30 s, `loop_wait` 10 s and `retry_timeout` 10 s. A 24-hour stability run over the emulated WAN (acceptance scenario AC-18) and a sweep of latencies and jitter (AC-24) are planned to confirm or correct these figures. If they show instability, the fallback is an operator-approved failover across regions, with automatic failover only within a region.

## Connections

Nodes connect directly to the primary, never through a transaction-mode pooler: DBOS keeps a `LISTEN` session, and session state matters. Each DSN lists all three members with `target_session_attrs=read-write`, so that a client finds the new primary after a failover, and each host is dialled with a 5 s timeout, so that a member that does not answer (a partitioned region) costs seconds, not the operating system's two minutes. The timeout is reported so that DBOS retries the connection rather than giving up on its workflows.

## Why PostgreSQL and DBOS

veho needs durable workflows: steps that are checkpointed and replayed after a crash, durable queues, and idempotency. DBOS Transact gives these as a library over PostgreSQL, with no other service to run. It also lets the API enqueue a job's workflow in the same transaction that inserts the job (application tables live in DBOS's database), so a job and its workflow commit together or not at all.

The alternatives considered were Temporal (a strong fit, but a second stateful cluster to operate across three regions), a hand-written outbox with leases on PostgreSQL (which re-implements DBOS), and distributed SQL databases (which pay the same inter-region quorum cost, with a less familiar operation). veho uses DBOS's open-source features only.
