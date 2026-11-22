# Introduction

veho replicates large objects (disk images, backups, archives: up to terabytes) from one storage system to several others, in other regions, sites or clouds. A client submits a **job**: copy this source object to these destinations, according to a **distribution policy**. veho accepts the job durably, in a fraction of a second, and copies it in the background. If a node dies, another resumes the copy from the last part it recorded; if a destination fails, the copy is retried with back-off; every copy is verified against the source before it counts.

This guide takes you from nothing to a first working replication. *Architecture and Design* explains why veho works as it does; the *Operator's Guide* covers running it.

## What you install

A deployment has two layers:

| Layer | What | Where |
| :------------------ | :---------------------------------------------------------- | :-------------------- |
| The state store | One PostgreSQL 16 cluster with a member in each of three regions, managed by Patroni with a three-member etcd, committing synchronously to a quorum of the other regions | One database host per region |
| The nodes | `veho serve`, the service: the API and the executor of the copies, all nodes identical | Two or more hosts per region (the reference deployment has two) |

The storage systems themselves (S3 buckets, file systems) are not part of veho: they are the tenants', and veho reaches them through registered **endpoints**.

The package `veho` installs:

| Path | What |
| :------------------------------------------ | :---------------------------------------------- |
| `/usr/bin/veho` | The service and the command-line client, one binary |
| `/usr/lib/systemd/system/veho.service` | The systemd unit, confined (no capabilities, a read-only system) |
| `/usr/share/veho/postgres/roles.sql` | The database, its schemas and its four roles, run once by the database administrator |

The post-install script creates the system user `veho` and the directory `/etc/veho`; it neither enables nor starts the service.

![The reference deployment: three regions, symmetric nodes, one state store.](topology.pdf){width=100%}

## How a replication goes

1. A client (the `veho` command, or an application over gRPC or REST) submits a job to any node.
2. The node writes the job, one **transfer** per destination and the job's workflow in one PostgreSQL transaction. The commit waits for another region to have the data, so a region lost a moment later loses nothing. The node answers `202 Accepted` with the job's ID.
3. Any node picks the transfers up and copies each object part by part, recording every part.
4. When enough copies are verified to meet the policy, the job is `SUCCEEDED`. The client polls the job, long-polls it, or watches its stream.

## Known limits of this version

veho has not been released yet. What follows is true of the current code and is tested in the project's lab; these limits apply:

- **No upgrade path for the schema.** Until the first deployment, the database schema is changed in place: a new version may need an empty database (chapter *Upgrades and removal*). Rolling upgrades of the nodes with transfers in flight are validated.
- **Topology by SQL.** The regions and sites that endpoints and nodes refer to are created with a SQL function; no command or API manages them yet.
- **Drivers.** The shipped drivers are `file` (a directory) and `s3` (any S3-compatible service). A Glance driver is planned. Process plugins for other storage systems can be loaded, but no third-party plugin exists.
- **Audit.** Audit events go to the local system log, to be forwarded to a SIEM; they are not tamper-evident (the full audit log is planned).
- **Secrets.** The key that protects the stored credentials comes from a file or a systemd credential; Vault, HSMs and a Shamir unseal are planned.
- **WAN validation.** The state store's behaviour over the real inter-region latency for days on end, and the latency envelope, are still being measured.
