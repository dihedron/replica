# Incident procedures

Each procedure says what veho does by itself, what you see, and what to do.

## A node dies

**veho does**: the node's heartbeat stops. Once its last heartbeat is older than the lease (60 s), the next housekeeping pass (within 30 s) resumes its workflows on the live nodes, which continue each transfer from its last recorded part. Measured in the lab: about 65 s from a `kill -9` to the takeover.

**You see**: `veho_adoptions_total` rises on another node; a `SYSTEM_ZOMBIE_ADOPTED` audit event names the dead node.

**Do**: restart or replace the node when convenient. Nothing to repair. If the same transfer keeps killing nodes, see *A poison pill*.

## A node is frozen, then comes back

A paused VM, a stalled host, a node cut off for a while.

**veho does**: a node that cannot write its heartbeat for half its lease stops its own work. Another node adopts its transfers after the lease. When the frozen node comes back, its writes carry a stale **fence** and are refused: it cannot record a part, or end a transfer, it no longer owns. Its in-flight part may still reach the destination; part writes are idempotent, so the object is not affected.

**Do**: nothing, unless it recurs: then look at the host.

## A region is cut off

**veho does**: the state store needs two regions to commit. In the isolated region, nodes cannot commit: submissions there answer `unavailable` (HTTP 503, retryable) within 30 s, and the nodes fence themselves. On the majority side, Patroni elects a new primary if the old one was in the isolated region (about 30 to 60 s), and the nodes adopt the isolated region's transfers after the lease. Transfers whose source is in the isolated region retry until it comes back.

**You see**: `veho_heartbeat_age_seconds` rising on the isolated region's nodes; `unavailable` errors there; adoptions on the others.

**Do**: point clients at the nodes of the healthy regions (a load balancer using `/readyz` does this). When the region is back, its nodes resume by themselves.

## The primary's host or region is lost

**veho does**: Patroni promotes a quorum standby; no acknowledged commit is lost, because each was flushed by another region first. The nodes reconnect through their multi-host DSN to the new primary; copies in flight retry their current step. Measured in the lab: a new primary 40 s after the primary's host was killed, and every transfer finished.

**Do**: rebuild the lost member and let Patroni re-join it as a standby. Check that `synchronous_standby_names` is back to `ANY 1` over two standbys.

## Two regions are lost

**veho does**: nothing can commit. Every node fences itself; submissions answer `unavailable`.

**Do**: restore a second region. Do not force a single member to be primary unless you accept losing what it may not have (`patronictl` would need to override the quorum): veho's guarantees assume a quorum.

## A destination is down

**veho does**: retries with exponential back-off and jitter (by default from 2 s up to 15 min, within the policy and the tenant's bounds), until the job's deadline. A `503` or a timeout is `Transient`.

**Do**: nothing on veho's side; fix the destination. Retries continue on their own.

## A destination refuses

`403`, a missing bucket, revoked credentials.

**veho does**: the transfer fails `Permanent` at once. If the policy can no longer be met, the job fails and its other transfers are cancelled; otherwise the job goes on.

**You see**: `veho_transfer_attempts_total{class="Permanent"}`; the transfer's `last_error` in `veho job show`.

**Do**: fix the destination (credentials: `veho secret set`; configuration: `veho endpoint update`), then `veho job retry <id>`.

## A poison pill

A transfer that crashes the node running it, on every attempt.

**veho does**: housekeeping counts adoptions per transfer; after 20 it fails the transfer with class `Internal`, and decides the job. A workflow that DBOS itself parks after too many recoveries is treated the same.

**Do**: collect the nodes' logs around the crashes and report it, with the transfer's ID: it is a bug.

## A node cannot unwrap the key

**veho does**: the node refuses to start, saying why (no credential, a key of the wrong size or ownership, a key that does not unwrap the active KEK).

**Do**: give the node the same unwrap key as the others (*Setup Guide*, *The unwrap key*). If the key is lost on every node and offline, the stored secrets are lost: delete them and have the tenants set them again.

## The system log does not take audit events

**veho does**: each write waits at most 250 ms; on a timeout the sink pauses 5 s and drops events meanwhile, counting them. The action itself is never failed by the audit.

**You see**: `veho_audit_dropped_total` rising.

**Do**: fix rsyslog or journald on the node.
