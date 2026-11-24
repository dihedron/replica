// Package plugin is the contract between veho's core and its storage drivers
// (.specs/veho-plugins.md): the types every driver implements, the error
// classes and the in-process registry. A driver manages one kind of storage
// and acts on one endpoint at a time; it knows nothing of policies, topology
// or jobs (PLG-11).
package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
)

// Driver is the factory of one kind of storage, registered once.
type Driver interface {
	// Name is the endpoints' driver field: "file", "s3".
	Name() string
	// Schemes are the URL schemes ParseRef understands.
	Schemes() []string
	Capabilities() Capabilities
	// ConfigSchema is the JSON Schema of an endpoint's configuration (PLG-10).
	ConfigSchema() []byte
	// ParseRef turns a URL of one of Schemes into a reference.
	ParseRef(url string) (Ref, error)
	// Open binds a client to an endpoint's configuration and credentials. It
	// is called per transfer attempt: cheap and safe for concurrent use.
	Open(ctx context.Context, cfg Config, creds Credentials) (Client, error)
}

// Capabilities are what a driver can do; the core adapts to them (PLG-9).
type Capabilities struct {
	Source          bool
	Sink            bool
	RangeRead       bool // required for Source
	ResumableUpload bool // Resume works in another process, from Upload.Token
	ParallelParts   bool // parts may be written out of order and concurrently
	NativeChecksum  []string
	MaxParts        int
	MinPartSize     int64 // every part but the last
	MaxPartSize     int64
	ServerSideCopy  bool
}

// Ref names an object: Container is the bucket (empty: the endpoint's
// default), Key the object's name inside it.
type Ref struct {
	Container string `json:"container,omitempty"`
	Key       string `json:"key"`
}

// Config is an endpoint's configuration, as stored in endpoints.config.
type Config = json.RawMessage

// Credentials are an endpoint's secrets, passed per Open and never persisted
// or logged by a driver (PLG-5).
type Credentials map[string]string

// Client is a driver bound to one endpoint.
type Client interface {
	Source
	Sink
	Close() error
}

// Source reads objects.
type Source interface {
	Stat(ctx context.Context, ref Ref) (ObjectInfo, error)
	// ReadRange returns [off, off+length). With a non-empty ifVersion it
	// fails with class SourceChanged when the object's version differs.
	ReadRange(ctx context.Context, ref Ref, off, length int64, ifVersion string) (io.ReadCloser, error)
}

// Sink writes objects, through uploads invisible under the final name until
// they are committed (PLG-8).
type Sink interface {
	// Begin starts an upload of ref.
	Begin(ctx context.Context, ref Ref, info ObjectInfo, opts PutOptions) (Upload, error)
	// Resume re-attaches to an upload from its Token; class ResumeInvalid when
	// the upload is gone.
	Resume(ctx context.Context, ref Ref, token []byte) (Upload, error)
	Delete(ctx context.Context, ref Ref) error
}

// Upload is an upload in progress.
type Upload interface {
	// Token is opaque and serialisable; the core persists it.
	Token() []byte
	// WritePart writes part partNo (from 0) of length bytes; writing the same
	// part again with the same bytes is harmless.
	WritePart(ctx context.Context, partNo int, r io.Reader, length int64) (PartResult, error)
	// ListParts returns the parts written so far, by part number.
	ListParts(ctx context.Context) ([]PartResult, error)
	// Commit publishes the object atomically; committing again with the same
	// parts returns the object (PLG-7).
	Commit(ctx context.Context, parts []PartResult) (ObjectInfo, error)
	// Abort discards the upload; aborting one that is gone is no error.
	Abort(ctx context.Context) error
}

// ObjectInfo describes an object.
type ObjectInfo struct {
	Size      int64             `json:"size"`
	Version   string            `json:"version,omitempty"`
	Checksums map[string]string `json:"checksums,omitempty"` // algorithm to hex
	Metadata  map[string]string `json:"metadata,omitempty"`
}

// PutOptions are the settings of an upload.
type PutOptions struct {
	ContentType string            `json:"content_type,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

// PartResult is a written part.
type PartResult struct {
	PartNo int    `json:"part_no"`
	Size   int64  `json:"size"`
	ETag   string `json:"etag,omitempty"`
	SHA256 string `json:"sha256,omitempty"` // hex, of the bytes written; empty when unknown (ListParts)
}

// DecodeSettings decodes a driver's settings, a JSON object, into v,
// refusing keys v does not have, so that a misspelled setting is not dropped
// (veho-plugin-distribution.md §4). Empty settings are {}.
func DecodeSettings(settings json.RawMessage, v any) error {
	if len(bytes.TrimSpace(settings)) == 0 {
		settings = json.RawMessage("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(settings))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("settings: %w", err)
	}
	return nil
}
