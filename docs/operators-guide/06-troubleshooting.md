# Troubleshooting

## A node does not start

`journalctl -u veho -b` shows the step that failed and why. The usual ones:

| Message contains | Cause | Fix |
| :-------------------------------------- | :------------------------------------------------ | :------------------------------------------ |
| `config:` and a key name | An unknown key or an invalid value in `veho.yaml` | Fix the key (*Setup Guide*, configuration reference) |
| `environment variable ... is not set` | A DSN missing from `veho.env` | Add it; check the unit's `EnvironmentFile` |
| `sslmode` or `insecure` | A DSN without `sslmode=verify-full` | Add it, with `sslrootcert` |
| `standby`, `pooler` | The DSN reaches a standby or a pooler | List every member and add `target_session_attrs=read-write`; connect directly |
| `synchronous_commit`, `synchronous_standby_names` | The cluster does not commit to another region | Patroni `synchronous_mode: quorum`, `synchronous_commit: on` |
| `schema` and a version | The binary and the database disagree on the schema | Run the matching `veho migrate`, or install the matching version |
| `STO-C5` and `clock` | The node's clock is off the database's | Synchronize it with chrony; `checks.clock: warn` only as a stopgap |
| `unwrap key` | The credential or file is missing, of the wrong size, or readable by others | *Setup Guide*, *The unwrap key* |
| `node.region` | The node's region is not a location of the topology | Create it (`veho.put_location`) or fix `node.region` |
| `socket` and `syslog` | `audit.sink: syslog` without `/dev/log` | Start rsyslog or journald's socket, or use `stderr` |
| `plugin` and `manifest` | The plugin's package is not installed, or a file of it was changed | Install `veho-plugin-<name>` of veho's version; check `/usr/libexec/veho/plugins/` |
| `plugin`, `version` | veho's version is not in the range of the plugin's manifest | Upgrade veho and the release's plugins together; for a third-party plugin, install the publisher's version for this veho |
| `plugin`, `signature` | The manifest is not signed by the key the driver names (`drivers[].key`, `veho` by default) | Reinstall the package; check `key` and the file in `/etc/veho/plugin-keys/`; never edit the manifest |
| `plugin key` | The key file is missing, holds no valid key, or is not root's | Reinstall `veho` for `veho.pub`, or the publisher's key; `chown root:root`, `chmod go-w` |
| `plugin`, `owned by root` | A plugin file or its directory is writable by someone other than root | `chown root:root`, `chmod go-w` |
| `settings refused` | A driver's `settings` has an unknown key or a bad value | Fix `drivers[].settings` (the message names the key) |

## A job does not progress

1. `veho job show <id>`: the state of each transfer, its attempt and its `last_error`.
2. A transfer in `RETRY_WAIT` with a `Transient` error: the destination or the source is failing; it retries on its own until the deadline.
3. A transfer `PENDING` for long: the tenant may be at its concurrency (`engine.tenant_concurrency` per node), or every node is busy (`engine.worker_concurrency`); add nodes or wait.
4. A transfer `COPYING` with no progress: look at `veho_expired_leases` and the owning node's log; a node that is fenced or frozen has its transfers adopted after the lease.
5. Nothing at all happens anywhere: check the state store (Patroni has a leader, a quorum standby streams) and `/readyz` on the nodes.

## Submissions are refused

| Code | Look at |
| :------------------- | :---------------------------------------------- |
| `unavailable` | The node's `/readyz`; Patroni's state; whether the node's region is cut off |
| `quota_exceeded` | `veho job list --state RUNNING`; the tenant's `max_active_jobs` |
| `rate_limited` | The caller's rate; `api.rate_limit` |
| `invalid_policy` and its message | The policy (`veho policy validate --file ...`) and the tenant's retry bounds |
| `permission_denied` | The principal's role and the policy's grants (`veho policy show`) |

## A copy fails with `Integrity`

The bytes written differ from the bytes read, three times in a row. Suspect the network path to the destination (a proxy that rewrites bodies), or the destination's storage. veho never reports such a copy as done.

## A copy fails with `SourceChanged`

The source object was overwritten while it was copied. Submit the job again once the source is stable.

## Memory

A node killed by the OOM killer (`oom-kill` in the journal) has less memory than its parts need. Give it `engine.part_memory_mib` + 200 MiB at least, or lower `part_memory_mib`; copies then wait for room rather than overflow.

## Collecting evidence for a report

- `veho job show <id> -o json` for each job concerned;
- the journals of every node for the window (`journalctl -u veho --since ... --until ...`);
- `patronictl list` and `patronictl history`;
- the version of each node (`veho version --verbose`).
