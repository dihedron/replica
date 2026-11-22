# veho: lightweight audit (chunk C4)

**Status:** approved and implemented 2026-11-21 (`internal/audit`; the secrets and `veho kek` audited). Decision of 2026-11-21: veho is not a critical system, so v1 audits by **sending one structured event per security-relevant action to the system log**, which the platform forwards to the SIEM. The full design of `veho-audit-housekeeping.md` §1, §2 and §4 (audit table, hash chain, sealer, signed checkpoints, export, offline verification, pruning, `GET /v1/audit`) and the audit signer (SEC-28) move to the backlog (item 32). The housekeeping of `veho-audit-housekeeping.md` §3 is unchanged and comes with the engine.

## 1. What is audited

The actions of AUD-1, each as one event:

- state changes: job submit, cancel, retry; policy, grant, endpoint, tenant and entitlement changes; secret writes and deletes; KEK init, rotate, rewrap and rekey; housekeeping repairs (`SYSTEM_*`);
- events that change nothing: authentication failures, authorization denials, break-glass.

C4 delivers the package and audits what exists today: the secrets (`Store.Put`, `Store.Delete`) and `veho kek`. Each later chunk audits its own actions with it (C6: jobs and housekeeping; C7: the API, authentication and authorization).

## 2. The event

One JSON object (AUD-2), with these fields; empty ones are left out:

| Field | Content |
| :--- | :--- |
| `time` | RFC 3339 with nanoseconds, UTC |
| `node`, `boot`, `seq` | the node's name, a random ID of the process, and a sequence number that starts at 1 in each process and grows by one per event: the SIEM sees a lost or deleted event as a gap |
| `action` | e.g. `SECRET_PUT`, `SECRET_DELETE`, `KEK_ROTATE`, `JOB_SUBMIT`, `AUTHN_FAILED` |
| `outcome` | `success`, `denied` or `failed` |
| `tenant_id`, `principal`, `groups` | who; the principal is an identifier, never a token |
| `resource_type`, `resource_id`, `resource_name` | on what |
| `job_id`, `transfer_id`, `request_id`, `workflow_id` | correlation |
| `details` | a non-secret JSON object (INV-5), at most 4 KiB once encoded; a larger one is replaced by `{"truncated": true}` |

An event never carries a credential, a token, a secret's value, a key or personal data beyond the principal's identifier (INV-5). `details` takes a value the caller builds from IDs, names, versions and counts; a `secrets.Secret` marshals as `[redacted]` in any case.

## 3. Where it goes

```go
// Auditor records events. Emit never fails the caller's action: an event
// that cannot be written is logged with slog at error level and counted.
type Auditor interface{ Emit(ctx context.Context, e Event) }
```

Two sinks, chosen by configuration (`audit.sink`, with C6's configuration; `veho kek` takes `--audit-sink`):

- **`syslog`** (the default): a datagram per event on the local syslog socket (`/dev/log`, which rsyslog, syslog-ng and journald all read), facility `authpriv`, severity `notice` (`warning` for `denied` and `failed`), tag `veho-audit`, the message being the JSON object. The platform forwards it to the SIEM (rsyslog `omfwd` over TLS, or the journal's forwarder); the Setup Guide gives an rsyslog example. The writer sets a 250 ms deadline on each write; a write that times out **pauses** the sink for 5 s, during which writes fail at once (counted, AUDL-5), so a stuck syslog daemon costs one request 250 ms every 5 s, not every request; any other error reconnects once. `authpriv` keeps the events out of world-readable logs on the usual distributions.
- **`stderr`**: one JSON line per event on standard error, prefixed by nothing else, for containers (the platform's log collector takes it) and for development.

Tests use a recorder that keeps the events in memory.

A node configured with `syslog` fails at start when the socket is missing (it dials the socket at start).

## 4. When an event is emitted

- **A state change is audited after its commit.** The event says what happened, not what was tried; a change that rolls back emits a `failed` event instead, if it got as far as the database. The cost is that a node crashing between the commit and the write loses that event (R-14). Nothing is buffered: the write is a local datagram.
- **The events of §1 that change nothing** are emitted when they happen.

## 5. Threat model of C4

| ID | Threat | Mitigation | Residual |
| :--- | :--- | :--- | :--- |
| T-35 | The audit record is altered or deleted to hide an action | The record of authority is the SIEM's copy, off the node and out of the database: an attacker with the database or the application's roles cannot reach it; `node`, `boot` and `seq` make a gap visible in the SIEM | Events not yet forwarded can be removed by root on the node; no cryptographic tamper evidence (R-14) |
| C4-1 | An event carries a secret | Events carry IDs, names, versions and counts; `details` is bounded; SECR-9's redaction; AUDL-3 scans the events of the tests for the canary | — |
| C4-2 | A stuck or missing syslog daemon stalls or breaks requests | A 250 ms write deadline and a 5 s pause after a timeout; `Emit` never fails the action; the failure is logged and counted; a missing socket fails the start | Events lost while the daemon is stuck (R-14) |
| C4-3 | Forged events: a local process writes to `/dev/log` with the tag `veho-audit` | Not prevented: any local user can write to the syslog socket. journald records the sender's PID and UID (`_PID`, `_UID`), which the SIEM can check | Accepted (R-14) |
| C4-4 | Audit events are read by users of the host | Facility `authpriv`, which the distributions route to a root-only file | — |

**R-14** is added to the threat model, and replaces R-10 and R-11 while the full audit log is in the backlog: events can be lost (a crash between commit and write, a stuck or missing syslog daemon, the forwarder's own losses), forged by a local process, or removed on the node before they are forwarded, and nothing in veho proves that the record is complete. The SIEM's copy and the sequence numbers are the mitigation. The full design (backlog 32) removes the risk.

## 6. Acceptance

| ID | Test |
| :--- | :--- |
| AUDL-1 | An event encodes with the fields of §2; empty ones are left out; `seq` grows by one per event; `details` over 4 KiB becomes `{"truncated": true}` |
| AUDL-2 | The `syslog` sink writes `<PRI>veho-audit[pid]: {json}` with facility `authpriv` and the right severity to a socket (a test socket stands in for `/dev/log`), times out on a socket that is not read and then refuses writes at once while it pauses, reconnects after the socket is replaced, and fails at start without a socket |
| AUDL-3 | `Store.Put` and `Store.Delete` emit `SECRET_PUT` and `SECRET_DELETE` with tenant, name and version, only after their commit, and `failed` on an error; no event carries the canary value (integration) |
| AUDL-4 | `veho kek init`, `rotate`, `rewrap` and `rekey` each emit their event, with the versions and counts (integration) |
| AUDL-5 | A write that fails is logged with slog and does not fail the action |

## 7. Documentation

Until the guides exist (backlog 7): the Setup Guide takes the rsyslog example of §3 and the facility; the Operator's Guide takes the list of actions and the meaning of `boot` and `seq`; the Architecture and Design Guide takes R-14 and the pointer to the full design.
