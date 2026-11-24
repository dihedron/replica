# Context and goals

## The problem

An organisation keeps large objects (virtual machine images, backups, data sets, from gigabytes to terabytes) and needs copies of them in other regions, sites or clouds: for disaster recovery, to serve them close to where they are used, or because a regulation asks for copies in several places. Copying them by hand or with scripts fails in the ways large transfers fail: a copy dies at 80 % and starts again; a destination is down for an hour; a node is rebooted mid-copy; nobody knows which copies are complete and correct.

veho does these copies as a service. A client submits a **job** (copy this object to these destinations, under this **distribution policy**), and veho makes it happen, durably: it survives the loss of nodes and of a whole region, retries what can be retried, resumes rather than restarts, and verifies every copy before it counts.

## Goals

| Goal | What it means |
| :--------------------- | :-------------------------------------------------------------------- |
| Durable acceptance | Once a job is acknowledged, it is not lost, even if the acknowledging region is lost a moment later (RPO 0 for a region) |
| Asynchronous | Acceptance takes a fraction of a second; the copy takes as long as it takes, minutes or hours |
| Resumable | A copy interrupted by a node failure resumes from its last recorded part, on any node |
| Verified | A copy counts only once its size and digest match the source's |
| Policy-driven | M of N copies, spread over regions or zones; the job is done when the policy is met, failed as soon as it cannot be |
| Multi-tenant | Tenants are isolated from each other in the API, in the database and in the use of credentials |
| Simple to run | One binary, one database cluster; no message broker, no workflow server |

## Non-goals

- **A storage system.** veho stores no object data; the objects live in the tenants' storage systems.
- **Synchronisation.** veho copies objects it is asked to copy; it does not watch buckets or mirror them. It replicates data, not commands: deleting or changing the source later does nothing to the copies, and removing the copies is the client's job. Objects are expected to be VM and container images, from a few hundred megabytes to about 10 GB.
- **High throughput of small objects.** veho is built for few, large objects (tens of concurrent transfers across the cluster, not thousands per second).
- **Strong availability under partition.** A region cut off from the other two cannot accept jobs: veho prefers never losing or forking a job to accepting one it might lose.

## Scope of this document

This document explains how veho works and why it is built this way, what it protects against and what it does not. It describes the current, preliminary version; where something is planned rather than built, it says so.

![Inside one veho serve process.](components.pdf){width=100%}
