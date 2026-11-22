# Routine procedures

The commands below are the `veho` client (connection in `VEHO_API_URL`, `VEHO_CA_FILE` and a credential, *Setup Guide*, chapter *A first replication*), except those marked *as `veho_migrator`*, which connect to the database directly and need `VEHO_MIGRATOR_DATABASE_URL`.

## Tenants (platform administrators)

```bash
veho tenant create globex
veho tenant list
veho tenant update <id> --state suspended            # submissions refused; running jobs go on
veho tenant update <id> --state active
veho tenant update <id> --max-destinations 16 --max-active-jobs 200 \
                        --min-retry-initial 1s --max-retry-interval 1h
```

Quotas, with their defaults:

| Quota | Default | Limits |
| :------------------------ | :------------- | :-------------------------------------------------- |
| `max_destinations` | 16 | Destinations of one job; at most 32 (the platform's ceiling) |
| `max_active_jobs` | 1000 | Jobs not yet settled; beyond it a submission is refused with `quota_exceeded` |
| `min_retry_initial` | 1 s | The shortest first retry delay a policy may set; at most 2 s |
| `max_retry_interval` | 1 h | The longest retry delay a policy may set; at least 15 min |

The two retry bounds keep a tenant's policies from hammering a shared storage system; the policy defaults (2 s, then up to 15 min) are always within them. `tenant update` replaces all the quotas at once: give every one you want to keep.

A platform administrator sees tenants, never what is inside them.

## People and applications (tenant administrators)

```bash
veho apikey create --name ci --role operator > ci.key    # printed once
veho apikey list
veho apikey revoke <id>                                  # at once

veho entitlement create --kind oidc --issuer https://idp.example --group storage-ops --role operator
veho entitlement create --kind spiffe --issuer example.org --subject spiffe://example.org/backup --role operator
veho entitlement list
```

Roles: `viewer` reads; `operator` also submits, cancels and retries jobs; `admin` also manages endpoints, secrets, policies, keys, entitlements and settings.

## Policies and who may use them

Named policies are versioned: `veho policy create geo --file geo.json` makes version 1, the next call version 2; jobs name `geo` (the latest) or `geo@1`. A policy in use by a running job cannot be deleted.

```bash
veho policy grant geo group:storage-ops submit
veho policy grant geo key:ci read
veho policy revoke geo group:storage-ops submit
```

Grantees are `key:<name>` (an API key's principal), `oidc:<subject>`, `spiffe:<id>` or `group:<name>`; permissions are `submit`, `read` and `manage`. Tenant administrators may do everything without grants. Whether principals may submit inline policies, which no grant governs, is a tenant setting:

```bash
veho settings update --inline-policies granted    # all (default), granted, none
```

## Endpoints and secrets

```bash
veho secret set s3-b --from-file creds.json       # or --stdin; a new version
veho endpoint update b-images --config-file b.json --credential-ref db://s3-b
veho endpoint test b-images
```

An endpoint referenced by a job (settled ones included, until retention removes them) cannot be deleted; a secret referenced by an endpoint cannot be deleted either.

## Rotating the key-encryption key

Rotate the KEK periodically, or when a node may have been compromised. *As `veho_migrator`*, from a host with the unwrap key:

```bash
veho kek rotate --unwrap-key file:./unwrap.key    # new active version; the old one is "retiring"
veho kek rewrap --unwrap-key file:./unwrap.key    # re-wraps every data key; retires unused versions
veho kek show
```

New secrets use the new KEK at once. The nodes load the KEK versions at start: restart them, one at a time, after a rotation. `rewrap` works in batches (`--batch`, default 500) and can be run again if interrupted.

## Replacing the unwrap key

*As `veho_migrator`*, with both keys:

```bash
openssl rand 32 > new-unwrap.key
veho kek rekey --unwrap-key file:./unwrap.key --new-unwrap-key file:./new-unwrap.key
```

Then give every node the new key (re-encrypt its systemd credential) and restart them one at a time. Destroy the old key only once every node runs with the new one.

## Retention

Settled jobs are kept 90 days (`housekeeping.jobs_retention`), DBOS's records of ended workflows 7 days (`housekeeping.workflow_retention`). Change them in `veho.yaml` on every node; housekeeping applies them on its next pass.

## Nodes

- **Adding a node**: install, configure (its own `node.id`), start. It takes work at once.
- **Removing a node**: `systemctl stop veho`, then remove the host. Its unfinished transfers are adopted after its lease (60 s).
- **Restarting a node** (a new configuration, a new KEK): `systemctl restart veho`, one node at a time; the copies it had in flight are resumed from their last recorded part.

## Upgrading the nodes

Each build carries a **workflow version**, logged at start (`workflow_version`). DBOS gives each node only the work of its own workflow version, so how you upgrade depends on whether it changes (the release notes say so):

- **The same workflow version** (the usual case): replace the nodes one at a time (stop, install, start). Transfers in flight are resumed by the other nodes, then by the new ones. Measured in the lab: no job affected.
- **A new workflow version**: the old nodes must finish the old work.
  1. Upgrade some of the nodes, keeping at least one old node in each region you need. An upgraded node hands the old work it was running back to the queue within a minute, and the old nodes take it.
  2. Point the clients at the upgraded nodes: a job submitted to an old node is old work.
  3. Wait until no work of the old version is left. As `veho_dbos`:

     ```sql
     select application_version, status, count(*) from dbos.workflow_status
     where status in ('PENDING', 'ENQUEUED') group by 1, 2;
     ```

  4. Upgrade the remaining nodes.

  Do not stop the last old node while old work is left: nothing would run it until a node of that version starts again.

## The state store

- **Planned switchover** (maintenance of the primary's host): `patronictl switchover --leader <current> --candidate <standby> --force`. The nodes reconnect by themselves; copies in flight retry their current step.
- **Backups**: continuous WAL archiving with point-in-time recovery, to object storage in a region other than the primary's. A backup holds encrypted secrets only; it is useless without the unwrap key.
