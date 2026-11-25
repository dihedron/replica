// Package s3 is the S3 storage driver (.specs/veho-plugins.md §7), on the
// AWS SDK for Go v2, for AWS and S3-compatible services (Garage in the lab).
// Uploads are multipart uploads, invisible until they are completed; each
// part is buffered in memory, so that its MD5 goes with it and the server
// verifies it.
package s3

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // Content-MD5 and multipart ETags are MD5 by protocol, not for security
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/dihedron/veho/pkg/plugin"
)

const (
	maxParts    = 10000
	minPartSize = 5 << 20
	// shortcut: parts are buffered in memory for their Content-MD5, so the
	// part size is capped below S3's 5 GiB; stream with a trailing checksum
	// if larger parts are ever needed.
	maxPartSize = 512 << 20
)

// Driver is the S3 driver.
type Driver struct{}

// New returns the S3 driver.
func New() Driver { return Driver{} }

// Factory builds the driver from its settings; it has none yet, so any key
// is refused (veho-plugin-distribution.md §4).
func Factory(settings json.RawMessage) (plugin.Driver, error) {
	var none struct{}
	if err := plugin.DecodeSettings(settings, &none); err != nil {
		return nil, fmt.Errorf("s3: %w", err)
	}
	return New(), nil
}

func (Driver) Name() string      { return "s3" }
func (Driver) Schemes() []string { return []string{"s3"} }

func (Driver) Capabilities() plugin.Capabilities {
	return plugin.Capabilities{
		Source: true, Sink: true, RangeRead: true, ResumableUpload: true, ParallelParts: true,
		NativeChecksum: []string{"md5"}, MaxParts: maxParts, MinPartSize: minPartSize, MaxPartSize: maxPartSize,
	}
}

// schema is the endpoint configuration's JSON Schema (PLG-10).
//
//go:embed schema.json
var schema []byte

func (Driver) ConfigSchema() []byte { return schema }

// ParseRef parses s3://bucket/key.
func (Driver) ParseRef(raw string) (plugin.Ref, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "s3" || u.Host == "" || len(u.Path) < 2 {
		return plugin.Ref{}, plugin.Wrapf(plugin.Permanent, "s3: %q is not s3://bucket/key", raw)
	}
	return plugin.Ref{Container: u.Host, Key: u.Path[1:]}, nil
}

type config struct {
	Endpoint  string `json:"endpoint"`
	Region    string `json:"region"`
	Bucket    string `json:"bucket"`
	PathStyle bool   `json:"path_style"`
	// StorageClass is sent on every write and checked after every commit
	// (veho-storage-class.md); empty: the backend's default, unchecked.
	StorageClass string `json:"storage_class"`
}

// archive are the classes that cannot be read without a restore, refused
// in v1 (veho-storage-class.md §1).
var archive = []string{"GLACIER", "DEEP_ARCHIVE"}

// Open builds a client with its own connections, closed by Close (no state
// shared across tenants, PLG-6). The SDK does not retry: the core does, by
// the class of the error.
func (Driver) Open(ctx context.Context, raw plugin.Config, creds plugin.Credentials) (plugin.Client, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var cfg config
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields() // a misspelled key must not be dropped
	if err := dec.Decode(&cfg); err != nil || cfg.Region == "" || cfg.Bucket == "" {
		return nil, plugin.Wrapf(plugin.Permanent, "s3: the configuration needs region and bucket, and no other keys than the schema's (%v)", err)
	}
	if slices.Contains(archive, cfg.StorageClass) {
		return nil, plugin.Wrapf(plugin.Permanent, "s3: storage class %s is an archive class, refused", cfg.StorageClass)
	}
	if creds["access_key_id"] == "" || creds["secret_access_key"] == "" {
		return nil, plugin.Wrapf(plugin.Permanent, "s3: the credentials need access_key_id and secret_access_key")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	opts := s3.Options{
		Region:                     cfg.Region,
		UsePathStyle:               cfg.PathStyle,
		Credentials:                credentials.NewStaticCredentialsProvider(creds["access_key_id"], creds["secret_access_key"], creds["session_token"]),
		HTTPClient:                 &http.Client{Transport: transport},
		Retryer:                    aws.NopRetryer{},
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	}
	if cfg.Endpoint != "" {
		opts.BaseEndpoint = aws.String(cfg.Endpoint)
	}
	return &client{s3: s3.New(opts), bucket: cfg.Bucket, class: cfg.StorageClass, transport: transport}, nil
}

type client struct {
	s3        *s3.Client
	bucket    string
	class     string
	transport *http.Transport
}

func (c *client) Close() error {
	c.transport.CloseIdleConnections()
	return nil
}

// where is the bucket and key of ref.
func (c *client) where(ref plugin.Ref) (string, string, error) {
	if ref.Key == "" {
		return "", "", plugin.Wrapf(plugin.Permanent, "s3: empty key")
	}
	if ref.Container != "" {
		return ref.Container, ref.Key, nil
	}
	return c.bucket, ref.Key, nil
}

// classify maps an SDK error to a class (veho-plugins.md §7); a cancelled
// or expired context is returned as the context's error.
func classify(ctx context.Context, op string, err error) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	err = fmt.Errorf("s3: %s: %w", op, err)
	code := ""
	if ae, ok := errors.AsType[smithy.APIError](err); ok {
		code = ae.ErrorCode()
	}
	status := 0
	if re, ok := errors.AsType[interface {
		error
		HTTPStatusCode() int
	}](err); ok {
		status = re.HTTPStatusCode()
	}
	switch {
	case code == "NoSuchKey" || code == "NotFound" || status == http.StatusNotFound && code == "":
		return fmt.Errorf("%w (%w)", plugin.ErrNotFound, err)
	case code == "NoSuchUpload":
		return plugin.Wrap(err, plugin.ResumeInvalid)
	case code == "BadDigest" || code == "InvalidDigest" || code == "XAmzContentSHA256Mismatch":
		return plugin.Wrap(err, plugin.Integrity)
	case code == "PreconditionFailed" || status == http.StatusPreconditionFailed:
		return plugin.Wrap(err, plugin.SourceChanged)
	case code == "SlowDown" || code == "Throttling" || code == "RequestTimeout" || status == http.StatusTooManyRequests || status >= 500:
		return plugin.Wrap(err, plugin.Transient)
	case status >= 400:
		return plugin.Wrap(err, plugin.Permanent)
	default: // no answer: the network
		return plugin.Wrap(err, plugin.Transient)
	}
}

func (c *client) Stat(ctx context.Context, ref plugin.Ref) (plugin.ObjectInfo, error) {
	info, _, err := c.head(ctx, ref, "")
	return info, err
}

// committed is the Stat that ends every commit: the object must be in the
// configured class, a missing header meaning STANDARD (veho-storage-class.md §2.2).
func (c *client) committed(ctx context.Context, ref plugin.Ref) (plugin.ObjectInfo, error) {
	info, class, err := c.head(ctx, ref, "")
	if err != nil || c.class == "" {
		return info, err
	}
	if class == "" {
		class = string(types.StorageClassStandard)
	}
	if class != c.class {
		return plugin.ObjectInfo{}, plugin.Wrapf(plugin.Permanent, "s3: %s stored as %s, not %s (the object is in place)", ref.Key, class, c.class)
	}
	return info, nil
}

// head is Stat, with If-Match when ifVersion is set, and the object's
// storage class as reported.
func (c *client) head(ctx context.Context, ref plugin.Ref, ifVersion string) (plugin.ObjectInfo, string, error) {
	bucket, key, err := c.where(ref)
	if err != nil {
		return plugin.ObjectInfo{}, "", err
	}
	in := &s3.HeadObjectInput{Bucket: &bucket, Key: &key}
	if ifVersion != "" {
		in.IfMatch = &ifVersion
	}
	out, err := c.s3.HeadObject(ctx, in)
	if err != nil {
		return plugin.ObjectInfo{}, "", classify(ctx, "head "+key, err)
	}
	return plugin.ObjectInfo{Size: aws.ToInt64(out.ContentLength), Version: aws.ToString(out.ETag), Metadata: out.Metadata}, string(out.StorageClass), nil
}

func (c *client) ReadRange(ctx context.Context, ref plugin.Ref, off, length int64, ifVersion string) (io.ReadCloser, error) {
	if off < 0 || length < 0 {
		return nil, plugin.Wrapf(plugin.Permanent, "s3: range [%d, +%d)", off, length)
	}
	if length == 0 { // no empty Range in HTTP: check that the object is there
		info, _, err := c.head(ctx, ref, ifVersion)
		if err != nil {
			return nil, err
		}
		if off > info.Size {
			return nil, plugin.Wrapf(plugin.Permanent, "s3: offset %d beyond %d bytes", off, info.Size)
		}
		return io.NopCloser(bytes.NewReader(nil)), nil
	}
	bucket, key, err := c.where(ref)
	if err != nil {
		return nil, err
	}
	rng := fmt.Sprintf("bytes=%d-%d", off, off+length-1)
	in := &s3.GetObjectInput{Bucket: &bucket, Key: &key, Range: &rng}
	if ifVersion != "" {
		in.IfMatch = &ifVersion
	}
	out, err := c.s3.GetObject(ctx, in)
	if err != nil {
		return nil, classify(ctx, "get "+key, err)
	}
	if n := aws.ToInt64(out.ContentLength); n != length {
		_ = out.Body.Close()
		return nil, plugin.Wrapf(plugin.Permanent, "s3: range [%d, +%d) of %s: %d bytes", off, length, key, n)
	}
	return out.Body, nil
}

// PresignRead is a presigned GET of the object for ttl, for a destination
// that pulls it. It carries no condition (the destination would not send
// it): the engine reads the copy back and compares it with the source
// (veho-openstack.md §3.2).
func (c *client) PresignRead(ctx context.Context, ref plugin.Ref, _ string, ttl time.Duration) (string, error) {
	bucket, key, err := c.where(ref)
	if err != nil {
		return "", err
	}
	if ttl <= 0 || ttl > 7*24*time.Hour { // SigV4's limit
		return "", plugin.Wrapf(plugin.Permanent, "s3: a presigned URL lives from 1 s to 7 days, not %s", ttl)
	}
	out, err := s3.NewPresignClient(c.s3).PresignGetObject(ctx, &s3.GetObjectInput{Bucket: &bucket, Key: &key}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", classify(ctx, "presign "+key, err)
	}
	return out.URL, nil
}

func (c *client) Delete(ctx context.Context, ref plugin.Ref) error {
	bucket, key, err := c.where(ref)
	if err != nil {
		return err
	}
	_, err = c.s3.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &bucket, Key: &key})
	if err = classify(ctx, "delete "+key, err); errors.Is(err, plugin.ErrNotFound) {
		return nil
	}
	return err
}

// --- uploads -----------------------------------------------------------------

type token struct {
	Bucket   string `json:"bucket"`
	Key      string `json:"key"`
	UploadID string `json:"upload_id"`
}

type upload struct {
	c *client
	t token
}

func (c *client) Begin(ctx context.Context, ref plugin.Ref, _ plugin.ObjectInfo, opts plugin.PutOptions) (plugin.Upload, error) {
	bucket, key, err := c.where(ref)
	if err != nil {
		return nil, err
	}
	in := &s3.CreateMultipartUploadInput{Bucket: &bucket, Key: &key, Metadata: opts.Metadata}
	if opts.ContentType != "" {
		in.ContentType = &opts.ContentType
	}
	in.StorageClass = types.StorageClass(c.class)
	out, err := c.s3.CreateMultipartUpload(ctx, in)
	if err != nil {
		return nil, classify(ctx, "begin "+key, err)
	}
	return &upload{c: c, t: token{Bucket: bucket, Key: key, UploadID: aws.ToString(out.UploadId)}}, nil
}

func (c *client) Resume(ctx context.Context, ref plugin.Ref, raw []byte) (plugin.Upload, error) {
	bucket, key, err := c.where(ref)
	if err != nil {
		return nil, err
	}
	var t token
	if err := json.Unmarshal(raw, &t); err != nil || t.UploadID == "" {
		return nil, plugin.Wrapf(plugin.ResumeInvalid, "s3: malformed upload token")
	}
	if t.Bucket != bucket || t.Key != key {
		return nil, plugin.Wrapf(plugin.ResumeInvalid, "s3: the upload token is of %s/%s", t.Bucket, t.Key)
	}
	u := &upload{c: c, t: t}
	if _, err := u.list(ctx, 1); err != nil {
		return nil, err
	}
	return u, nil
}

func (u *upload) Token() []byte {
	b, _ := json.Marshal(u.t) // strings only: cannot fail
	return b
}

func (u *upload) WritePart(ctx context.Context, partNo int, r io.Reader, length int64) (plugin.PartResult, error) {
	if partNo < 0 || partNo >= maxParts || length < 0 || length > maxPartSize {
		return plugin.PartResult{}, plugin.Wrapf(plugin.Permanent, "s3: part %d of %d bytes is out of range", partNo, length)
	}
	if length == 0 {
		// S3-compatible services refuse empty parts (Garage does): an empty
		// object is one empty part, published by Commit with a PutObject
		if partNo != 0 {
			return plugin.PartResult{}, plugin.Wrapf(plugin.Permanent, "s3: part %d is empty: only an empty object has an empty part", partNo)
		}
		return plugin.PartResult{PartNo: 0, Size: 0, ETag: emptyMD5, SHA256: emptySHA256}, nil
	}
	data := make([]byte, length)
	if _, err := io.ReadFull(r, data); err != nil {
		if ctx.Err() != nil {
			return plugin.PartResult{}, ctx.Err()
		}
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) && length > 0 {
			return plugin.PartResult{}, plugin.Wrapf(plugin.Integrity, "s3: part %d: short read: %v", partNo, err)
		}
		return plugin.PartResult{}, plugin.Wrap(fmt.Errorf("s3: part %d: %w", partNo, err), plugin.Transient)
	}
	md := md5.Sum(data) //nolint:gosec // Content-MD5, by protocol
	sha := sha256.Sum256(data)
	out, err := u.c.s3.UploadPart(ctx, &s3.UploadPartInput{
		Bucket: &u.t.Bucket, Key: &u.t.Key, UploadId: &u.t.UploadID,
		PartNumber:    aws.Int32(int32(partNo + 1)), //nolint:gosec // partNo < maxParts
		ContentLength: aws.Int64(length), ContentMD5: aws.String(base64.StdEncoding.EncodeToString(md[:])),
		Body: bytes.NewReader(data),
	})
	if err != nil {
		return plugin.PartResult{}, classify(ctx, fmt.Sprintf("part %d of %s", partNo, u.t.Key), err)
	}
	return plugin.PartResult{PartNo: partNo, Size: length, ETag: aws.ToString(out.ETag), SHA256: hex.EncodeToString(sha[:])}, nil
}

func (u *upload) ListParts(ctx context.Context) ([]plugin.PartResult, error) {
	return u.list(ctx, 0)
}

// list lists the parts, all of them, or the first page of at most limit.
func (u *upload) list(ctx context.Context, limit int32) ([]plugin.PartResult, error) {
	var parts []plugin.PartResult
	in := &s3.ListPartsInput{Bucket: &u.t.Bucket, Key: &u.t.Key, UploadId: &u.t.UploadID}
	if limit > 0 {
		in.MaxParts = &limit
	}
	for {
		out, err := u.c.s3.ListParts(ctx, in)
		if err != nil {
			return nil, classify(ctx, "list parts of "+u.t.Key, err)
		}
		for _, p := range out.Parts {
			parts = append(parts, plugin.PartResult{PartNo: int(aws.ToInt32(p.PartNumber)) - 1, Size: aws.ToInt64(p.Size), ETag: aws.ToString(p.ETag)})
		}
		if limit > 0 || !aws.ToBool(out.IsTruncated) {
			break
		}
		in.PartNumberMarker = out.NextPartNumberMarker
	}
	slices.SortFunc(parts, func(a, b plugin.PartResult) int { return a.PartNo - b.PartNo })
	return parts, nil
}

func (u *upload) Commit(ctx context.Context, parts []plugin.PartResult) (plugin.ObjectInfo, error) {
	if len(parts) == 1 && parts[0].PartNo == 0 && parts[0].Size == 0 {
		return u.commitEmpty(ctx)
	}
	completed := make([]types.CompletedPart, len(parts))
	for i, p := range parts {
		if p.PartNo != i {
			return plugin.ObjectInfo{}, plugin.Wrapf(plugin.Permanent, "s3: parts must be 0 to %d in order, got %d at %d", len(parts)-1, p.PartNo, i)
		}
		completed[i] = types.CompletedPart{ETag: aws.String(p.ETag), PartNumber: aws.Int32(int32(i + 1))} //nolint:gosec // i < maxParts
	}
	_, err := u.c.s3.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: &u.t.Bucket, Key: &u.t.Key, UploadId: &u.t.UploadID,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: completed},
	})
	ref := plugin.Ref{Container: u.t.Bucket, Key: u.t.Key}
	if refused(err) {
		// the upload does not hold the parts given (another execution began
		// it again): begin again, not a permanent failure (found by AC-17)
		err = plugin.Wrap(fmt.Errorf("s3: commit %s: %w", u.t.Key, err), plugin.ResumeInvalid)
	} else {
		err = classify(ctx, "commit "+u.t.Key, err)
	}
	if plugin.ClassOf(err) == plugin.ResumeInvalid {
		// committed before? then the object carries the parts' ETag
		info, serr := u.c.committed(ctx, ref)
		if serr == nil && strings.Trim(info.Version, `"`) == multipartETag(parts) {
			return info, nil
		}
		return plugin.ObjectInfo{}, err
	}
	if err != nil {
		return plugin.ObjectInfo{}, err
	}
	return u.c.committed(ctx, ref)
}

// refused is true when CompleteMultipartUpload was refused because the
// upload does not hold the parts given: AWS's InvalidPart and
// InvalidPartOrder, and Garage's InvalidRequest "No data was uploaded".
func refused(err error) bool {
	ae, ok := errors.AsType[smithy.APIError](err)
	if !ok {
		return false
	}
	switch ae.ErrorCode() {
	case "InvalidPart", "InvalidPartOrder":
		return true
	case "InvalidRequest":
		return strings.Contains(ae.ErrorMessage(), "No data was uploaded")
	}
	return false
}

// The digests of zero bytes.
const (
	emptyMD5    = `"d41d8cd98f00b204e9800998ecf8427e"`
	emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

// commitEmpty publishes an empty object: the multipart upload is aborted
// and an empty PutObject, atomic too, takes its place. Committing again
// finds the upload gone and the empty object there.
func (u *upload) commitEmpty(ctx context.Context) (plugin.ObjectInfo, error) {
	ref := plugin.Ref{Container: u.t.Bucket, Key: u.t.Key}
	_, err := u.c.s3.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: &u.t.Bucket, Key: &u.t.Key, UploadId: &u.t.UploadID})
	if err = classify(ctx, "commit "+u.t.Key, err); plugin.ClassOf(err) == plugin.ResumeInvalid || errors.Is(err, plugin.ErrNotFound) {
		if info, serr := u.c.committed(ctx, ref); serr == nil && info.Size == 0 {
			return info, nil
		}
		return plugin.ObjectInfo{}, err
	}
	if err != nil {
		return plugin.ObjectInfo{}, err
	}
	if _, err := u.c.s3.PutObject(ctx, &s3.PutObjectInput{
		Bucket: &u.t.Bucket, Key: &u.t.Key, ContentLength: aws.Int64(0), Body: bytes.NewReader(nil),
		ContentMD5: aws.String("1B2M2Y8AsgTpgAmY7PhCfg=="), StorageClass: types.StorageClass(u.c.class),
	}); err != nil {
		return plugin.ObjectInfo{}, classify(ctx, "commit "+u.t.Key, err)
	}
	return u.c.committed(ctx, ref)
}

// multipartETag is the ETag S3 gives a multipart object: the MD5 of the
// parts' MD5s, a dash and the number of parts.
func multipartETag(parts []plugin.PartResult) string {
	h := md5.New() //nolint:gosec // the protocol's ETag
	for _, p := range parts {
		b, err := hex.DecodeString(strings.Trim(p.ETag, `"`))
		if err != nil {
			return ""
		}
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil)) + "-" + strconv.Itoa(len(parts))
}

func (u *upload) Abort(ctx context.Context) error {
	_, err := u.c.s3.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: &u.t.Bucket, Key: &u.t.Key, UploadId: &u.t.UploadID})
	if err = classify(ctx, "abort "+u.t.Key, err); plugin.ClassOf(err) == plugin.ResumeInvalid || errors.Is(err, plugin.ErrNotFound) {
		return nil
	}
	return err
}
