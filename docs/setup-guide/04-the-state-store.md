# The state store

All of veho's durable state lives in one PostgreSQL database: the jobs and transfers, the policies, endpoints and tenants, the encrypted secrets, and DBOS's workflow tables, in the same database so that a job and its workflow commit together. This chapter prepares it.

## The cluster

One PostgreSQL 16 member per region, managed by Patroni, with one etcd member per region as Patroni's store. One member is the primary; the two others stream from it and are its **quorum standbys**: a commit returns once **one** of them has flushed it. Losing any one region therefore loses no committed job.

These Patroni settings are what veho relies on (the bootstrap `dcs` section of every member's `patroni.yml`):

```yaml
bootstrap:
  dcs:
    ttl: 30
    loop_wait: 10
    retry_timeout: 10
    synchronous_mode: quorum        # Patroni 4.0+: manages "ANY 1 (...)"
    synchronous_node_count: 1
    postgresql:
      use_pg_rewind: true
      parameters:
        synchronous_commit: "on"
```

And in each member's `postgresql.parameters`:

```yaml
ssl: "on"
ssl_min_protocol_version: TLSv1.3
password_encryption: scram-sha-256
# a lost client's session (and its locks) ends in about two minutes
tcp_keepalives_idle: 60
tcp_keepalives_interval: 10
tcp_keepalives_count: 6
```

With `pg_hba` lines that accept the veho roles over `hostssl` from the nodes' networks, with `scram-sha-256`, and nothing over plain TCP.

etcd: one member per region, client and peer URLs on the region's address; heartbeat 100 ms and election timeout 1000 ms are right for inter-region round trips of up to about 50 ms. Give the member in the region you least want as primary a lower `failover_priority` tag.

## The database and its roles

Run `/usr/share/veho/postgres/roles.sql` once, as the PostgreSQL superuser on the primary, with four passwords you generate:

```bash
psql -v migrator_password="$(openssl rand -hex 24)" \
     -v dbos_password="$(openssl rand -hex 24)" \
     -v api_password="$(openssl rand -hex 24)" \
     -v worker_password="$(openssl rand -hex 24)" \
     -f /usr/share/veho/postgres/roles.sql
```

Keep the passwords: the nodes need three of them, the administrators the fourth. The script creates:

| Role | Used by | May |
| :-------------- | :-------------------------------------- | :---------------------------------------------------- |
| `veho_migrator` | `veho migrate`, `veho kek`, `veho bootstrap` (administrators) | Own the schema `veho`: create its objects, initialise the keys |
| `veho_api` | The API of every node | Read and write the tenants' rows, under row-level security |
| `veho_worker` | The executor and housekeeping of every node | Update transfers and jobs; cross-tenant work only through reviewed functions |
| `veho_dbos` | DBOS, on every node | Own the schema `dbos`, DBOS's tables |

and the database `veho` with its schemas `veho` and `dbos`. The tenants' rows are protected by row-level security: a session sees only the tenant it set, and a session that set none sees nothing.

## Connection strings

Every DSN lists the three members, so that a client follows a failover, verifies the server and asks for the primary:

```text
postgres://veho_api:<password>@db-a.example:5432,db-b.example:5432,db-c.example:5432/veho?sslmode=verify-full&sslrootcert=/etc/veho/tls/ca.crt&target_session_attrs=read-write
```

veho refuses a DSN whose `sslmode` is not `verify-full`. Each connection gives up on a member it cannot reach (a partitioned region) after 5 seconds and tries the next one; without that, the operating system would wait about two minutes per member. A `connect_timeout` in the DSN is ignored.

## The schema

From an administrator's host with the binary, as `veho_migrator`:

```bash
export VEHO_MIGRATOR_DATABASE_URL='postgres://veho_migrator:<password>@db-a.example:5432,db-b.example:5432,db-c.example:5432/veho?sslmode=verify-full&sslrootcert=ca.crt&target_session_attrs=read-write'
veho migrate
```

`veho migrate` applies the migrations the binary carries, in order, each in its own transaction, under a lock so that two runs cannot overlap. The DSN is read from the environment, never from the command line; `--dsn-env` names another variable.

## What a node checks at start

`veho serve` stops at start, with a message naming the check, when one of these fails:

| Check | Fails when |
| :------------------------------ | :---------------------------------------------------------------- |
| A direct connection to the primary | The session is on a standby, or on a pooler rather than a real backend |
| Synchronous commit | `synchronous_commit` is `off` or `local` |
| A quorum over other regions | `synchronous_standby_names` is empty, or not `ANY n (...)` with `n` of 1 or more |
| The schema version | The database is behind or ahead of the migrations in the binary |

## Topology: the regions

Endpoints and nodes name the **location** they are in: a region, or an availability zone inside one. Locations form a tree with named levels. Until a command manages them, create them as `veho_migrator`:

```sql
select veho.put_location('region-a', 'region', null);
select veho.put_location('region-b', 'region', null);
select veho.put_location('region-c', 'region', null);
-- optionally, zones inside a region
select veho.put_location('region-a-az1', 'az', 'region-a');
```

A node's `node.region` must be one of them, or it refuses to start.
