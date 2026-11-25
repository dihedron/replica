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

| Driver | Configuration | Notes |
| :------- | :-------------------------------------------- | :-------------------------------------------------------- |
| `file` | `{"root": "/srv/replicas"}` | A directory; uploads are staged and renamed into place |
| `s3` (plugin) | `{"endpoint": ..., "region": ..., "bucket": ..., "path_style": ...}` | Any S3-compatible service; multipart uploads; each part's MD5 is sent as `Content-MD5`, so the service verifies it; parts of 5 to 512 MiB, at most 10,000; an optional `storage_class` is sent on every write and read back after every commit, so a backend that ignores it is caught; archive classes refused |

Both pass the same **conformance suite**, which any driver must pass: idempotent commits, resumes from a token in another process, refusal of a changed source, classification of errors, and the rest of the contract. The S3 driver passes it on Garage, an S3-compatible service, in the lab.

## Process plugins

Only the drivers that need nothing but Go's standard library are compiled into veho (`file`, and `http` to come). Every driver with dependencies of its own is a separate binary, `veho-plugin-<name>`, run as a process plugin with HashiCorp's go-plugin over gRPC: `s3` is the first, so the core carries no AWS SDK. A crashed plugin process is restarted. The process boundary keeps a driver out of the core's memory.

- **Which drivers run.** `drivers[]` in `veho.yaml` lists them; a driver not listed is not available.
- **Trust.** A plugin is installed by its package with a manifest (name, its version, the versions of veho it supports, SHA-256, protocol) signed with its publisher's Ed25519 key. The publishers' public keys are files in one directory, `/etc/veho/plugin-keys/`; the release's is `veho.pub`, from the `veho` package, and no key is compiled into veho. A driver names the key that signs it, `veho` by default, so a publisher's key signs only the drivers that name it. At start veho checks that the files and keys are root's, the signature, the name, and that its own version is in the manifest's range, written in interval notation (`[0.5.0,0.6.0)`); the release signs its plugins with its own version alone (`[0.5.0]`), so that an older plugin cannot be put back. go-plugin then checks the SHA-256 before running the file. A plugin can also be pinned by its path and SHA-256 in `veho.yaml`, without a manifest.
- **Settings.** Each driver gets its own `settings` from `veho.yaml`, and nothing else of the configuration: a plugin receives them in its environment at start and refuses keys it does not know, which stops the node with the plugin's message.

Measured in the lab, a transfer through a process plugin ran at 325 MB/s against 380 MB/s compiled in: about 15 %, far above what one transfer gets over a WAN link.

## Credentials

A driver receives the endpoint's credentials (a JSON object from the tenant's secret) on each `Open`, and must neither store nor log them. The core fetches and decrypts them when the transfer opens the endpoint and zeroes its copy afterwards.
