# veho: source drivers

This file specifies the new kinds of source of backlog item 34: where the object to replicate can live, besides S3. It is part of `veho_specification.md`.

Status: approved, 2026-11-24; implemented 2026-11-25 (the lab tests SRC-T5 and SRC-T8 to run).

## 1. Model

A source is not a new kind of plugin. It is an endpoint of the tenant, of any driver whose `Capabilities` have `Source` and `RangeRead`, and a path in it (`veho-plugins.md`). The engine calls `Stat` once when the job is resolved, records the size and the version, and then reads each part with `ReadRange(ref, off, length, version)`, streaming it into the destination and hashing it on the way (`veho-engine.md` §4). Nothing in the engine changes for a new kind of source; each kind is a driver that keeps the `Source` contract:

- **SRC-1** `Stat` returns the size and a **version**: a value that changes whenever the content can have changed.
- **SRC-2** `ReadRange` returns exactly the bytes `[off, off+length)` of the object **as it was at that version**; when the object is no longer at that version, it fails with class `SourceChanged` (T-09). It never returns bytes of another version.
- **SRC-3** Every connection a driver opens to a host named by an endpoint goes through the **dial guard** (§4): loopback, link-local, private and metadata addresses are refused, checked on the address actually dialled.
- **SRC-4** Every driver passes the conformance suite as a source (`pkg/plugin/conformance`), extended with SRC-1 and SRC-2: a source changed between two `ReadRange` calls fails with `SourceChanged`.

## 2. Decisions

1. **Submission checks the source's capabilities** (fixes a gap found 2026-11-24). A job whose source endpoint's driver lacks `Source` or `RangeRead`, or a destination whose driver lacks `Sink`, is refused with `400` `invalid_endpoint` (as the other codes of a refused policy, `veho-api-server.md`), instead of failing during the copy.
2. *Proposed:* **Stream-only sources are refused in v1.** A source must range-read and give a version (SRC-1, SRC-2); a server that cannot (an HTTP server without `Accept-Ranges: bytes`, or without `ETag` and `Last-Modified`) fails `Stat` with a `Permanent` error that says why. Without them a copy could not resume after a crash and could mix two versions; with images of 5 to 10 GB (OI-4) a copy that always restarts from zero is not acceptable.
3. *Proposed:* **The first kinds**, in this order: `http` (HTTP and HTTPS), `sftp` (which also serves the `scp` case, §3.3), `ftp` (FTPS by default), and the `file` driver made fit for production as a directory or NFS mount. `http` and `file` use only the standard library and are built into `veho`; `sftp` and `ftp` are plugins, `veho-plugin-sftp` and `veho-plugin-ftp` (`veho-plugin-distribution.md`, decision 1). Swift and Glance come together, with the OpenStack client (`gophercloud`), when backlog 21 is unblocked; Azure Blob and GCS when someone needs them (each adds a large SDK).
4. *Proposed:* **Source-only, except `sftp` and `file`.** `http` and `ftp` are source-only (`Sink: false`); `sftp` and `file` are both, because a staged upload and an atomic rename work there as in the `file` driver.
5. *Proposed:* **No new platform flag per endpoint.** The dial guard allows extra ranges only through the platform configuration, in the `allow_cidrs` setting of each driver (`veho-plugin-distribution.md` §2), as `notifications.allow_cidrs` for webhooks; a tenant cannot set it. This replaces SEC-13's per-endpoint `allow_private` flag, which was never built.

## 3. Drivers

### 3.1 `http`

```json
{"base_url": "https://images.example/pub/", "headers": {"Accept": "*/*"}}
```

- The path of the job is resolved against `base_url` and must stay under it (no `..`, no other host).
- Credentials (optional, from the secret store): `bearer_token`, or `username` and `password` for basic authentication, sent only to the host of `base_url`.
- `Stat`: `HEAD`; the size from `Content-Length`; `Accept-Ranges: bytes` required; the version is the **strong** `ETag`, else `Last-Modified` with the size (`lm:<http-date>:<size>`). A weak `ETag` (`W/…`) alone is not a version.
- `ReadRange`: `GET` with `Range: bytes=off-end` and `If-Match: <etag>` (or `If-Unmodified-Since` with a `Last-Modified` version); only `206` with the exact `Content-Range` is accepted; `412` is `SourceChanged`; the response's validators are compared with the version too, for servers that ignore the conditional headers.
- No redirects (SEC-13, T-29); a `3xx` is a `Permanent` error naming the location. HTTPS certificates are verified; no proxy.

### 3.2 `sftp`

```json
{"host": "files.example", "port": 22, "root": "/srv/images", "host_key": "ssh-ed25519 AAAA..."}
```

- `host_key` is required: the server's public key, as in `known_hosts`. veho never trusts on first use.
- Credentials: `username` and either `private_key` (PEM, optionally with `passphrase`) or `password`.
- Every path is confined to `root` (cleaned, no `..`, symbolic links not followed out of it where the server reports them).
- `Stat`: size and modification time; the version is `<size>-<mtime>`. SFTP gives the time in seconds, so a change within the same second that keeps the size is not detected: accepted as R-15.
- `ReadRange`: `Stat` again and compare with the version, then read at the offset (`ReadAt`); after the read, `Stat` once more, so a change during the read is `SourceChanged`.
- As a sink: upload to `<root>/.veho-staging/<upload>/<part>`, concatenate on the server at commit into a temporary name and rename into place (`posix-rename@openssh.com` when the server has it, which replaces atomically); resumable from the token, as the `file` driver.

### 3.3 `scp`

SCP has no way to read from an offset, so a copy could not resume or read parts in parallel. Every OpenSSH server that offers `scp` also offers SFTP on the same port with the same credentials (OpenSSH's own `scp` uses SFTP since 9.0), so an `scp` source is registered as an `sftp` endpoint. `ParseRef` of the `sftp` driver accepts `scp://` and `sftp://`.

### 3.4 `ftp`

```json
{"host": "ftp.example", "port": 21, "root": "/pub/images", "tls": "explicit"}
```

- `tls`: `explicit` (default; `AUTH TLS`), `implicit` (port 990), or `none`, accepted only when the driver's settings allow it (`plaintext: true`), because the password would cross the network in clear.
- Credentials: `username` and `password`; anonymous FTP with neither.
- Passive mode only; the data connection must go to the control connection's address (no FXP, a classic FTP bounce attack), and through the dial guard.
- `Stat`: `SIZE` and `MDTM` (`MLST` when the server has it); the version is `<size>-<mdtm>`, with MDTM's precision (one second at worst: R-15).
- `ReadRange`: `Stat` again and compare, `REST off` then `RETR`, read `length` bytes and close the data connection (`ABOR`); `Stat` once more after the read.

### 3.5 `file`

The `file` driver keeps its contract and becomes a production driver for a directory on the veho nodes, typically an NFS mount:

- `root` must be under one of the directories of the driver's `roots` setting (empty by default: no `file` endpoint can be created). A tenant cannot reach the node's file system outside them.
- Every node that may run a transfer must mount the same directory at the same path; the endpoint test checks it on the node that runs it.
- The version is `<size>-<mtime ns>`, as today; on NFS the time precision is the server's.

## 4. The dial guard

The dial guard of the webhooks (`internal/webhook`, `Guard`) moves to a package of its own, `internal/netguard`, shared by the webhooks, the built-in `http` driver and the `sftp` and `ftp` plugins (built from this repository, so they can import it):

- the address is checked in `net.Dialer.Control`, after resolution, so DNS rebinding cannot get past it;
- refused: loopback, link-local, private (RFC 1918, ULA), CGNAT, multicast, unspecified, and the metadata addresses (`169.254.169.254`, `fd00:ec2::254`), these last ones always;
- the driver's `allow_cidrs` setting opens extra ranges, for storage on a private network; it never opens the metadata addresses.

The `s3` driver stays outside the guard in this item: its endpoints are on private networks in most deployments (Garage in the lab), and guarding it is a separate decision (§9, point 1).

## 5. Configuration

The settings of each driver, in the `drivers` list (`veho-plugin-distribution.md` §2):

| Driver | Setting | Meaning |
| :--- | :--- | :--- |
| `http`, `sftp`, `ftp` | `allow_cidrs` | extra ranges the dial guard allows; never the metadata addresses |
| `ftp` | `plaintext` | allow endpoints with `tls: none`; default `false` |
| `file` | `roots` | directories a `file` endpoint's `root` may be under; empty: none |

## 6. Threat model

| ID | Threat | Mitigation |
| :--- | :--- | :--- |
| SRC-W1 | **SSRF**: a tenant points an `http`, `sftp` or `ftp` endpoint at an internal service or at the metadata service, and replicates what it answers to a destination the tenant reads (T-04). | The dial guard on every connection, the address checked after resolution (SRC-3, §4); extra ranges only from the driver's settings in the platform configuration; metadata never; no redirects; FTP data connections only to the control connection's address. |
| SRC-W2 | **A man in the middle** reads the credentials or alters the object (T-07). | HTTPS with verified certificates; SFTP with a pinned host key and no trust on first use; FTPS by default, plain FTP only when the platform allows it; the digest of the copies is computed by the core from the bytes read, so the destinations agree with each other (T-08). |
| SRC-W3 | **The source changes during the copy** and the copies mix two versions (T-09). | SRC-1 and SRC-2: conditional range requests for HTTP, a `Stat` before and after each read for SFTP and FTP; residual R-15 for one-second time stamps. |
| SRC-W4 | **Path traversal**: a job's path leaves the endpoint's `root` or `base_url` (SEC-14). | Paths cleaned and confined per driver; `file` roots only under the driver's `roots`; the conformance suite tries `..`, absolute paths and encoded separators. |
| SRC-W5 | **A hostile server** stalls the copy, answers more or fewer bytes than asked, or floods the node (T-12). | Deadlines on every call; exactly `length` bytes read, anything else is an error; the part memory budget is unchanged; FTP and HTTP responses limited to what was asked. |
| SRC-W6 | **Credentials sent to the wrong place**: an HTTP redirect, or a `base_url` that changes host (T-29). | No redirects; credentials only to the host of `base_url`; per-attempt credentials, never logged (PLG-5). |
| SRC-W7 | **A file endpoint reads the node itself**: `/etc`, the unwrap key, another tenant's directory. | No `file` endpoint unless its root is under one of the driver's `roots`; `os.Root` confinement as today; the service unit's `ProtectSystem=strict` and a dedicated user. Two tenants given the same root share it: the platform decides that. |

New residual risk:

- **R-15** SFTP and FTP give a modification time with a precision of one second at worst: a source rewritten within the same second, with the same size, between the `Stat` and a read, is not detected, and the copies can mix two versions. Mitigated by the `Stat` after each read; removed by HTTP with a strong `ETag`, or S3.

## 7. Tests (written first)

| ID | Test | Kind |
| :--- | :--- | :--- |
| SRC-T1 | Submission refuses a source without `Source`/`RangeRead`, a destination without `Sink`, and a driver not on the node, with `400` `invalid_endpoint` | integration |
| SRC-T2 | `netguard`: the address table (refused, allowed, metadata never), DNS rebinding through a resolver that changes its answer | unit |
| SRC-T3 | `http`: version from a strong ETag, from Last-Modified; weak ETag alone, no `Accept-Ranges`, `200` instead of `206`, a wrong `Content-Range`, a redirect: each refused; `412` and a changed validator are `SourceChanged`; credentials not sent to another host | unit, `httptest` |
| SRC-T4 | `sftp`: host key mismatch refused; confinement; `SourceChanged` when the file changes; the sink path (staging, rename, resume) | unit, in-process SSH server (`golang.org/x/crypto/ssh` and `pkg/sftp`'s server) |
| SRC-T5 | `ftp`: plain refused unless allowed; data connection to another address refused; `REST`/`RETR` ranges; `SourceChanged` | lab (vsftpd) |
| SRC-T6 | `file`: endpoint refused outside the `roots` setting | integration |
| SRC-T7 | The conformance suite, as a source, on every new driver; as a sink on `sftp` | unit and lab |
| SRC-T8 | End to end on the lab: a job from each new kind of source to the three Garage clusters | lab |
| SRC-T9 | Fuzz tests of the path confinement of `http`, `sftp` and `ftp` | fuzz |

## 8. Documentation

- Setup Guide: the settings of each driver and one endpoint example per kind.
- Operator's Guide: the kinds of source, the host key and TLS requirements, `allow_cidrs`, `roots`, R-15 in plain words.
- Architecture and Design Guide: the source contract (SRC-1 to SRC-4), the dial guard, the new drivers in the table of shipped drivers, the threat rows.
- `veho-threat-model.md`: SRC-W1 to SRC-W7 by reference, R-15; SEC-13 reworded (§2, decision 5).

## 9. Open points

1. Whether the `s3` driver should go through the dial guard too (it would need `allow_cidrs` in its settings for every private S3 service).
2. Sources that cannot range-read (decision 2): if one is needed later, it needs its own design (a whole-object stream, restarted from zero, and a staging copy on the node).
3. Swift, Glance, Azure Blob and GCS, as in decision 3.
