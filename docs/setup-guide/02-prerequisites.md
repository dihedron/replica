# Prerequisites

## Regions and network

- **Three regions** (sites, availability zones far enough apart to fail independently). Two regions are not enough: the state store needs a majority to elect a primary and to commit.
- **Round-trip time** between any two regions of at most 150 ms; the reference deployment measured 30 ms ± 10 ms. Above 150 ms veho still works, but submission latency rises and the failover timings need tuning (*Architecture and Design*, the state store).
- **Clocks** synchronised with NTP (chrony) on every host. Leases are compared on the database's clock, not the nodes', but logs and audit events carry each host's time.

## Hosts

| Role | Operating system | Per host |
| :---------------- | :---------------------------------------- | :-------------------------------------------------- |
| Database member (one per region) | Ubuntu 24.04 LTS or later, or Rocky Linux / AlmaLinux 9 | PostgreSQL 16, Patroni 4.0 or later, etcd 3.6; disk for the database and its WAL |
| veho node (two or more per region) | The same | 1.5 GiB of memory or more (below); systemd |

On OpenStack, keep the two nodes of a region on different hypervisors (an anti-affinity server group).

### Sizing a node

A node holds parts of objects in memory while it copies them (the S3 driver reads each part fully, to send its MD5). Its memory for parts is bounded by `engine.part_memory_mib` (default 1 GiB): a part waits for room before it is read. Give a node about **`part_memory_mib` + 200 MiB**, plus what the host itself needs.

The part size is `engine.part_size_mib` (default 64 MiB) until an object needs more parts than its destination accepts: S3 takes at most 10,000 parts, so above 640 GiB the parts grow, up to 512 MiB for a 5 TiB object. A part larger than the whole budget takes all of it and runs alone, so even such objects stay within the bound; they just copy one part at a time.

## Ports

| From | To | Port | What |
| :------------------ | :------------------ | :---------- | :---------------------------------------- |
| Clients | Every node | 8443/tcp (`api.listen`) | The API: gRPC and REST over TLS 1.3 |
| Every node | Every database member | 5432/tcp | PostgreSQL, over TLS, verified |
| Database members | Each other | 5432/tcp, 8008/tcp, 2379-2380/tcp | Replication, Patroni's REST API, etcd |
| Every node | The storage systems | Theirs (443 for S3 over TLS) | The copies |
| Prometheus | Every node | 9464/tcp (`metrics.listen`, on 127.0.0.1 by default) | Metrics |

## Certificates

- A server certificate for each node's API, with its host name or address as a subject alternative name; clients verify it against your CA.
- A server certificate for each database member, for TLS 1.3 to PostgreSQL. The nodes verify it (`sslmode=verify-full`), so the CA must be on every node.

## PostgreSQL requirements

veho checks these at start and refuses to run otherwise (details in *The state store*):

- The nodes connect **directly** to the primary, never through a transaction-mode pooler such as PgBouncer: DBOS keeps a `LISTEN` session.
- `synchronous_commit` is `on` (or stronger), and `synchronous_standby_names` is `ANY 1 (...)` over the members of the **other** regions: a commit waits for another region.
- Patroni manages this with `synchronous_mode: quorum`, which needs **Patroni 4.0 or later**. Ubuntu's own Patroni (3.2) does not have it: use the PGDG repository.
