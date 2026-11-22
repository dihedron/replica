# Operating model

This guide is for whoever runs veho day to day and is on call for it. The *Setup Guide* covers installing it; *Architecture and Design* explains why it behaves as it does.

## What runs where

- **Nodes.** Every `veho serve` is the same: it serves the API and executes copies. Any node can take any request and copy any object. There is no leader among the nodes and nothing to fail over: when a node stops, the others carry on, and its unfinished work is adopted once its lease expires.
- **The state store.** One PostgreSQL cluster across three regions, managed by Patroni and etcd. It is the only stateful part of veho. A commit waits for a second region, so losing a region loses nothing that was acknowledged.
- **The storage systems.** The tenants' buckets and file systems, reached through registered endpoints. veho never stores object data itself; a part is in a node's memory only while it is being copied.

## The objects you deal with

| Object | What it is |
| :------------------ | :---------------------------------------------------------------- |
| Tenant | An isolated customer: its endpoints, secrets, policies and jobs are invisible to others. Suspended tenants cannot submit |
| Principal | A caller: an API key, an OIDC identity or a SPIFFE identity, with a role in one tenant (`viewer`, `operator`, `admin`), or a platform administrator |
| Entitlement | The mapping of an OIDC or SPIFFE identity (or an OIDC group) to a tenant and role |
| Endpoint | A registered storage location: driver, configuration, location in the topology, secret of its credentials |
| Secret | A credential, stored encrypted, written and never read back |
| Policy | Where copies go and when a job is done: destinations, minimum copies, spread over regions or zones, options. Named policies are versioned and have grants |
| Job | One source object to replicate under one policy |
| Transfer | One copy of a job's object to one destination |

## The life of a job

![The states of a job.](job-states.pdf){width=80%}

A job is `ACCEPTED` as soon as its row, its transfers and its workflow are committed. It is `RUNNING` while its transfers copy, and `SUCCEEDED` as soon as its policy is met: the minimum number of verified copies, spread as required. With the default `on_satisfied: complete`, the remaining transfers continue after that and a later failure of one of them does not change the job. A job is `FAILED` as soon as its policy can no longer be met (fail fast: its other transfers are then cancelled), or when its deadline passes (seven days by default). `veho job retry` restarts the failed and cancelled transfers of a failed job.

Three times are recorded separately: `satisfied_at` (the policy met), `completed_at` (every destination copied) and `settled_at` (every transfer ended).

## Housekeeping

Every 30 seconds, one node (whichever takes a PostgreSQL advisory lock first) runs housekeeping:

- **adoption**: the workflows of a node not seen for a lease (60 s) are resumed on the live nodes; a transfer adopted more than 20 times is failed, as a poison pill;
- **zombies**: a job whose workflow DBOS lost is started again; one whose workflow ended without recording its outcome is repaired;
- **deadlines**: jobs past theirs are failed and their transfers cancelled;
- **retention**: settled jobs older than `housekeeping.jobs_retention` (90 days) and DBOS's records of ended workflows older than `housekeeping.workflow_retention` (7 days) are deleted.

Every node also resumes, every 30 seconds, its own pending workflows that it no longer executes (after a database failover cut one short). Every repair is audited.

## What needs a person

Most failures need nobody: a node that dies is replaced by the others, a destination that is down is retried with back-off, a primary that is lost is replaced by Patroni. A person is needed for:

- a region partitioned for long: jobs submitted there are refused until it is back (by design);
- a destination that refuses permanently (credentials revoked, bucket deleted): fix it, then retry the job;
- the state store losing two regions at once: no commits are possible until a quorum is back;
- the keys: rotating the KEK, replacing the unwrap key;
- capacity: adding nodes, changing quotas.
