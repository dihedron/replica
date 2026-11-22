# Design decisions and alternatives

::: landscape

| Decision | Why | Alternatives considered |
| :------------------------------------------------ | :---------------------------------------------------- | :---------------------------------------------- |
| DBOS Transact on PostgreSQL as the durable engine (open-source features only) | Durable workflows and queues with PostgreSQL as the only infrastructure; a job and its workflow commit together | Temporal (a second stateful cluster to run across regions); a hand-written outbox with leases; distributed SQL databases |
| One primary with a synchronous quorum standby in another region, Patroni and etcd for failover | No acknowledged commit lost when a region is lost | Asynchronous replication (loses recent jobs); multi-primary (conflicts) |
| Consistency over availability | A job is never lost or forked | Active-active with conflict resolution |
| Symmetric nodes: any node accepts and executes | Nothing to fail over between nodes; any node resumes any work | Executors pinned to regions |
| veho's own executor registry, adoption and fencing | Open-source DBOS does not detect dead executors | DBOS Conductor (commercial) |
| Part-level checkpoints, a digest from the parts' hashes | Large objects resume instead of restarting, and the checksum survives the resume | Whole-object retries; a streaming whole-object hash (lost on a resume) |
| Drivers behind one interface, compiled in or as go-plugin processes | Isolation and third-party drivers, without forcing either | Only compiled-in drivers; only plugins |
| Jobs name registered endpoints, never URLs | Request forgery and tenant isolation | Free-form URLs, filtered |
| Tenant isolation by row-level security in one cluster | Simple operation; enforced by the database, not only the code | A database per tenant |
| gRPC first, REST generated from the same definitions | One implementation of authentication, authorisation and audit; streams for watching jobs | Two hand-written APIs |
| Secrets in PostgreSQL under envelope encryption, the unwrap key outside the database | No extra service to run; unattended restarts | Vault from the start (planned as an option) |
| Clients poll, long-poll or watch; veho never calls clients | No inbound path to clients, no calls to tenant-supplied URLs | Webhooks (planned as an option, off by default) |
| Fail fast: a job that can no longer meet its policy fails at once and cancels its other transfers | No bandwidth spent on a job that has failed; `retry` restarts everything | Letting the other transfers finish |
| A node-wide memory budget for parts | A node's memory stays bounded whatever the object size and the number of transfers | Per-transfer bounds only (which grow with the object above 640 GiB) |

:::
