# Durable execution

## Acceptance in one transaction

`Submit` inserts the job, one transfer per destination, and enqueues the job's `ReplicateJob` workflow, all in one transaction on the primary. The commit waits for a quorum standby in another region. Either everything exists, durably, or nothing does: there is no job without a workflow, and no workflow without its job.

## The workflows

`ReplicateJob` resolves the source (its size and version, and the part size of the job), then starts one `Transfer` child workflow per destination on the `transfer` queue, and waits for them. `Transfer` copies one object to one destination:

![A transfer, part by part.](transfer.pdf){width=100%}

1. **Claim**: the transfer is taken under a **lease** and its **fence** is incremented. Every later write of the transfer's state carries the fence it claimed; a write with a stale fence changes nothing.
2. **Begin** an upload at the destination, or **resume** one from its stored token. Uploads are invisible under the final name until committed.
3. **Batches**: the next `parts_in_flight` parts not yet recorded are read from the source (each read passing the source's version, so a changed source fails as `SourceChanged`) and written to the destination, in parallel where the driver allows it. Each part's SHA-256 is computed while it streams. The batch's parts, their hashes and the bytes done are recorded in one transaction.
4. **Commit** the upload: the object appears atomically under its final name. Committing again with the same parts returns the same object.
5. **Verify** size and digest, then end the transfer and decide the job, in one transaction under the job's lock.

Each of these is a DBOS step: checkpointed, so that after a crash the workflow replays from the last completed step, not from the beginning.

## The digest

An object's digest is computed from its parts, not over the whole object at once:

```text
SHA-256("veho-digest/v1" ‖ part size ‖ size ‖ SHA-256(part 0) ‖ … ‖ SHA-256(part n−1))
```

The parts' hashes are recorded with the parts, so a transfer resumed on another node has every hash it needs: a restart never loses checksum state and never forces the copy to begin again. The first transfer to verify records the digest on the job; every other must match it. One part size per job makes the digests of its destinations comparable. The digest is not the `sha256sum` of the object.

## Leases, heartbeats and fences

- Every node records a **heartbeat** every 10 s (`engine.heartbeat`) in the executors' table, and renews the leases of its transfers.
- A node that cannot record its heartbeat for half the lease **fences itself**: it stops its own work, because another node may soon take it.
- A transfer's **fence** moves each time a node claims it. A node that was frozen and comes back (a **zombie**) may finish the step it was in, but its next write carries the old fence and is refused. Part writes are idempotent per transfer and part, so a zombie's last part does not corrupt the object; it only costs bandwidth.
- Times are compared on the database's clock, never across nodes' clocks.

## Adoption

DBOS's open-source edition does not detect dead executors by itself. veho's housekeeping does: for each node whose last heartbeat is older than the lease (60 s), it lists that node's pending workflows and resumes them, through DBOS's public API, on the queue, where any live node picks them up. The adopter claims each transfer (moving the fence) and continues from the last recorded batch.

A node that is **stopped** (SIGTERM) leaves its workflows pending for adoption: DBOS's context is deliberately not derived from the signal's context, because DBOS durably cancels a workflow whose context ends for any other reason than its own shutdown. The lab found this: a clean stop cancelled the node's work, and its transfers then failed instead of being adopted.

A transfer adopted more than 20 times is failed (class `Internal`): it is a poison pill that crashes whichever node runs it. A workflow DBOS parks after too many recoveries is handled the same way.

## The zombie kinds

| Kind | Housekeeping does |
| :------------------------------------------ | :---------------------------------------------- |
| A dead node's pending workflows | Resumes them elsewhere (adoption) |
| A job whose workflow DBOS no longer knows | Enqueues it again |
| A job whose workflows all ended without recording the outcome | Fails the transfers left open (class `Internal`) and decides the job |
| A lease that expired while its owner lives | Counted (`veho_expired_leases`): a fenced or stuck node; the fence protects the state |

## Retries

A failed step is classified by the driver: `Transient` errors are retried with exponential back-off and jitter (the jitter derived from the transfer's ID and attempt, so a replay computes the same delay), from the policy's `retry.initial` up to `retry.max_interval`, until the deadline; `Integrity` rewrites the part, at most three times; `ResumeInvalid` begins the upload again; `Permanent` and `SourceChanged` fail the transfer; `Unknown` is retried at most ten times in a row. The waits are DBOS durable sleeps: a node can restart during one.

## Cancel

A cancel sets a flag on the job and ends at once the transfers that are pending or waiting to retry, moving their fences; running transfers stop at their next batch and abort their uploads, so no object appears under the final name and no multipart upload is left behind.
