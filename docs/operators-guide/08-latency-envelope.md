# The latency envelope

How veho behaves as the round trip between regions grows, as measured in the lab: three regions over an emulated WAN, each cell an hour of light load with the WAN set to one round trip, jitter or packet loss, then the database primary and a node holding a transfer killed. Every release redraws these charts from the measurements kept with the documents, so a version that is slower than the one before shows here.

What a cell measures:

- **Spurious elections and demotions**: etcd leader elections and Patroni demotions while nothing failed. Any is a failure of the database timeouts for that round trip.
- **Acceptance latency**: how long `CreateJob` takes, p50 and p99, probed every 15 s through a random node. The requirement (NFR-4) is a p99 under 1 s up to a 150 ms round trip.
- **Failover**: from killing the primary until a job is accepted again. The requirement (NFR-3) is within 5 minutes.
- **Adoption**: from killing a node until another node carries on its transfer.
- **Jobs done**: the jobs the light load completed in the cell, a measure of throughput.

A cell is *claimed* when its profile promises to hold it: no packet loss and jitter up to 25 % of the round trip. A claimed cell passes with no spurious election or demotion, the acceptance p99 under 1 s (up to 150 ms) and a failover within 5 minutes. A cell outside the claims is measured and shown, but not judged.

## Acceptance latency

![Acceptance latency by round trip and jitter, for the latest version measured.](latency-envelope-acceptance.pdf){width=100%}

At a given round trip, the latency does not depend much on jitter: it grows with the round trip itself. A job's acceptance is one transaction, committed synchronously to a standby in another region. A node outside the primary's region pays a round trip for every statement it sends, not only for the commit.

## Throughput

![Jobs done in a cell, by jitter and round trip, for the latest version measured.](latency-envelope-throughput.pdf){width=100%}

High jitter (50 % of the round trip) reorders packets, and the transfers' TCP connections slow down. Real WANs seldom show this much jitter; where they do, expect throughput to fall even though jobs keep succeeding.

## Across versions

![Acceptance p99 without jitter, one line per version measured.](latency-envelope-regression.pdf){width=100%}

## The measurements

::: landscape

```{.markdown include="docs/common/data/latency-envelope.md"}
```

:::

All the measurements, of every version, are in `docs/common/data/latency-envelope.csv` in the source repository. Each row names the commit the nodes ran, so a version's rows can be dropped once it no longer matters.
