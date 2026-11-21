# veho: storage plugins

This file specifies the storage plugin interfaces and their conformance suite. It is part of `veho_specification.md`.

Status: draft, 2026-11-21, for review.

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
  `endpoints.config` at registration time.

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
