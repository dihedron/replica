# veho: the OpenStack drivers, Glance and Swift

This file specifies the `glance` and `swift` drivers (backlog 21 and 34, OI-6) and the lab that tests them against DevStack. It is part of `veho_specification.md`; the driver contract is `veho-plugins.md`, the source rules `veho-sources.md`, the plugin distribution `veho-plugin-distribution.md`.

Status: approved, 2026-11-25 (revised the same day: the minimum release per feature, §1.1). Steps 1 to 4 implemented the same day (§10 records what the implementation changed); the end-to-end tests of step 5 next.

## 1. Decisions (2026-11-25)

1. **Both drivers are plugins**, `veho-plugin-glance` and `veho-plugin-swift`, over one OpenStack client library, gophercloud v2 (Apache-2.0, pure Go), with a shared package for the authentication (`pkg/plugin/openstack`).
2. **Both authentications**, chosen per endpoint by its credentials: Keystone v3 **application credentials** (`application_credential_id` and `application_credential_secret`, scoped to one project by Keystone) or **username and password** (`username`, `password`, `user_domain_name`, `project_name`, `project_domain_name`). No token is stored: each `Open` authenticates, and gophercloud re-authenticates when the token expires.
3. **Glance offers the three upload methods, chosen per endpoint** (`upload`): `put` (the plain `PUT /v2/images/{id}/file`), `glance-direct` (stage, then import, which lets multi-store choose the stores) and `web-download` (Glance pulls the object from a URL veho gives it). Each restarts from zero after a failure: Glance has no resumable upload.
4. **Swift is a source and a destination**, with **static large objects** (SLO): each part a segment, the manifest written at commit, so parts are parallel and resumable as with S3.
5. **The minimum OpenStack release is Queens (2018), and each feature asks only for the release it needs** (§1.1): the plain `put` upload, the import methods and application credentials work on Queens; hiding an image while it uploads, Glance's SHA-512 and the stores need Rocky or later. The driver detects what the cloud offers; it does not take a release number from the endpoint. The lab runs the most recent DevStack stable branch.
6. **Nexus** (Sonatype Nexus Repository) is specified apart, in `veho-nexus.md`.

### 1.1 The release each feature needs

| Feature | Needs | Detected by | Without it |
| :--- | :--- | :--- | :--- |
| Keystone v3, username and password | any release with Keystone v3 | | |
| Application credentials | Keystone Queens | the authentication's answer | the endpoint's credentials are refused with a `Permanent` error that says so |
| Glance source (`GET /v2/images/{id}`, download) | Glance v2, any release | | |
| Ranges on the image download | probed | `Stat`'s probe (§3.1) | `Permanent` |
| `put` (`PUT /v2/images/{id}/file`) | Glance v2, any release | | |
| `glance-direct`, `web-download` (interoperable import, API 2.6) | Glance Queens | `GET /v2/info/import` | `Begin` refuses the method (§3.3) |
| `os_hidden` (API 2.7) | Glance Rocky | the API versions (`GET /versions`) | the image uploads under a temporary name (§3.2) |
| `os_hash_algo`, `os_hash_value` (multihash, API 2.7) | Glance Rocky | the image's fields | the MD5 `checksum` is compared instead (§3.2) |
| `stores` (multi-store, API 2.8) | Glance Rocky (experimental), Train (stable) | `GET /v2/info/stores` | `Begin` refuses `stores` (§3.3) |
| Swift SLO | any supported release | `GET /info` (`slo`) | `Begin` refuses the endpoint |

The floor is Queens because it is the oldest release with application credentials and the import methods; a Pike cloud would work for `put` with a password, but it is not supported or tested.

## 2. Common: endpoints, catalog, TLS, dial guard

```json
{"auth_url": "https://keystone.example:5000/v3", "region": "RegionOne", "interface": "public", "ca_cert": "-----BEGIN CERTIFICATE-----..."}
```

- `auth_url` must be `https` (credentials cross it), unless the driver's `plaintext` setting allows `http` (the lab's DevStack without its TLS proxy, never in production).
- `region` and `interface` (`public`, `internal`, `admin`; default `public`) select the service's endpoint in Keystone's catalog.
- `ca_cert`: an optional PEM bundle that verifies Keystone's and the services' certificates, besides the node's system roots (a private cloud's own CA). Certificates are always verified.
- **Every connection goes through the dial guard** (`internal/netguard`, SRC-3): Keystone and every endpoint the catalog returns, which a tenant's Keystone controls. Private ranges only through the driver's `allow_cidrs` setting.
- No redirects are followed (SEC-13); credentials go only to `auth_url`, tokens only to the catalog's endpoints of the same cloud.

Settings of both drivers, in `drivers[].settings`: `allow_cidrs`, `plaintext` (default `false`).

## 3. Glance

### 3.1 As a source

- The key of a job's source path is the **image ID** (a UUID); `glance://<image-id>` in `ParseRef`.
- `Stat`: `GET /v2/images/{id}`; the image must be `active`; the size is `size`; the **version** is `<os_hash_algo>:<os_hash_value>` (Glance computes it at upload; an active image's data never changes), else `md5:<checksum>` on images uploaded before Rocky.
- `ReadRange`: `GET /v2/images/{id}/file` with `Range`; only `206` with the exact `Content-Range` is accepted, as `http` (SRC-2). A Glance that answers `200` to a range request fails `Stat`'s probe with a `Permanent` error that says so. An image no longer active (deactivated, deleted, re-created under the ID) is `SourceChanged`: `ReadRange` checks the status and the hash with a `GET` of the image before the data request.

### 3.2 As a destination

The destination path is the **image name**. Properties of the new image come from the endpoint:

```json
{"auth_url": "...", "upload": "put", "disk_format": "qcow2", "container_format": "bare", "visibility": "private",
 "properties": {"os_distro": "ubuntu"}, "stores": ["ceph-a", "ceph-b"], "web_download_url_ttl": "1h"}
```

- `disk_format` and `container_format` are required (Glance needs them); `visibility` defaults to `private`; `properties` are set on the image; `stores` only with `glance-direct` (multi-store `import` with `stores`; `all_stores: true` with `"stores": ["*"]`).
- **Invisible until commit (PLG-8):** `Begin` creates the image with `os_hidden: true` and the property `veho_upload: <upload id>`, under the name `<name>`, with an **ID derived from the upload** (UUIDv5 of the transfer's token), so a `Begin` retried after a lost answer finds its image instead of creating a second one. `Commit` sets `os_hidden: false` once the image is `active` and its hash checked. A hidden image is not listed by default, but it is readable by its ID to the project: accepted (R-16).
- **On a Glance before Rocky** (no `os_hidden`), `Begin` creates the image under the temporary name `veho-upload-<upload id>`, and `Commit` renames it to `<name>`. The image is listed while it uploads, under a name that says what it is, and is never found by `Stat` under the destination name before `Commit` (R-16).
- **Overwrite.** Glance names are not unique. `Stat` of a destination finds the project's visible, active images of that name: none is `ErrNotFound`, more than one is a `Permanent` error. With `overwrite: replace`, `Commit` unhides the new image, then deletes the older ones of that name (a moment when both are visible: R-16).
- **Integrity.** The driver computes the SHA-512 of the bytes it uploads (`put`, `glance-direct`) and compares it, at `Commit`, with Glance's `os_hash_value` (when `os_hash_algo` is `sha512`, Glance's default); a mismatch is `Integrity`. A Glance without multihash (before Rocky), or with another `os_hash_algo`, has its MD5 `checksum` compared instead, from an MD5 the driver computes too: enough against corruption, not against a Glance that lies (OS-8). The engine's own digest (SHA-256 of the parts) and size check apply as for every destination.
- **Capabilities:** sink, not resumable (`ResumableUpload: false`, `ParallelParts: false`): the parts of an attempt are streamed in order into one request; a new attempt finds the token's upload gone (`ResumeInvalid`), deletes the hidden image and begins again from byte zero. `MaxParts` is unbounded and the part size is the engine's.

**The three methods:**

| `upload` | How | Glance needs | Notes |
| :--- | :--- | :--- | :--- |
| `put` (default) | `Begin` creates the image and opens `PUT /v2/images/{id}/file`, chunked; each `WritePart` writes into it; `Commit` closes it, waits for `active` | any release | the simplest; one request per attempt |
| `glance-direct` | the same stream to `PUT /v2/images/{id}/stage`; `Commit` calls `POST /v2/images/{id}/import` (`glance-direct`, with `stores`) and waits for `active` | the import workflow enabled, `glance-direct` among `enabled_import_methods` | multi-store; the import's failure is reported from the image's `os_glance_failed_import` |
| `web-download` | `Begin` creates the image; `Commit` calls `import` with `web-download` and a URL of the source, and waits for `active`; then **read-back** (below) | `web-download` enabled, Glance able to reach the URL, the URL allowed by Glance's own filters | veho does not upload the bytes |

**`web-download` in the engine.** The bytes do not cross veho, so two pieces are new:

- *The URL.* A source driver may offer a URL to read the object without credentials, for a time (`plugin.Presigner`: `PresignRead(ctx, ref, version, ttl) (url string, err)`): `s3` gives a presigned `GET`, `http` its URL when the endpoint has no credentials. A source that cannot is refused at submission for a `web-download` destination, with `invalid_endpoint`. The URL is given to Glance only, never logged (it is a credential for its lifetime, `web_download_url_ttl`, default 1 h).
- *The integrity.* A sink may declare `Pull` (a new capability): the engine then calls `Pull(ctx, ref, url, info)` in place of the parts, and after `Commit` reads the image back from Glance by ranges, as a source, hashing each part; the transfer's digest is that of the parts read back, compared with the job's digest as for every destination. When no other destination has recorded the job's digest yet, the engine first reads the source's parts and records theirs, so the image is always compared with the source (FR-8). The cost: veho reads the object twice (the source and the image) and uploads nothing.

### 3.3 Order of the checks

A `Begin` refuses an endpoint whose Glance lacks the upload method (`GET /v2/info/import`: `import-methods`), and `stores` that Glance does not have (`GET /v2/info/stores`), with `Permanent` errors that name them.

## 4. Swift

```json
{"auth_url": "...", "container": "images", "segment_container": "images_segments", "storage_policy": ""}
```

- The key is the object's name in `container`; `ParseRef` takes `swift://<container>/<name>`.
- **Source:** `HEAD` gives the size and the version, the `ETag` (the MD5 of the object, or of an SLO's manifest, which changes with any segment) together with `X-Timestamp`; `ReadRange` is `GET` with `Range` and `If-Match`, the response checked as `http` (SRC-2).
- **Destination:** each part is a segment `<segment_container>/<name>/<upload id>/<part number, 8 digits>`, written with `PUT` and its `ETag` (Swift checks the MD5, as S3's `Content-MD5`); `ListParts` lists the segments of the upload; `Commit` writes the SLO manifest (`?multipart-manifest=put`) with each segment's size and `ETag`, which Swift checks; an object is invisible until its manifest exists. `Abort` deletes the segments; `Delete` deletes the manifest and its segments (`?multipart-manifest=delete`).
- Capabilities: source, sink, resumable, parallel parts; parts of 1 MiB to 5 GiB, at most 1,000 segments (Swift's default `max_manifest_segments`; the endpoint may set `max_segments` to the cluster's).
- `storage_policy` creates the containers with that policy when they do not exist; it is checked, not changed, on existing ones (a mismatch is `Permanent`, as `s3`'s storage class).

## 5. Threat model

| ID | Threat | Mitigation |
| :--- | :--- | :--- |
| OS-1 | **SSRF through Keystone's catalog**: a tenant's Keystone returns service endpoints on veho's internal network. | The dial guard on every connection, after resolution, the catalog's endpoints included (SRC-3); private ranges only from the driver's `allow_cidrs`. |
| OS-2 | **Credentials or tokens sent elsewhere**: a redirect, or a catalog endpoint of another host. | No redirects; credentials only to `auth_url` over HTTPS; tokens only to the catalog of the same Keystone; `ca_cert` adds trust, never removes verification. |
| OS-3 | **A presigned URL leaks** (`web-download`): anyone with it reads the source for its lifetime. | Short lifetime (`web_download_url_ttl`, default 1 h); given only to Glance, never logged, never stored; `web-download` an opt-in per endpoint. |
| OS-4 | **Glance stores bytes other than the source's** (`web-download`: Glance fetched another object, or a man in the middle on Glance's side). | Read-back: the image's parts are hashed by veho and compared with the source's digest (§3.2). |
| OS-5 | **A half-uploaded image is used** before veho has verified it. | `os_hidden` until `Commit`, after `active` and the hash check; the hidden image deleted on a new attempt or `Abort` (R-16). |
| OS-6 | **An image replaced under its ID** while it is read. | The status and the hash are checked before every range; a changed image is `SourceChanged`. |
| OS-8 | **A weaker hash on old clouds**: before Rocky only Glance's MD5 compares the stored image with the uploaded bytes. | The MD5 detects corruption, which is the threat (Glance is the tenant's own service); `web-download` always uses the read-back, which veho hashes with SHA-256. |
| OS-7 | **Swift segments of another upload or object** are taken into a manifest. | Segment names carry the upload ID; Swift checks each segment's size and `ETag` against the manifest. |

New residual risks:

- **R-16** A Glance image being uploaded is hidden, not invisible: the project's users can read it by its ID before `Commit` (and, before Rocky, see it listed under its temporary name), and with `overwrite: replace` the old and new images are both visible for a moment. Glance has no atomic replacement; accepted.
- **R-17** Glance cannot resume an upload: a failed attempt restarts a 5 to 10 GB image from zero (`put`, `glance-direct`). Mitigated by the engine's retries and backoff; `web-download` moves the transfer to Glance. Accepted.

## 6. The DevStack lab

A lab of its own, **self-contained in this repository** (`test/devstack/`), so it starts on a fresh machine; it borrows the approach and scripts of `openstack-spiffe`'s lab, adapted and copied, with no reference to that project at run time.

- **One libvirt VM**, `veho-devstack` (Ubuntu 24.04, 4 vCPUs, 12 GiB, 60 GiB), on a NAT network of its own, with DevStack's most recent stable branch: **Keystone, Glance and Swift only** (no Nova, Neutron, Cinder or Horizon), behind DevStack's **TLS proxy** with its own CA.
- Glance with the **import workflow** (`glance-direct` and `web-download` enabled, `web-download` allowed to reach the lab's addresses), **multi-store** with two file stores (`fast`, `cheap`), and `os_hidden` support (every release since Rocky).
- `lab.sh` commands as in the veho and openstack-spiffe labs: `preflight`, `up`, `down`, `status`, `ssh`, `snapshot`, `reset` (back to the baseline in minutes), `stop`, `start`, `env` (the tests' environment: `auth_url`, CA, credentials of a test project with an application credential and a user), `test`.
- `up` makes a baseline snapshot after `stack.sh`, so later runs `reset` instead of stacking again (about 30 minutes). An optional `LAB_IMPORT_IMAGE` takes an Ubuntu cloud image already downloaded (such as openstack-spiffe's cache) instead of downloading one; nothing else is shared.
- DevStack builds only recent releases on Ubuntu 24.04, so the lab tests the current release; the paths for older clouds (§1.1) are tested against fakes (OS-T4b).
- The tests (build tag `devstack`) run the conformance suite on Glance (source; destination with each method) and Swift (source and destination), the checks of §3 and §4, and an end-to-end job from S3 (the veho lab's Garage, when it runs) or an HTTP source to Glance and Swift.

## 7. Tests (written first)

| ID | Test | Kind |
| :--- | :--- | :--- |
| OS-T1 | Authentication: application credential, username and password, a bad secret (`Permanent`), a token expired during a transfer (re-authenticated) | unit (a fake Keystone), DevStack |
| OS-T2 | The catalog: region and interface chosen; an endpoint on a refused address (`ErrForbidden`); `ca_cert` trusted, an unknown CA refused | unit |
| OS-T3 | Glance source: version from the hash, a deactivated or deleted image `SourceChanged`, a Glance without ranges refused | unit (fake Glance), DevStack |
| OS-T4 | Glance destination, each method: hidden until commit, the ID derived from the upload, the hash compared, `overwrite: replace`, a new attempt deletes the hidden image, a missing method or store refused at `Begin` | unit, DevStack |
| OS-T4b | A Glance of Queens (a fake answering API 2.6, no multihash, no stores): `put` under the temporary name and renamed, the MD5 compared, `stores` refused, application credentials accepted | unit |
| OS-T5 | `web-download`: the presigned URL from `s3` and `http`, a source that cannot presign refused at submission, the read-back digest compared with the source's, a URL never logged | unit, integration, DevStack |
| OS-T6 | Swift: the conformance suite as source and sink, SLO manifest, segment `ETag` checked, `storage_policy` | DevStack |
| OS-T7 | End to end: a job to Glance (each method) and Swift | DevStack |
| OS-T8 | Fuzz tests of the keys and image names | fuzz |

## 8. Documentation

- Setup Guide: the packages `veho-plugin-glance` and `veho-plugin-swift`; the settings.
- Operator's Guide: endpoints of both kinds, the credentials, the upload methods and what Glance needs for each, R-16 and R-17 in plain words.
- Architecture and Design Guide: the drivers in the table, `Pull` and `Presigner`, the threat rows.
- `veho-threat-model.md`: OS-1 to OS-7 by reference, R-16 and R-17; OI-6 decided in `veho_specification.md`; backlog 21.

## 9. Order of work

1. `pkg/plugin/openstack` (authentication, catalog, guard, TLS) with OS-T1 and OS-T2.
2. The DevStack lab (`test/devstack`), checked with a smoke test.
3. Swift, then Glance with `put` and `glance-direct`.
4. `web-download`: `Presigner` in `s3` and `http`, `Pull` and the read-back in the engine.
5. The end-to-end tests, the docs, the packages.

## 10. Changes from the implementation (2026-11-25)

- **Image IDs are random, not derived from the upload.** `Begin` does not know the transfer, and Glance never reuses a deleted image's ID, so a derived ID would make every new attempt fail. A `Begin` retried after a lost answer leaves a hidden, queued image (garbage, never visible); a new attempt's `Resume` deletes the token's image, unless it is already published under the name with the token's `veho_upload`, which is then the committed object.
- **A destination name may not be a UUID**, so a key is an ID or a name, never both. `ReadRange` and `Stat` take either: a name is the project's one visible active image of that name (`owner` filtered).
- **The data request** has the size announced at `Begin` as its `Content-Length` (chunked when none was announced); a part written again with the same bytes (its SHA-256) is harmless, with other bytes `ResumeInvalid`.
- **Replacement:** the engine checks `overwrite` before `Begin` (`fail`, `skip`), so `Commit` deletes the project's other active images of the name after publishing the new one.
- **Swift:** `max_segments` is a driver setting (capabilities are the driver's, not the endpoint's); an upload has a marker object (`<name>/<upload id>/upload` among the segments) that `Begin` writes and `Commit` and `Abort` remove, so a write into an aborted or committed upload is `ResumeInvalid`; overwriting an SLO deletes the old one's segments; a `HEAD` whose `Content-Length` a proxy dropped (Apache drops `0`) takes the size from the container listing.
- **The lab:** DevStack configures Glance's file stores only without Swift, so the lab adds `fast` and `cheap` itself (`lib/devstack.sh`, `glance_stores_conf`); Glance's `allowed_ports` must be `[]`.
- **`web-download`:** `plugin.Puller` is an optional interface of the upload (`PullTTL`, `Pull(ctx, url) (Ref, error)`) rather than a capability: the method is the endpoint's, the capabilities the driver's. `Pull` returns a reference (the image ID) that reads the image back while it is hidden. The engine compares the read-back digest with the job's before `Commit`, so a wrong image is never published, and `verify` compares it again. A source that cannot presign fails the transfer as `Permanent`, not at submission: the destination's method is known only once its upload begins (a submission check needs the driver to describe an endpoint: later). The presigned URL carries no condition on the version (Glance would not send it); the read-back covers it.
- **Verified on DevStack stable/2026.2:** the authentication with both kinds of credentials, the conformance suite on Swift (both credentials) and on Glance (`put`, and `glance-direct` to the store `cheap`); `web-download` from an HTTP source on the lab host, read back while hidden, then published.
