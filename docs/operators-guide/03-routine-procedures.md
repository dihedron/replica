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

## Drivers

A node offers the drivers `drivers[]` lists in `veho.yaml`, and no other: an endpoint of a driver no node lists cannot be created. List the same drivers on every node. A driver of the release other than `file` comes in its own package (`veho-plugin-<name>`), signed with the release's key. A third-party plugin is either signed by its publisher, whose key you name, or pinned by its executable and SHA-256:

```yaml
drivers:
  - name: file
  - name: s3                # the release's key, /etc/veho/plugin-keys/veho.pub
  - name: acme              # signed by its publisher
    key: acme               # /etc/veho/plugin-keys/acme.pub
  - name: beta              # pinned
    path: /opt/beta/veho-plugin-beta
    sha256: 9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08
```

- **A signed plugin.** The publisher's package installs `veho-plugin-<name>`, its `.manifest` and `.manifest.sig` in `/usr/libexec/veho/plugins/`, and its public key as `/etc/veho/plugin-keys/<publisher>.pub` (or you copy the key there). The key signs only the drivers that name it in `veho.yaml`. The manifest says which veho versions the plugin supports; upgrading veho outside that range stops the node until you upgrade the plugin.
- **A pinned plugin.** After you replace the file, update the SHA-256 and restart the node.

Plugin files, key files and their directories must be root's and writable only by root.

### Sources and their settings

| Kind | Driver | Endpoint configuration | Credentials |
| :--- | :--- | :--- | :--- |
| HTTP, HTTPS | `http` (built in, source only) | `{"base_url": "https://images.example/pub/"}` | `bearer_token`, or `username` and `password`; only over HTTPS |
| SFTP, SCP | `sftp` (source and destination) | `{"host": "files.example", "root": "/srv/images", "host_key": "ssh-ed25519 AAAA..."}` | `username` and `private_key` (with `passphrase`) or `password` |
| FTP, FTPS | `ftp` (source only) | `{"host": "ftp.example", "root": "/pub/images", "tls": "explicit"}` | `username` and `password`, or none for anonymous FTP |
| A directory | `file` | `{"root": "/mnt/images/tenant-a"}` | none |
| OpenStack Glance | `glance` (source and destination) | `{"auth_url": "https://keystone.example:5000/v3", "region": "RegionOne", "upload": "put", "disk_format": "qcow2", "container_format": "bare"}` | `application_credential_id` and `application_credential_secret`, or `username`, `password` and `project_name` (with `user_domain_name` and `project_domain_name`, default `Default`) |
| Sonatype Nexus 3 | `nexus` (source of any repository, destination of raw hosted ones) | `{"url": "https://nexus.example", "repository": "images"}`, with `format` optional | `username` and `password` (a user token's name and passcode), or none for anonymous reading |
| OpenStack Swift | `swift` (source and destination) | `{"auth_url": "https://keystone.example:5000/v3", "region": "RegionOne", "container": "images"}` | as Glance |

- **The dial guard.** veho never connects a source to a loopback, private, link-local or CGNAT address, nor to the cloud metadata service, whatever the endpoint's host resolves to. For storage on a private network, list its ranges in the driver's settings: `drivers: [{name: s3, settings: {allow_cidrs: [10.20.0.0/16]}}]` for an S3 service (such as Garage or MinIO) on the platform's network, the same for `sftp` and the others. A tenant cannot change them. Every driver connects directly and follows no redirect; the environment's `HTTP_PROXY`, `HTTPS_PROXY` and `NO_PROXY` are ignored.
- **Through a forward proxy.** Where the nodes reach the Internet only through a corporate proxy, and may not even resolve external names, the `s3`, `http` and `nexus` drivers can send chosen names through it: `drivers: [{name: s3, settings: {allow_cidrs: [10.20.0.0/16], proxy: {url: "http://proxy.corp.example:3128", hosts: ["*.amazonaws.com"], credentials_file: /etc/veho/proxy-credentials}}}]`. An `https` endpoint whose host name matches `hosts` (an exact name, or `*.` and a suffix of at least two labels) goes through the proxy by `CONNECT`; veho does not resolve the name, only the proxy does, and TLS is verified end to end for that name. Everything else (an IP address, a name not listed, plain `http`) is dialled directly through the dial guard, so the on-premise source and the AWS destination of a backup each take their own path. The proxy's URL is `http://`; its optional credentials file holds `user:password`, must be mode 0600 and owned by root, and is sent to the proxy only. List provider names or exact names in `hosts`, never a domain a tenant controls: a listed name that resolves, at the proxy, to an internal host would be reached (R-20); let the proxy refuse internal destinations too. A refusal of the proxy (`403`, `407`) fails the transfer at once; a failing proxy (`502`, `503`) is retried. `endpoint check` probes such a flow through the proxy, `tls via proxy`.
- **HTTP.** The server must answer range requests (`Accept-Ranges: bytes`) and give a strong `ETag` or a `Last-Modified`; otherwise the job fails with a message that says so. Redirects are not followed.
- **SFTP.** `host_key` is required: take it from a trusted copy of the server's key (`ssh-keyscan` over a network you trust, then compare its fingerprint). An `scp://` source is an `sftp` endpoint. Symbolic links under the root are not followed.
- **FTP.** FTPS (`explicit`, or `implicit` on port 990) by default. `tls: none` is refused unless the driver's `plaintext` setting allows it, because the password crosses the network in clear.
- **Directories.** A `file` endpoint's root must be under one of the driver's `roots`, and every node that may run a transfer must mount the same directory at the same path: `drivers: [{name: file, settings: {roots: [/mnt/images]}}]`.
- **OpenStack.** `auth_url` must be `https`; `interface` (`public`, `internal`, `admin`) and `region` choose the service in Keystone's catalog, and the catalog's addresses go through the dial guard too, so a private cloud's ranges go in the driver's `allow_cidrs`. `ca_cert` adds a private CA. Prefer application credentials: they are bound to one project and can be revoked alone. Queens (2018) is the oldest release supported; some features need a later one (below).
- **Glance as a source.** The source path is the image's ID. The image must be active; its version is Glance's hash (`os_hash_value`, or the MD5 `checksum` before Rocky), so a deactivated, deleted or re-uploaded image fails the transfer as changed.
- **Glance as a destination.** The destination path is the image's name, which may not be a UUID. `upload: put` sends the data to the image (any release); `glance-direct` stages it and imports it, and is the one that takes `stores` (Queens for the import, Rocky or later with multiple stores for `stores`; `["*"]` is every store). The image is created hidden (`os_hidden`, Rocky) and shown only once it is active and its hash matches the bytes sent; before Rocky it uploads under the name `veho-upload-<id>` and is renamed. The project's users can read a hidden image by its ID while it uploads (accepted, R-16). At commit, the project's other active images of that name are deleted: overwrite `fail` and `skip` check before the upload, `replace` replaces. Glance cannot resume an upload: a failed attempt starts the image again from its first byte (R-17).
- **Glance's `web-download`.** Glance fetches the image itself from a URL veho gives it, so the bytes do not cross veho: an S3 source gives a presigned GET, an HTTP source its URL (only without credentials or headers); other sources fail the transfer. The URL lives `web_download_url_ttl` (default `1h`), is given to Glance only and never logged. Glance must reach the source, and its `[import_filtering_opts]` must allow its host and port. veho then reads the image back while it is still hidden and compares it with the source (it first reads the source when no other copy of the job has been verified), so the object is read twice and nothing is uploaded; a copy that differs is never published.
- **Swift.** Objects are written as static large objects: each part a segment in `<container>_segments` (or `segment_container`), under the object's name and an upload ID, made visible by the manifest at commit; parts are parallel and resumable as with S3. Overwriting an object deletes the old object's segments. `storage_policy` creates the containers with that policy and refuses existing ones with another. A cluster whose `max_manifest_segments` is not 1,000 sets the driver's `max_segments`.
- **Nexus.** A key is the content's path in the repository (`/repository/<repository>/<key>`), which every repository serves, so a source may be a hosted, proxy or group repository of any format; a destination must be a `raw` hosted repository, and its user needs the right to add assets (and to delete them, for `veho` to remove a copy that failed its check). `format` names the format the endpoint expects: `raw` is checked against the repository; `maven2`, `npm` and `pypi` are planned and refused for now. A repository that does not allow redeploying refuses to overwrite an asset: the transfer fails with a message that says so. An upload cannot resume: a failed attempt starts again from the first byte (R-18). The version of an asset is its `ETag`, the SHA-1 of its content, checked on every range.
- **Changes during a copy.** SFTP and FTP give a modification time to the second: a source rewritten within the same second with the same size is not detected, and the copies could mix two versions. Prefer HTTP with a strong `ETag`, or S3, for sources that change in place.

A publisher signs its plugins with `plugin-manifest` from veho's sources: `go run ./tools/plugin-manifest genkey acme.key` writes the private key and prints the public one; `PLUGIN_SIGNING_KEY="$(cat acme.key)" go run ./tools/plugin-manifest sign -name acme -version 2.1.0 -veho '[0.5.0,0.6.0)' veho-plugin-acme` writes the manifest and its signature next to the binary. The range is in interval notation: `[` and `]` include the bound, `(` and `)` exclude it, an empty bound is unbounded (`[0.5.0,)`). Its parser is `github.com/dihedron/veho/pkg/semver`, for publishers' own tools.

## Storage classes

The storage class of the copies is a property of the endpoint: the `s3` driver's `storage_class` key. To write copies in two classes to one bucket, register two endpoints on it, for example `eu2-images` and `eu2-images-ia` with `"storage_class": "STANDARD_IA"`, and let the policies choose between them. Without the key, veho sends no class and the service uses its default.

- The archive classes `GLACIER` and `DEEP_ARCHIVE` are refused: their copies cannot be read without a restore. `GLACIER_IR` is accepted.
- The class is read back after every commit; a missing header counts as `STANDARD`. A service that ignores the class fails the endpoint test (`stored as STANDARD, not STANDARD_IA`, at the `write` step), so run `veho endpoint test` after setting one.
- Garage accepts the header and ignores it: on Garage, leave `storage_class` unset or use `STANDARD`.
- A transfer that meets a different class fails permanently, and the copy is left in place under its final name: deleting it is the client's choice.
- Lifecycle rules of the bucket can move copies to another class later; veho does not watch them.

## Rotating the key-encryption key

Rotate the KEK periodically, or when a node may have been compromised. *As `veho_migrator`*, from a host with the unwrap key:

```bash
veho kek rotate --unwrap-key file:./unwrap.key    # new active version; the old one is "retiring"
veho kek show
```

New secrets use the new KEK at once, and the nodes pick it up without a restart. Housekeeping then re-wraps the existing data keys in the background, 500 per pass (every 30 s), and retires the old version once no secret uses it (`KEK_REWRAP` audit events). To finish at once instead, run `veho kek rewrap --unwrap-key file:./unwrap.key` (batches of `--batch`, default 500; it can be run again if interrupted).

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

Upgrade veho and its plugin packages together (`apt upgrade`, `dnf upgrade`): a node refuses to start a plugin of another release.

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
