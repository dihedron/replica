# replica: storage plugins

This file specifies the storage plugin interfaces and their conformance suite. It is part of `replica_specification.md`.

Status: draft, 2026-11-21, for review.

## 1. Mechanism

- **PLG-1** v1 plugins are **compiled-in Go packages** registering themselves
  with the registry via `plugin.Register(driver)` (in `init()` or explicit
  wiring in `cmd/replica`). Build tags allow slimmed binaries.
- **PLG-2** The contract in this document is **transport-neutral** (only
  serialisable types cross it; `context.Context` and `io` for data). This
  allows a later out-of-process option (e.g. `hashicorp/go-plugin` over gRPC)
  without changing drivers' semantics. Go's stdlib `plugin` package is
  explicitly not used (fragile, same-toolchain-only).
- **PLG-3** A driver is selected by the endpoint's `driver` field and by URL
  scheme for submitted source URLs.

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
see replica_specification.md OI-6.)

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
  Toxiproxy/MinIO)
- context cancellation latency

Backed in CI by MinIO (S3), a Glance/devstack or stub server, and the
`file` driver.
