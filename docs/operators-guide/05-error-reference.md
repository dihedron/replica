# Error reference

## API errors

The gRPC API answers errors with a status and an `ErrorInfo` detail (domain `veho`) carrying the code and whether a retry may succeed; the REST API answers `application/problem+json` with the same code. Clients should act on the code, not on the message.

::: landscape

| Code | gRPC | HTTP | Retryable | Meaning, and what to do |
| :------------------------ | :------------------- | :--- | :-------- | :------------------------------------------------------------------- |
| `unauthenticated` | `UNAUTHENTICATED` | 401 | no | No credential, or one that is unknown, expired or revoked |
| `permission_denied` | `PERMISSION_DENIED` | 403 | no | The principal's role, or its policy grants, do not allow the call |
| `tenant_required` | `INVALID_ARGUMENT` | 400 | no | The caller has an OIDC token or an SVID and named no tenant: `X-Veho-Tenant`, or `--tenant` |
| `not_entitled` | `PERMISSION_DENIED` | 403 | no | The identity is verified, and has no entitlement in the tenant it named, or that tenant does not exist or is suspended; or an API key named a tenant that is not its own |
| `tenant_suspended` | `PERMISSION_DENIED` | 403 | no | The tenant is suspended: a platform administrator must reactivate it |
| `inline_policy_refused` | `PERMISSION_DENIED` | 403 | no | The tenant's setting does not let this principal submit an inline policy: use a named policy it has `submit` on |
| `invalid_request` | `INVALID_ARGUMENT` | 400 | no | A malformed request: a missing field, a bad label, a bad duration |
| `invalid_policy` | `INVALID_ARGUMENT` | 400 | no | The policy is malformed: no destination, a bad `min_copies`, an option out of range, a retry outside the tenant's bounds |
| `too_many_destinations` | `INVALID_ARGUMENT` | 400 | no | More destinations than the tenant's `max_destinations` |
| `unknown_endpoint` | `INVALID_ARGUMENT` | 400 | no | A destination names an endpoint the tenant does not have |
| `duplicate_destination` | `INVALID_ARGUMENT` | 400 | no | Two destinations with the same endpoint and path |
| `destination_is_source` | `INVALID_ARGUMENT` | 400 | no | A destination is the source object itself |
| `unsatisfiable_policy` | `INVALID_ARGUMENT` | 400 | no | The spread asks for more distinct locations than the destinations span, or than `min_copies` |
| `invalid_endpoint` | `INVALID_ARGUMENT` | 400 | no | The source's driver cannot be a source that reads ranges, a destination's driver cannot be a destination (`http` and `ftp` are sources only), or the driver is not on this node |
| `not_found` | `NOT_FOUND` | 404 | no | No such object in the caller's tenant (another tenant's objects look the same) |
| `already_exists` | `ALREADY_EXISTS` | 409 | no | A tenant, endpoint or entitlement with that name exists |
| `idempotency_key_reuse` | `ALREADY_EXISTS` | 409 | no | The caller used the idempotency key for a different request |
| `invalid_state` | `FAILED_PRECONDITION` | 409 | no | The job's state forbids it: cancelling a settled job, retrying one that did not fail |
| `in_use` | `FAILED_PRECONDITION` | 409 | no | The endpoint, secret or policy is referenced and cannot be deleted |
| `not_modified` | `ABORTED` | 304 | no | A long poll with `If-None-Match`: the job has not changed |
| `quota_exceeded` | `RESOURCE_EXHAUSTED` | 429 | yes | The tenant has `max_active_jobs` jobs not yet settled |
| `rate_limited` | `RESOURCE_EXHAUSTED` | 429 | yes | Too many calls from the tenant or principal, or too many refused credentials from the client's address |
| `too_many_watches` | `RESOURCE_EXHAUSTED` | 429 | yes | The node has `api.max_watches` streams open |
| `unavailable` | `UNAVAILABLE` | 503 | yes | The node cannot reach the state store, or is fenced: try another node; or the token's OIDC issuer cannot be reached |
| `internal` | `INTERNAL` | 500 | yes | A bug: the message carries a request ID, and the node's log has the details |

:::

## Transfer error classes

A transfer's `last_error` carries a class, which decides what veho does with the failure:

| Class | Typical cause | What veho does |
| :---------------- | :------------------------------------------------ | :------------------------------------------------ |
| `Transient` | A timeout, a 5xx, throttling (429, 503), a reset connection, a database failover | Retries with back-off until the deadline |
| `ResumeInvalid` | The multipart upload expired or was aborted | Drops the upload and begins again |
| `Integrity` | A size or checksum mismatch | Rewrites the part; at most 3 times, then `FAILED` |
| `SourceChanged` | The source object changed during the copy | `FAILED`: a copy never mixes two versions |
| `Permanent` | 401, 403, a missing bucket or object, an invalid configuration | `FAILED` at once |
| `Unknown` | An error the driver did not classify | Retried as `Transient`, at most 10 times in a row, then `FAILED` |
| `Internal` | Set by veho itself: a poison pill, a workflow that ended without recording its outcome | `FAILED`, audited |

## Exit codes of the client

| Code | Meaning |
| :--- | :------------------------------------------------ |
| 0 | Success; for `veho job wait`, the job `SUCCEEDED` |
| 1 | An error, the API's or the client's; the message says which |
| 3 | `veho job wait`: the job `FAILED` |
| 4 | `veho job wait`: the job was `CANCELLED` |
| 5 | `veho job wait`: `--max-wait` ran out |
