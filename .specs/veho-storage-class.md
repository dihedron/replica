# veho: storage class of the copies

This file specifies how a tenant chooses the storage class of the copies veho writes. It answers the storage-class part of OI-7 and is part of `veho_specification.md`.

Status: approved, 2026-11-24.

## 1. Decisions

1. **The storage class is a property of the endpoint** (decided 2026-11-24). A tenant administrator sets it in the endpoint's driver configuration. To write copies in two classes to one bucket, the administrator registers two endpoints, for example `eu2-images` and `eu2-images-ia`, and the policies choose between them. Policies, jobs, the API and the plugin protocol do not change.
2. **Archive classes are refused in v1** (decided 2026-11-24). A copy in an archive class cannot be read without a restore, so it could not be the source of another transfer (FR-10) or be read back. Refused: `GLACIER` and `DEEP_ARCHIVE`, the AWS archive classes. `GLACIER_IR`, which reads at once, is allowed.
3. **veho checks that the backend used the class.** S3-compatible services differ: some ignore a class they do not support and store the object in their default class. veho reads the class back after every commit, so a mismatch shows at the endpoint test (`veho endpoint test`), before any job uses the endpoint.

## 2. Contract

### 2.1 S3 driver configuration

A new optional key, `storage_class`:

```json
{"endpoint": "https://s3.eu-2.example", "region": "eu-2", "bucket": "images", "path_style": true, "storage_class": "STANDARD_IA"}
```

- Unset or empty: veho sends no class and the backend uses its default (S3: `STANDARD`).
- Set: sent on `CreateMultipartUpload` and on the `PutObject` of an empty object (`x-amz-storage-class`).
- `GLACIER` or `DEEP_ARCHIVE`: `Open` fails with a `Permanent` error.
- The decoding of the S3 configuration becomes strict: an unknown key fails `Open` with a `Permanent` error, so that a misspelled `storage_class` is not silently dropped.

The name is otherwise passed as given: the classes are the backend's own (AWS, Ceph placement targets and others), and the driver cannot know them. A name the backend does not know fails the upload, and so the endpoint test.

### 2.2 The check after commit

Every commit of the S3 driver ends with a `HeadObject`. When `storage_class` is set, the class it reports must equal it; a missing `x-amz-storage-class` header counts as `STANDARD`, as in S3. A mismatch is a `Permanent` error: `s3: <key> stored as <reported>, not <configured>`.

- The endpoint test writes and deletes a probe object through the same commit, so it reports the mismatch at the `write` step.
- A transfer that meets a mismatch fails permanently. The object is already in place under its final name (the commit is done), so the error says so, and the copy is left there: deleting it is the client's choice (OI-9).

### 2.3 Other drivers

The `file` driver has no storage class. Glance (backlog 21) will be specified with its driver.

## 3. Threat model

No new threat. The class is set by a tenant administrator, who can already register any endpoint within the platform's limits (T-22, R-08); a costlier class is a matter of the tenant's own account with the storage provider. The strict decoding removes a way for a misconfiguration to go unnoticed.

## 4. Tests (written first)

| ID | Test | Kind |
| :--- | :--- | :--- |
| SC-T1 | `Open` refuses `storage_class` `GLACIER` and `DEEP_ARCHIVE`, and accepts `GLACIER_IR` and `STANDARD_IA`, with `Permanent` errors | unit |
| SC-T2 | `Open` refuses an unknown configuration key | unit |
| SC-T3 | `Begin` sends the class on `CreateMultipartUpload`, the empty-object commit sends it on `PutObject`; nothing is sent when it is unset | unit, fake S3 server |
| SC-T4 | Commit: a reported class different from the configured one is a `Permanent` mismatch; a missing header equals `STANDARD`; nothing is checked when the class is unset | unit, fake S3 server |
| SC-T5 | The endpoint test on the lab's Garage: passes with `STANDARD`; with `STANDARD_IA`, the outcome (Garage ignores or refuses the class) is recorded in the Operator's Guide | lab |

## 5. Documentation

- Setup Guide, a first replication: the `storage_class` key of the S3 driver.
- Operator's Guide: storage classes, one endpoint per class, the refused archive classes, the endpoint test as the check, and Garage's behaviour (SC-T5).
- Architecture and Design Guide, storage plugins: the check after commit.

## 6. Open points

1. Lifecycle rules of the bucket (for example an S3 transition to an archive class after some days, or Intelligent-Tiering's archive tiers) can move copies to an archive class later. That happens after the copy and is the bucket owner's choice; veho does not check it.
2. PLG-10 (each driver validates its configuration at registration) is not implemented: a wrong configuration shows at the endpoint test or at the first transfer, not at `endpoint create`. Proposed as a separate backlog item.
