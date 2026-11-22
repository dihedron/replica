# The failure model

What happens when each part fails, and what is guaranteed.

::: landscape

| Failure | What veho does | Guarantee |
| :---------------------------------- | :------------------------------------------------------------------------ | :---------------------------------------------- |
| A node crashes mid-transfer | Its lease expires (60 s); housekeeping resumes its workflows on another node, which continues from the last recorded batch | No job lost; at most one batch copied twice |
| A node is frozen past its lease, then resumes | It fenced itself after half the lease; another node took over; its late writes carry a stale fence and are refused | One final object, written by one owner's state |
| The node that accepted a job dies just after `202` | The job and its workflow were committed before the answer; any node runs it | Acceptance is durable |
| Every node of a region stops | The nodes of the other regions continue the work, after the lease | Work continues while two regions are up |
| The PostgreSQL primary is lost | Patroni promotes a quorum standby; nodes reconnect through their multi-host DSN; copies retry their current step | No acknowledged commit lost (RPO 0) |
| A region is partitioned from the other two | It cannot commit: its nodes answer `unavailable` and fence themselves; the majority side fails over if needed and adopts the region's work | Consistency over availability; no fork |
| Two regions are lost | No commit is possible; every node fences itself | Nothing accepted can be lost or forked |
| A destination is down | Retries with back-off until the deadline | Completes after recovery |
| A destination refuses (403, missing bucket) | The transfer fails `Permanent`; the job fails at once if its policy can no longer be met, cancelling the rest | A clear verdict; `retry` after the fix |
| A part is corrupted in flight | The destination refuses it (MD5) or verification fails; the part is rewritten | No corrupt copy counts |
| The source changes mid-copy | Reads carry the source's version: the transfer fails `SourceChanged` | Never a mix of two versions |
| A poison pill crashes every node that runs it | After 20 adoptions, the transfer fails (`Internal`) | The cluster is not taken down by one job |
| A node cannot unwrap the key | It refuses to start | No node runs without the means to read credentials |
| The system log stalls | Each audit write waits at most 250 ms, then the sink pauses and counts drops | The service is never stopped by the audit |

:::

## What is not guaranteed

- **Availability under partition.** A partitioned region accepts nothing until it rejoins.
- **Exactly-once bandwidth.** A part may be written twice (a zombie's last step, a retried batch); the object is still correct.
- **Source integrity before the first read.** veho copies what the source holds; an object corrupt at the source is copied faithfully, and the recorded digest proves only that the copies equal it.
- **The audit trail.** Events can be lost on a node crash and carry no tamper evidence (planned).
