# veho: the Nexus driver

This file specifies the `nexus` driver: Sonatype Nexus Repository 3 as a source and a destination. It is part of `veho_specification.md`; the driver contract is `veho-plugins.md`, the source rules `veho-sources.md`, the plugin distribution `veho-plugin-distribution.md`. It follows the `http` driver (`veho-sources.md`) and the Glance `put` method (`veho-openstack.md` §3.2), whose patterns it reuses.

Status: draft, 2026-11-25, for approval (the user asked for the driver on 2026-11-25).

## 1. Decisions (proposed)

1. **A plugin**, `veho-plugin-nexus`, over the standard library (`net/http`): Nexus's content and REST APIs are plain HTTP. It is a plugin, not built in, because it is a vendor's product with its own release cycle (`veho-plugin-distribution.md`).
2. **Nexus Repository 3 only** (Community Edition and Pro): Nexus 2 is out of support. The lab tests the current Community Edition.
3. **Source: any repository, any format**, by the path of the content (`/repository/<repo>/<path>`): hosted, proxy and group repositories of every format serve their content at that path.
4. **Destination: `raw` hosted repositories only.** Other formats (Maven's checksums and metadata, npm's packument) need their own layout: later, on demand (backlog).
5. **Credentials:** a user and password, or a user token (its name and passcode), sent with HTTP Basic, only over HTTPS. No anonymous upload.

## 2. Endpoint

```json
{"url": "https://nexus.example", "repository": "images", "ca_cert": "-----BEGIN CERTIFICATE-----..."}
```

- `url` is Nexus's base URL (with its context path, if any); `https`, unless the driver's `plaintext` setting allows `http` (the lab).
- `repository`: the repository's name. The key is the path in it; `ParseRef` takes `nexus://<repository>/<path>`.
- Settings, in `drivers[].settings`: `allow_cidrs`, `plaintext` (default `false`). Every connection goes through the dial guard (SRC-3); no redirects are followed (SEC-13), so credentials never leave `url`'s host.
- Keys are checked as `http`'s (`locate`): each segment escaped, `.` and `..` refused, no empty segment.

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

## 7. Tests (written first)

| ID | Test | Kind |
| :--- | :--- | :--- |
| NX-T1 | `ParseRef`, `locate`, the settings and the endpoint's checks | unit, fuzz |
| NX-T2 | Source against a fake Nexus: the version from the `ETag`, a changed `ETag` `SourceChanged`, a `200` to a range refused, `PresignRead` only without credentials | unit |
| NX-T3 | Destination against a fake: the stream in order, the SHA-1 compared, `overwrite: never` and a refused redeploy, a repository not `raw` or not `hosted` refused | unit |
| NX-T4 | The conformance suite, source and sink, against the lab's Nexus | lab |
| NX-T5 | End to end: a job from Nexus to Garage, and from S3 to Nexus | lab |

## 8. Documentation

The Setup Guide (package, settings), the Operator's Guide (endpoints, credentials, write policy, R-18), the Architecture and Design Guide (the drivers table, the threat rows), `veho-threat-model.md` (NX-1 to NX-5, R-18).

## 9. Order of work

After the OpenStack drivers' step 1 (the shared pieces of `pkg/plugin/http` it reuses: `locate`, the range checks): the driver with NX-T1 to NX-T3, then the lab node and NX-T4 and NX-T5.
