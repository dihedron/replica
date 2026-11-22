# Monitoring

## Health endpoints

On the API's port of every node:

| Path | Answers 200 when |
| :------------ | :---------------------------------------------------------------- |
| `/healthz` | The process is up |
| `/readyz` | The node reaches the state store, its workflow engine runs, and it is not fenced |

A load balancer in front of the nodes should use `/readyz`. The gRPC health service answers the same.

## Metrics

Each node serves Prometheus metrics on `metrics.listen` (default `127.0.0.1:9464`, path `/metrics`).

::: landscape

| Metric | Labels | Meaning |
| :-------------------------------- | :------------------ | :------------------------------------------------------------------------ |
| `veho_transfer_bytes_total` | none | Bytes this node copied and recorded |
| `veho_transfer_attempts_total` | `class` | Failed transfer attempts on this node, by error class (`Transient`, `Permanent`, `Integrity`, `SourceChanged`, `ResumeInvalid`, `Unknown`) |
| `veho_adoptions_total` | none | Workflows of dead nodes this node resumed |
| `veho_expired_leases` | none | Transfers whose lease had expired at the last housekeeping pass: their owner is fenced, stuck or dead |
| `veho_heartbeat_age_seconds` | none | Seconds since this node last recorded its heartbeat; above half the lease (30 s) the node stops its own work |
| `veho_objects` | `kind`, `state` | Jobs and transfers in the state store, by state, counted at each scrape (the same on every node) |
| `veho_audit_dropped_total` | none | Audit events this node could not write to the system log |

:::

The Go runtime's and the process's own metrics are not exported.

### Alerts

| Alert | Condition | Means |
| :------------------------ | :-------------------------------------------------- | :-------------------------------------------- |
| Node fenced | `veho_heartbeat_age_seconds > 30` for 2 minutes | The node cannot write to the state store: it has stopped its own work |
| Leases expiring | `veho_expired_leases > 0` for 5 minutes | A node holds transfers it does not advance |
| Permanent failures | `increase(veho_transfer_attempts_total{class="Permanent"}[15m]) > 0` | A destination refuses: credentials, bucket, permissions |
| Integrity failures | `increase(veho_transfer_attempts_total{class="Integrity"}[1h]) > 0` | Bytes changed on the way and were rewritten: a network or storage problem |
| Old jobs | `veho_objects{kind="job",state="RUNNING"}` not falling for hours | Jobs stuck behind a destination or a quota |
| Audit dropped | `increase(veho_audit_dropped_total[5m]) > 0` | The system log does not take events: they are lost |
| Synchronous standby | Patroni reports no quorum standby | Commits wait, then fail: submissions stop |

Monitor Patroni and etcd themselves too: Patroni's REST API (`/cluster`) and etcd's metrics.

## Logs

Each node logs to its journal (`journalctl -u veho`), structured, at the level of `VEHO_LOG_LEVEL` (the unit sets `info`). Lines carry `tenant_id`, `job_id`, `transfer_id`, `request_id` and `executor` where they apply. They never carry a credential, a token or a secret's value. At start a node logs its version and the SHA-256 of its configuration.

## Audit events

Every change of state is audited, after its commit, as one JSON object per event:

- with `audit.sink: syslog` (the default), to the local syslog socket, facility `authpriv`, tag `veho-audit`, severity `notice` (`warning` when the outcome is `denied` or `failed`);
- with `audit.sink: stderr`, to the journal.

Forward them to your SIEM, for example with rsyslog:

```text
# /etc/rsyslog.d/50-veho-audit.conf
if $programname == 'veho-audit' then {
  action(type="omfwd" target="siem.example" port="6514" protocol="tcp"
         StreamDriver="gtls" StreamDriverMode="1" StreamDriverAuthMode="x509/name")
  stop
}
```

Each event carries `time`, `node`, `boot` (a random ID of the process) and `seq` (1, 2, 3... per process): a missing event shows in the SIEM as a gap in `seq`. Then `action`, `outcome` (`success`, `denied`, `failed`), `tenant_id`, `principal`, the resource and the correlation IDs, and a bounded `details` object with no secret in it.

The actions:

::: landscape

| Action | When |
| :---------------------------- | :---------------------------------------------------------------- |
| `JOB_SUBMIT`, `JOB_CANCEL`, `JOB_RETRY`, `JOB_SETTLED` | A job submitted, cancelled, retried; a job's last transfer ended |
| `POLICY_CREATE`, `POLICY_DELETE`, `POLICY_GRANTS` | A policy version created; a policy deleted; its grants changed |
| `ENDPOINT_CREATE`, `ENDPOINT_UPDATE`, `ENDPOINT_DELETE` | An endpoint registered, changed, removed |
| `SECRET_PUT`, `SECRET_DELETE` | A secret written (a new version) or removed |
| `TENANT_CREATE`, `TENANT_UPDATE`, `TENANT_SETTINGS` | A tenant created; its state or quotas changed; its own settings changed |
| `APIKEY_CREATE`, `APIKEY_REVOKE` | An API key issued or revoked |
| `ENTITLEMENT_CREATE`, `ENTITLEMENT_DELETE` | An identity mapped to a tenant and role, or unmapped |
| `KEK_INIT`, `KEK_ROTATE`, `KEK_REWRAP`, `KEK_REKEY` | The key-encryption key initialised, rotated; data keys re-wrapped; the unwrap key replaced |
| `SYSTEM_ZOMBIE_ADOPTED`, `SYSTEM_ZOMBIE_RESTARTED`, `SYSTEM_ZOMBIE_REPAIRED` | Housekeeping adopted a dead node's work, restarted a lost workflow, repaired a job whose workflow ended unrecorded |
| `SYSTEM_JOB_EXPIRED`, `SYSTEM_RETENTION_PURGED` | A job failed at its deadline; old settled jobs deleted |

:::

These events can be lost: a node that crashes between a commit and the write loses that event, and events not yet forwarded live only on the node. A tamper-evident audit log in the database is planned.
