# veho: the Nexus driver

This file specifies the `nexus` driver: Sonatype Nexus Repository 3 as a source and a destination. It is part of `veho_specification.md`; the driver contract is `veho-plugins.md`, the source rules `veho-sources.md`, the plugin distribution `veho-plugin-distribution.md`. It follows the `http` driver (`veho-sources.md`) and the Glance `put` method (`veho-openstack.md` §3.2), whose patterns it reuses.

Status: approved, 2026-11-25; revised the same day with the repository formats (§2.1). Implemented the same day for `raw` (and no format) with NX-T1 to NX-T3 and the conformance suite against a fake Nexus; the lab node (§6, NX-T4, NX-T5) next.

## 1. Decisions (2026-11-25)

1. **A plugin**, `veho-plugin-nexus`, over the standard library (`net/http`): Nexus's content and REST APIs are plain HTTP. It is a plugin, not built in, because it is a vendor's product with its own release cycle (`veho-plugin-distribution.md`).
2. **Nexus Repository 3 only** (Community Edition and Pro): Nexus 2 is out of support. The lab tests the current Community Edition.
3. **Source: any repository, any format**, by the path of the content (`/repository/<repo>/<path>`): hosted, proxy and group repositories of every format serve their content at that path.
4. **Destination: `raw` hosted repositories first.** Maven, npm and Python (PyPI) repositories are planned: each has its own key syntax and upload protocol, chosen by the endpoint's `format` (§2.1).
5. **Credentials:** a user and password, or a user token (its name and passcode), sent with HTTP Basic, only over HTTPS. No anonymous upload.
6. **The format is in the endpoint, not in the driver.** One endpoint names one repository, and a repository has one format, fixed by Nexus: so the endpoint says which format it expects (`format`), the driver checks that the repository has it, and the format decides what a job's key means and how an object is written. One plugin serves every format; the driver's capabilities (source, sink, not resumable) are the same for all of them.

## 2. Endpoint

```json
{"url": "https://nexus.example", "repository": "images", "ca_cert": "-----BEGIN CERTIFICATE-----..."}
```

- `url` is Nexus's base URL (with its context path, if any); `https`, unless the driver's `plaintext` setting allows `http` (the lab).
- `repository`: the repository's name. The key is the path in it; `ParseRef` takes `nexus://<repository>/<path>`.
- Settings, in `drivers[].settings`: `allow_cidrs`, `plaintext` (default `false`). Every connection goes through the dial guard (SRC-3); no redirects are followed (SEC-13), so credentials never leave `url`'s host.
- Keys are checked as `http`'s (`locate`): each segment escaped, `.` and `..` refused, no empty segment.

### 2.1 Formats

```json
{"url": "https://nexus.example", "repository": "releases", "format": "maven2"}
```

`format` is optional. Without it, a key is a **content path** (`/repository/<repo>/<key>`), which every format serves: the endpoint is then a source of any repository, and a destination only of a `raw` one. With it, the driver checks at `Open` that the repository has that format (`GET /service/rest/v1/repositories/<repo>`, which a user with read access may call), and keys follow the format's syntax:

| `format` | Key | Source | Destination | Status |
| :--- | :--- | :--- | :--- | :--- |
| (none) | the content path | any repository | `raw` hosted only | first |
| `raw` | the path | `HEAD`/`GET` of the path | `PUT` of the path (§4) | first |
| `maven2` | a path in the Maven layout (`org/acme/app/1.2/app-1.2.jar`), or coordinates `group:artifact:version[:classifier]@extension` (`org.acme:app:1.2@jar`), mapped to that path | the path | `PUT` of the artifact; Nexus computes the checksums and `maven-metadata.xml`; the repository's version policy (release, snapshot) refuses the other kind (`Permanent`); a POM is copied as its own object (`@pom`) | planned |
| `npm` | `package@version` or `@scope/package@version` | the tarball named by the packument (`GET /repository/<repo>/<package>`: `versions[v].dist.tarball`); the version is the tarball's `dist.integrity` | `PUT /repository/<repo>/<package>` of a publish document carrying the tarball in base64: held in memory, bounded by the setting `max_npm_size` (default 100 MiB) | planned |
| `pypi` | a distribution's file name (`pkg-1.0-py3-none-any.whl`, `pkg-1.0.tar.gz`, PEP 427 and PEP 625 names) | the file the simple index links (`GET /repository/<repo>/simple/<project>/`), its `#sha256=` the version | `POST /repository/<repo>/` as `twine` does (multipart, `:action=file_upload`, name and version from the file name, `sha256_digest`) | planned |

- Until a format is implemented, an endpoint naming it is refused at `Open` (and at `endpoint create`, through `CheckConfig` when the driver runs in process) with a `Permanent` error that says so.
- Every format's destination is one request per attempt, invisible until it ends (Nexus creates the component when the request completes), not resumable (R-18), and checked after the upload against the checksum Nexus reports (§4).
- `ParseRef` takes `nexus://<repository>/<key>`; a key's syntax is checked by the endpoint's format at `Open`'s client, not by `ParseRef`, which does not know the endpoint.
- Adding a format is a change to the driver and its tests only: its key mapping, its source lookup, its upload, and a lab repository of that format.

## 3. Source

- `Stat`: `HEAD /repository/<repo>/<path>`: the size from `Content-Length`; the version is the `ETag`, which Nexus derives from the content's SHA-1 (`"{SHA1{<hex>}}"`), so it changes with the content. A `HEAD` without an `ETag` is a `Permanent` error.
- `ReadRange`: `GET` with `Range`; only `206` with the exact `Content-Range` is accepted, and the response's `ETag` must equal the version, else `SourceChanged` (Nexus does not promise to honour `If-Match`, so the driver checks the answer instead). A Nexus that answers `200` fails `Stat`'s range probe with a `Permanent` error.
- `PresignRead` (for a Glance `web-download`, `veho-openstack.md` §3.2): the content URL, only when the endpoint has no credentials (an anonymous repository); otherwise not offered.
- A proxy repository fetches from upstream on a miss: the first `Stat` may be slow, and the upstream's content is what Nexus serves. veho does not see the upstream.

## 4. Destination

- One `PUT /repository/<repo>/<path>` per attempt: `Begin` opens it, chunked, the parts are written into it in order, `Commit` closes it. Nexus stores the blob and creates the asset only when the request ends, so nothing is visible before `Commit` and an aborted request leaves nothing (PLG-8).
- **Capabilities:** sink, not resumable, not parallel; a new attempt begins again from byte zero (R-18). `MaxParts` is unbounded; the part size is the engine's.
- **Integrity:** the driver computes the SHA-1 of the bytes it sends and compares it, at `Commit`, with the `ETag` of a `HEAD` of the new asset; a mismatch is `Integrity`. SHA-1 detects corruption, which is the threat (Nexus is the operator's own service); the engine's SHA-256 digest and size check apply as for every destination.
- **Overwrite:** the repository's write policy decides. `overwrite: never` makes `Begin` refuse an existing path (`Stat`); with `replace`, the `PUT` replaces it, which Nexus refuses on an "allow redeploy: no" repository with `400`, reported as a `Permanent` error that names the policy.
- `Delete`: `DELETE /repository/<repo>/<path>`.
- `Begin` checks, once per endpoint, that the repository exists and is `raw` and `hosted` (`GET /service/rest/v1/repositories/<repo>`), with a `Permanent` error otherwise.

## 5. Threat model

| ID | Threat | Mitigation |
| :--- | :--- | :--- |
| NX-1 | **SSRF**: an endpoint's `url` on veho's internal network. | The dial guard after resolution (SRC-3); private ranges only from `allow_cidrs`; no redirects. |
| NX-2 | **Credentials sent in clear or elsewhere.** | HTTPS required (unless `plaintext`, lab only); no redirects; `ca_cert` adds trust, never removes verification. |
| NX-3 | **A half-written asset is used.** | Nexus creates the asset only when the `PUT` ends; the driver checks the SHA-1 after it. |
| NX-4 | **The content changes while it is read** (a redeploy, a proxy refreshing from upstream). | The `ETag` of every range answer is compared with the version: `SourceChanged`. |
| NX-5 | **Path traversal** in the key. | `locate`'s checks (§2), fuzzed. |

New residual risk:

- **R-18** Nexus cannot resume an upload: a failed attempt restarts from zero. Mitigated by the engine's retries; accepted (as R-17 for Glance).

## 6. Lab

A Nexus Repository Community Edition node in the veho lab (`test/lab`): an LXD container `nexus` from Sonatype's Unix archive (with its bundled Java), a `raw` hosted repository with redeploy allowed and one with it refused, a test user with a user token, and Nexus's own TLS through a reverse proxy with the lab's CA. `lab.sh nexus up|down` builds and removes it, so the rest of the lab does not need its memory (about 3 GiB).

### 6.1 Order of the formats

`raw` (with no `format`, the content path) first; then `maven2`, `npm` and `pypi`, each on demand and with a revision of this table: its key syntax, the lab repository and its tests (NX-T6 for Maven, NX-T7 for npm, NX-T8 for PyPI), and the threat rows its upload adds (a publish document parsed by Nexus, a multipart form).

## 7. Tests (written first)

| ID | Test | Kind |
| :--- | :--- | :--- |
| NX-T1 | `ParseRef`, `locate`, the settings and the endpoint's checks; `format`: absent, `raw`, a repository of another format refused, a planned format refused as not implemented | unit, fuzz |
| NX-T2 | Source against a fake Nexus: the version from the `ETag`, a changed `ETag` `SourceChanged`, a `200` to a range refused, `PresignRead` only without credentials | unit |
| NX-T3 | Destination against a fake: the stream in order, the SHA-1 compared, `overwrite: never` and a refused redeploy, a repository not `raw` or not `hosted` refused | unit |
| NX-T4 | The conformance suite, source and sink, against the lab's Nexus | lab |
| NX-T5 | End to end: a job from Nexus to Garage, and from S3 to Nexus | lab |

## 8. Documentation

The Setup Guide (package, settings), the Operator's Guide (endpoints, credentials, write policy, R-18), the Architecture and Design Guide (the drivers table, the threat rows), `veho-threat-model.md` (NX-1 to NX-5, R-18).

## 9. Order of work

After the OpenStack drivers' step 1 (the shared pieces of `pkg/plugin/http` it reuses: `locate`, the range checks): the driver with NX-T1 to NX-T3, then the lab node and NX-T4 and NX-T5.
