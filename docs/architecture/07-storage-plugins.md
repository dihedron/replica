# Storage plugins

## The driver contract

Every storage back end is a **driver** behind one interface, the same whether it is compiled into veho or runs as a separate process:

- **Source**: `Stat` (size, version, checksums) and `ReadRange` (a byte range, failing as `SourceChanged` if the version differs).
- **Sink**: `Begin` an upload, `Resume` one from its serialised token, `WritePart`, `ListParts`, `Commit` (atomic, idempotent), `Abort`, `Delete`.
- **Capabilities**, which the core adapts to: parallel and out-of-order parts, resumable uploads, native checksums, the minimum and maximum part size and the maximum number of parts.
- **Error classes**: every error a driver returns is classified (`Transient`, `Permanent`, `ResumeInvalid`, `Integrity`, `SourceChanged`, `Unknown`); the core decides what to do by class.

Two rules keep drivers safe to retry: an upload is invisible under its final name until it is committed, and writing the same part again with the same bytes is harmless. A driver that cannot write parts out of order declares it, and the core writes them one at a time.

## Data through the core

A transfer's bytes flow from the source driver, through the core, to the destination driver. The core computes each part's SHA-256 on the way, so the digest does not depend on any driver's honesty; a driver's own figure is a cross-check. This also lets a source of one kind feed a destination of another.

## The shipped drivers

Each driver of the release, with an endpoint's configuration and what the driver does with it. A driver with dependencies of its own is a plugin process (§7.5); `file` and `http` are built in.

| Driver | Configuration | Notes |
| :------- | :-------------------------------------------- | :-------------------------------------------------------- |
| `file` | `{"root": "/srv/replicas"}` | A directory on the node, typically an NFS mount; uploads are staged and renamed into place; the root must be under the driver's `roots` setting |
| `http` | `{"base_url": "https://images.example/pub/", "headers": {...}}` | A source only, built in; range requests conditional on the strong `ETag` (else `Last-Modified` and the size); no redirect; credentials only over HTTPS |
| `sftp` (plugin) | `{"host": ..., "port": 22, "root": "/srv/images", "host_key": "ssh-ed25519 AAAA..."}` | A source and a destination, also for `scp://`; the host key pinned; uploads staged and renamed into place with `posix-rename`; symbolic links not followed |
| `ftp` (plugin) | `{"host": ..., "port": 21, "root": "/pub/images", "tls": "explicit"}` | A source only; FTPS by default; passive mode, the data connection only to the control connection's address |
| `glance` (plugin) | `{"auth_url": ..., "region": ..., "upload": "put", "disk_format": "qcow2", "container_format": "bare"}` | A source by image ID (the version is Glance's hash), a destination by image name: one request per attempt (`put` or `glance-direct` with `stores`), the image hidden until active and its hash compared with the bytes sent; not resumable |
| `nexus` (plugin) | `{"url": ..., "repository": "images", "format": "raw"}` | A source and a destination of hosted repositories, one request per attempt, not resumable; the version is the asset's `ETag` (its SHA-1), checked on every range and after every upload; the formats below |
| `swift` (plugin) | `{"auth_url": ..., "region": ..., "container": "images"}` | A source and a destination: static large objects, each part a segment of the upload, the manifest written at commit; resumable and parallel |
| `s3` (plugin) | `{"endpoint": ..., "region": ..., "bucket": ..., "path_style": ...}` | Any S3-compatible service, through the dial guard (a private service in `allow_cidrs`); multipart uploads; each part's MD5 is sent as `Content-MD5`, so the service verifies it; parts of 5 to 512 MiB, at most 10,000; an optional `storage_class` is sent on every write and read back after every commit, so a backend that ignores it is caught; archive classes refused |

**Nexus's formats.** The endpoint's `format` decides what a key means and how an asset is written: none or `raw`, the content path; `maven2`, a Maven path or `group:artifact:version[:classifier]@ext`, the checksums and the metadata left to Nexus; `npm`, `package@version`, published as npm's document (the tarball held in memory up to `max_npm_size`, its `package.json` checked against the key); `pypi`, a wheel's or an sdist's file name, uploaded as `twine` does. An npm or PyPI source is found through its packument or simple index, whose links must stay in the repository, and its content is compared once with the index's digest.

All pass the same **conformance suite**, which any driver must pass: idempotent commits, resumes from a token in another process, refusal of a changed source, classification of errors, keys confined to the endpoint's root, and the rest of the contract; a source-only driver runs its source cases. The S3 driver passes it on Garage, an S3-compatible service, in the lab.

## Sources

A source is an endpoint of any driver that can read ranges, and a path in it. The engine calls `Stat` once when a job is resolved, records the size and the version, and reads each part with `ReadRange` at that version. Every source driver keeps four rules:

- **SRC-1** `Stat` gives a version that changes whenever the content can have changed: the strong `ETag` for HTTP, the size and the modification time for SFTP and FTP (one second of precision at worst: a rewrite within the same second, with the same size, is not seen).
- **SRC-2** `ReadRange` returns exactly the bytes asked for, of that version, or fails as `SourceChanged`; SFTP and FTP check the version again after each read.
- **SRC-3** Every connection to a host a tenant names goes through the **dial guard** (`internal/netguard`, shared with the webhooks): the address actually dialled, after resolution, must not be loopback, link-local, private, CGNAT or multicast, unless the driver's `allow_cidrs` setting opens it; the cloud metadata addresses never.
- **SRC-4** The driver passes the conformance suite as a source.

**Through a forward proxy.** The `s3`, `http` and `nexus` drivers can send chosen names through the operator's forward proxy, for nodes that reach the Internet, and resolve external names, only through it (`drivers[].settings.proxy`). `internal/netguard.Proxy` routes a request: `https` to a name the operator lists in `proxy.hosts` goes through `CONNECT`, not resolved by veho, with TLS verified end to end for that name; an IP address, any other name and plain `http` are dialled directly through the guard. A tenant's endpoint therefore cannot make the proxy reach the metadata service or an internal address by its number; a listed name that the proxy resolves to an internal host is the residual risk R-20. The environment's `HTTP_PROXY` and `HTTPS_PROXY` are never used.

A job is refused at submission, with `invalid_endpoint`, when its source's driver cannot be a source that reads ranges, or a destination's cannot be a destination. Sources that cannot read ranges or give a version (an HTTP server without `Accept-Ranges` or validators) are refused: a copy of a 10 GB image that restarts from zero at every failure is not acceptable.

## Process plugins

Only the drivers that need nothing but Go's standard library are compiled into veho (`file` and `http`). Every driver with dependencies of its own is a separate binary, `veho-plugin-<name>`, run as a process plugin with HashiCorp's go-plugin over gRPC: `s3`, `sftp`, `ftp`, `glance`, `swift` and `nexus`, so the core carries neither the AWS SDK nor the SSH, FTP and OpenStack libraries. The destinations that take an upload in one request (Glance's `put` and `glance-direct`, Nexus) share `pkg/plugin/stream`: the parts go in order into one request, a part written again with the same bytes is harmless, and an upload whose request failed begins again. Two optional interfaces serve destinations that fetch the object themselves (Glance's `web-download`): a source's `Presigner` gives a URL that needs no credentials for a time (`s3`, `http`), and an upload that is a `Puller` takes that URL in place of the parts; the engine then reads the pulled object back by parts, still invisible, and compares their digest with the source's before Commit. Both cross the plugin protocol (`PresignRead`, `Pull`, and the upload handle's pull TTL). The OpenStack drivers share `pkg/plugin/openstack`: Keystone v3 with application credentials or a password, the service from the catalog, and an HTTP client with the dial guard, the endpoint's CA and no redirects. A crashed plugin process is restarted. The process boundary keeps a driver out of the core's memory.

- **Which drivers run.** `drivers[]` in `veho.yaml` lists them; a driver not listed is not available.
- **Trust.** A plugin is installed by its package with a manifest (name, its version, the versions of veho it supports, SHA-256, protocol) signed with its publisher's Ed25519 key. The publishers' public keys are files in one directory, `/etc/veho/plugin-keys/`; the release's is `veho.pub`, from the `veho` package, and no key is compiled into veho. A driver names the key that signs it, `veho` by default, so a publisher's key signs only the drivers that name it. At start veho checks that the files and keys are root's, the signature, the name, and that its own version is in the manifest's range, written in interval notation (`[0.5.0,0.6.0)`); the release signs its plugins with its own version alone (`[0.5.0]`), so that an older plugin cannot be put back. go-plugin then checks the SHA-256 before running the file. A plugin can also be pinned by its path and SHA-256 in `veho.yaml`, without a manifest.
- **Configurations.** An endpoint's configuration is validated when it is created or updated (PLG-10): the engine checks it against the driver's JSON Schema, compiled when the node starts with no reference outside it resolved (a schema cannot make the node fetch), then calls the driver's `CheckConfig`, which crosses the plugin protocol; each driver's `CheckConfig` is the part of its `Open` that needs neither credentials nor the network. `veho endpoint list --invalid` revalidates the stored ones.
- **Settings.** Each driver gets its own `settings` from `veho.yaml`, and nothing else of the configuration: a plugin receives them in its environment at start and refuses keys it does not know, which stops the node with the plugin's message.

Measured in the lab, a transfer through a process plugin ran at 325 MB/s against 380 MB/s compiled in: about 15 %, far above what one transfer gets over a WAN link.

## Credentials

A driver receives the endpoint's credentials (a JSON object from the tenant's secret) on each `Open`, and must neither store nor log them. The core fetches and decrypts them when the transfer opens the endpoint and zeroes its copy afterwards.
