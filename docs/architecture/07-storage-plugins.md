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
| `s3` | `{"endpoint": ..., "region": ..., "bucket": ..., "path_style": ...}` | Any S3-compatible service; multipart uploads; each part's MD5 is sent as `Content-MD5`, so the service verifies it; parts of 5 to 512 MiB, at most 10,000; an optional `storage_class` is sent on every write and read back after every commit, so a backend that ignores it is caught; archive classes refused |

Both pass the same **conformance suite**, which any driver must pass: idempotent commits, resumes from a token in another process, refusal of a changed source, classification of errors, and the rest of the contract. The S3 driver passes it on Garage, an S3-compatible service, in the lab.

## Process plugins

A driver can also run as a separate process, loaded with HashiCorp's go-plugin over gRPC: `plugins[]` in `veho.yaml` names its executable and its SHA-256, and veho refuses to start a file whose hash differs. A crashed plugin process is restarted. The process boundary keeps a third-party driver out of the core's memory and away from the other tenants' credentials in use.

Measured in the lab, a transfer through a process plugin ran at 325 MB/s against 380 MB/s compiled in, so performance does not decide between the two; whether the shipped drivers stay compiled in is open.

## Credentials

A driver receives the endpoint's credentials (a JSON object from the tenant's secret) on each `Open`, and must neither store nor log them. The core fetches and decrypts them when the transfer opens the endpoint and zeroes its copy afterwards.
