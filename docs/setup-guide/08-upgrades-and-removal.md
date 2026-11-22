# Upgrades and removal

## Upgrades before version 1.0.0

Until a first deployment exists, the database schema is still changed in place, so a new version may not be able to use a database created by an older one. A node refuses to start when its schema version differs from the database's, naming the difference.

Until then, to move to a new version:

1. Let the running jobs settle, or cancel them.
2. Stop every node.
3. Recreate the schema as `veho_migrator`: `drop schema veho cascade; create schema veho authorization veho_migrator;`, then `veho migrate`, `veho kek init` and the bootstrap steps of chapter *A first replication*. Tenants, endpoints, policies and secrets must be created again.
4. Install the new version on every node and start them.

From the first deployed release on, migrations are additive within a release series, and nodes of the old and the new version can run side by side during an upgrade. Rolling upgrades with transfers in flight are the subject of an acceptance scenario still to be run; until it passes, upgrade with the jobs settled.

## Adding and removing nodes

A new node needs only its configuration, the unwrap key and the database DSNs: start it and it takes work. To remove a node, stop it: its unfinished transfers are adopted by the other nodes after its lease expires. Its row in `veho.executors` is removed by housekeeping after a week.

## Removal

On every node:

```bash
sudo systemctl disable --now veho
sudo apt remove veho        # or: sudo dnf remove veho
sudo rm -rf /etc/veho
```

Then drop the database `veho` and the roles `veho_migrator`, `veho_api`, `veho_worker` and `veho_dbos`. Objects already copied to the storage systems stay there: they are the tenants'.
