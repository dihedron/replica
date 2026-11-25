# veho: storage plugins

This file specifies the storage plugin interfaces and their conformance suite. It is part of `veho_specification.md`.

Status: approved 2026-11-21 with §7 (the decisions of chunk C5); C5 done 2026-11-21, with the deferrals listed at the end of §7.

## 1. Mechanism

- **PLG-1** Plugins are **out-of-process** and use `hashicorp/go-plugin` over
  gRPC (MPL-2.0 library, compatible with this project's MIT licence). The core
  talks to every driver only through the `Driver` interface of this document,
  and has two loaders behind it: a **process loader** (go-plugin: the driver
  runs in its own process) and an **in-process loader** (a compiled-in Go
  package registered with `plugin.Register(driver)`). Whether the built-in
  drivers (S3, and later Glance) ship as separate plugin binaries or are
  compiled in is decided later (OI-10); nothing in the core depends on the
  answer.
- **PLG-2** The contract in this document is **transport-neutral**: only
  serialisable types cross it (`context.Context` and `io` for data), described
  by a protobuf service in `pkg/plugin`. The same driver code runs under both
  loaders and the conformance suite runs against both (PLG-10). Go's stdlib
  `plugin` package is not used (fragile, same-toolchain-only).
- **PLG-3** A driver is selected by the endpoint's `driver` field and by URL
  scheme for submitted source URLs.
- **PLG-11** A plugin manages one kind of storage (S3, Glance, …) and acts on
  one endpoint at a time. It knows nothing of policies, topology, locations,
  N or M: those belong to the core (`veho-distribution-policy.md`). The
  location of an endpoint is endpoint metadata read by the core, never by the
  driver, and a driver never decides which copies to create or when a job is
  done.

## 2. Interfaces (draft, package `plugin`)

```go
// Driver is the factory registered once per backend type.
type Driver interface {
    Name() string                    // "s3", "glance", "file", "http"
    Schemes() []string               // URL schemes it can parse: "s3"
    Capabilities() Capabilities
    // ParseRef turns a URL into a driver-neutral object reference.
    ParseRef(url string) (Ref, error)
    // Open creates a client bound to a tenant endpoint config and credentials.
    // Called per transfer attempt; MUST be cheap and safe to call concurrently.
    Open(ctx context.Context, cfg Config, creds Credentials) (Client, error)
}

type Capabilities struct {
    Source            bool
    Sink              bool
    RangeRead         bool   // required for Source
    ResumableUpload   bool   // can resume across processes via ResumeToken
    ParallelParts     bool   // parts may be written out of order/concurrently
    NativeChecksum    []string // e.g. "sha256", "md5", "crc32c"
    MaxParts          int
    MinPartSize       int64
    MaxPartSize       int64
    ServerSideCopy    bool   // optional optimisation, post-v1
}

type Client interface {
    Source   // nil-op error if !Capabilities.Source
    Sink     // likewise
    Close() error
}

type Source interface {
    Stat(ctx context.Context, ref Ref) (ObjectInfo, error)
    // ReadRange returns [off, off+length). MUST fail with ErrSourceChanged if
    // `ifVersion` is non-empty and no longer matches.
    ReadRange(ctx context.Context, ref Ref, off, length int64, ifVersion string) (io.ReadCloser, error)
}

type Sink interface {
    // Begin starts (or restarts) an upload to a *staging* location.
    Begin(ctx context.Context, ref Ref, info ObjectInfo, opts PutOptions) (Upload, error)
    // Resume re-attaches using a token previously returned by Upload.Token.
    Resume(ctx context.Context, ref Ref, token []byte) (Upload, error)
    // Stat for overwrite/idempotency checks and post-commit verification.
    Stat(ctx context.Context, ref Ref) (ObjectInfo, error)
    Delete(ctx context.Context, ref Ref) error
}

type Upload interface {
    Token() []byte // opaque, serialisable, persisted by core
    // WritePart MUST be idempotent per partNo (re-writing the same part
    // number with the same bytes is a no-op or harmless overwrite).
    WritePart(ctx context.Context, partNo int, r io.Reader, length int64) (PartResult, error)
    ListParts(ctx context.Context) ([]PartResult, error) // for reconciliation
    Commit(ctx context.Context, parts []PartResult) (ObjectInfo, error) // atomic publish
    Abort(ctx context.Context) error
}

type ObjectInfo struct {
    Size     int64
    Version  string            // etag / generation / image checksum+updated_at
    Checksums map[string]string // algo → hex
    Metadata map[string]string
}
```

Reference mapping:

| Concept | S3 | Glance | file (tests) |
|---------|----|--------|--------------|
| Ref | bucket/key | image ID (or name→ID) | path |
| Begin | CreateMultipartUpload | create image (`queued`), staged upload | create temp file |
| WritePart | UploadPart | **no native multipart**: `ParallelParts=false`, `ResumableUpload=false` unless using image import/staging; v1 streams in a single part | write at offset |
| Commit | CompleteMultipartUpload | image becomes `active` | atomic rename |
| Abort | AbortMultipartUpload | delete queued image | remove temp |

(Glance capabilities must be confirmed against the target OpenStack release;
see veho_specification.md OI-6.)

## 3. Requirements

- **PLG-4** Drivers MUST classify every returned error with one of
  `plugin.Transient | Permanent | ResumeInvalid | Integrity | SourceChanged`
  via `plugin.Wrap(err, class)`; unwrapped errors are treated as `Unknown`.
- **PLG-5** Drivers MUST NOT log secrets; credentials are passed per `Open`
  and never persisted by the driver.
- **PLG-6** Drivers MUST honour `ctx` cancellation promptly and MUST NOT hold
  global mutable state shared across tenants.
- **PLG-7** `Commit` MUST be atomic and idempotent (committing an
  already-committed upload with identical parts returns its `ObjectInfo`).
- **PLG-8** Hidden/staging objects MUST NOT be visible to readers under the
  final name until `Commit` (FR-9).
- **PLG-9** Drivers declare accurate `Capabilities`; core adapts (e.g.
  single-part streaming if `ParallelParts=false`).
- **PLG-10** Each driver ships a **config JSON Schema** used to validate
  `endpoints.config` at registration time. Implemented 2026-11-25 (`veho-config-validation.md`): the schema,
  then the driver's `CheckConfig(ctx, config)`, also over the plugin
  protocol (`rpc CheckConfig(ConfigRequest) returns (Empty)`).

## 4. Conformance suite

`plugin/conformance` provides `RunSuite(t, driverFactory)` which every
driver (built-in or third-party) must pass:

- stat/read-range correctness (incl. last partial range, zero-byte object)
- write → commit → read-back equals source (property-based sizes)
- idempotent re-write of a part; commit twice
- resume after process-simulated crash (if `ResumableUpload`)
- abort leaves no visible object; no object under final name before commit
- error classification table (induced via fault-injecting proxy, e.g.
  the lab's S3 fault proxy in front of Garage, `veho-lab.md` §5)
- context cancellation latency

Backed by Garage (S3, the lab or a local instance), a Glance/devstack or stub server, and the
`file` driver.

## 6. Process plugins (go-plugin)

- **PLG-12** The core starts a plugin process with go-plugin, over a Unix
  socket, with a magic cookie and a protocol version, and negotiates the
  version before the first call; an incompatible plugin is refused at start.
- **PLG-13** The channel uses go-plugin's automatic mutual TLS, so another
  local process cannot talk to the plugin or impersonate the core.
- **PLG-14** A plugin binary is started only from the root-owned plugin
  directory, and only if its SHA-256 matches the checksum in a manifest signed
  with the release key (go-plugin `SecureConfig`; the manifest is part of the
  signed release, `veho-threat-model.md` T-21).
- **PLG-15** The plugin process is confined like the service itself (no
  privileges, a read-only file system, no access to the state store, the
  configuration or the secret backend) and has network access only as its
  driver needs.
- **PLG-16** Credentials reach a plugin per attempt over the channel and are
  never written to disk or to the log (PLG-5). By default one process serves a
  driver for all tenants on a node; an operator can ask for one process per
  tenant for stricter isolation.
- **PLG-17** The data of a transfer flows **through the core**: the source
  plugin streams ranges to the core and the core streams them to the sink
  plugin. The core computes the digest while streaming (FR-8), so integrity
  does not depend on a plugin's honesty. The cost is an extra hop per byte;
  it is measured by a benchmark (AGENTS.md rule 9), and a direct
  plugin-to-plugin stream through go-plugin's broker is an optimization if
  the measurement requires it.
- **PLG-18** A plugin crash, a lost channel or a timeout is a `Transient`
  error (`veho-job-lifecycle.md` §7): the core restarts the process and the
  step retries; the plugin is stateless between calls, so the idempotency of
  `(transfer, part)` (INV-3) covers it.
- **PLG-19** Plugin logs are read from the plugin's log stream and written
  through `slog` with the contextual keys; a plugin never writes to the
  core's standard output.

## 7. Implementation (chunk C5)

C5 is built in three steps, each a commit with its tests:

| Step | Builds | Tested |
| :--- | :--- | :--- |
| **C5a** | `pkg/plugin` (the types of §2, the error classes, the in-process registry), the `file` driver, `pkg/plugin/conformance` | the suite on `file`, here |
| **C5b** | the S3 driver (`pkg/plugin/s3`, AWS SDK for Go v2) | the suite on the lab's three Garage clusters |
| **C5c** | the protobuf service, the go-plugin process loader and server, PLG-12 to PLG-19, the PLG-17 benchmark | the suite under the process loader, on `file` and S3 |

Decisions, completing §2:

- **Ref** is `{Container, Key}`: for S3 the bucket and the key, for `file` the key only (the root is in the endpoint's configuration). An empty `Container` means the endpoint's default (S3 `bucket`).
- **Config** is the endpoint's JSON configuration (`json.RawMessage`); **Credentials** is a `map[string]string` (`access_key_id`, `secret_access_key`, `session_token` for S3). Each driver exposes its configuration's JSON Schema (`ConfigSchema() []byte`, PLG-10); validating `endpoints.config` against it belongs to the API (C7).
- **Part numbers** start at 0, as `parts.part_no`; a driver maps them (S3: `partNo + 1`).
- **PartResult** is `{PartNo, Size, ETag, SHA256}`; the SHA-256 is computed by the driver over the bytes it wrote. The core computes its own digest too (PLG-17), so a driver's figure is a cross-check, not the authority.
- **Not found** is `plugin.ErrNotFound`, class `Permanent`, so the core can tell "absent" from other permanent errors (overwrite checks, PLG-8 tests).
- **Error classes** (PLG-4) are `Unknown`, `Transient`, `Permanent`, `ResumeInvalid`, `Integrity`, `SourceChanged`; `plugin.Wrap(err, class)` and `plugin.ClassOf(err)`. A cancelled context is returned as the context's error, unclassified: the core knows why it cancelled.
- **The `file` driver** writes through `os.Root` (no path can escape the root). An upload is a staging directory under `<root>/.veho-staging/<upload-id>/` with one file per part, each written to a temporary name and renamed (idempotent per part); `Commit` concatenates the parts into a temporary file, syncs it and renames it to the final key (atomic, PLG-7 and PLG-8). The version of an object is its size and modification time in nanoseconds.
- **The S3 driver** uses multipart uploads (invisible until complete: PLG-8). Each part is read into memory, its MD5 sent as `Content-MD5` (the server verifies it: a mismatch is `Integrity`) and its SHA-256 computed; this bounds memory to part size times parallel parts, so the driver declares `MaxPartSize` 512 MiB (S3 allows 5 GiB). The token is the bucket, key and upload ID. `Commit` of an upload that no longer exists returns the object if its ETag is the multipart ETag of the given parts (idempotent commit). Versions are ETags; `ReadRange` sends `If-Match`, and `412` is `SourceChanged`. Classification: `5xx`, throttling and network errors are `Transient`; `400`, `401`, `403`, `404` are `Permanent` (`NoSuchKey` is `ErrNotFound`); `NoSuchUpload` on `Resume` or `WritePart` is `ResumeInvalid`; `BadDigest` and `InvalidDigest` are `Integrity`. The SDK's default request checksums are off (`when_required`), so that any S3-compatible service, Garage included, accepts the requests; Content-MD5 gives the integrity check.
- **Empty objects on S3**: S3-compatible services refuse an empty part (Garage does, `InvalidRequest: Empty body`), so an empty part is not uploaded, and `Commit` of an empty object aborts the multipart upload and publishes it with an empty `PutObject`, which is atomic too.
- **Error table without the proxy**: the S3 driver's classification is checked against a fake service that answers with each status and error code (`pkg/plugin/s3/s3_test.go`); the fault proxy of L3 then repeats it against Garage, mid-transfer.
- **Conformance suite**: `conformance.Run(t, conformance.Target{...})`, with a seed for its random sizes (reported on failure). The error table of §4 by fault injection waits for the lab's S3 fault proxy (`veho-lab.md` §5, L3); until then the suite checks the classes it can provoke without one (not found, source changed, invalid resume token, bad credentials when the target provides them).

### 7.1 The process loader as built (C5c)

- `pkg/plugin/process`: `Serve(driver)` is a plugin binary's main (`cmd/veho-plugin-file`, `cmd/veho-plugin-s3`); `Load(Options)` starts one. The service is `pkg/plugin/process/proto/plugin.proto` (`make proto`; the generated code is committed). Clients and uploads live in the plugin process behind random handles; data moves in 1 MiB messages. An error's class travels in the trailer (`veho-class`, `veho-not-found`); a lost or crashed plugin is `Transient` (PLG-18), and the next `Open` restarts the process.
- PLG-12: handshake with a magic cookie and protocol version 1, gRPC only. PLG-13: go-plugin's automatic mutual TLS. PLG-14: go-plugin checks the binary's SHA-256 before starting it (`SecureConfig`); `RequireRootOwned` refuses a binary or directory that is not root's or is writable by others. The plugin gets none of the core's environment (`SkipHostEnv`), so no secret reaches it that way. PLG-19: the plugin's log lines go through `slog`.
- Tests: the suite passes under the process loader on `file` and, in the lab, on S3 (Garage of region B); a wrong or missing checksum and a user-owned binary are refused; a `SIGKILL`ed plugin fails the call in flight as `Transient` and is restarted by the next `Open`.
- **PLG-17 measured** (`BenchmarkCopy`, 64 MiB in 8 MiB parts, `file` driver to `file` driver, the core hashing the stream; AMD Ryzen 7 8745HS, 2026-11-21): 380 MB/s compiled in, 325 MB/s out of process, about 15 % less, both bound by the driver's synchronous writes. That is more than 2.5 Gbit/s per transfer through the core, so the direct plugin-to-plugin stream is not needed for v1; backlog 12 (OI-10) can decide on packaging alone. Decided: only standard-library drivers are compiled in (`veho-plugin-distribution.md`).

Deferred, each to the chunk that owns it:

- PLG-14's **signed manifest**: done 2026-11-24, `veho-plugin-distribution.md` §3 (`process.Installed`, Ed25519, the version of the manifest bound to veho's).
- PLG-15's **confinement** of the plugin process (systemd and AppArmor or SELinux settings): with the packaging (C11, backlog 27).
- PLG-16's **process per tenant** and the choice of loader per driver: configuration of `veho serve` (C6).
- The error table **by fault injection against a real service**: the lab's S3 fault proxy (L3); the classification is unit-tested against a fake service meanwhile.
