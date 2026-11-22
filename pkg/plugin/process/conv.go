package process

import (
	"context"
	"errors"
	"strconv"

	"github.com/dihedron/veho/pkg/plugin"
	pb "github.com/dihedron/veho/pkg/plugin/process/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// The trailer keys of an error's class.
const (
	trailerClass    = "veho-class"
	trailerNotFound = "veho-not-found"
)

func toRef(r plugin.Ref) *pb.Ref   { return &pb.Ref{Container: r.Container, Key: r.Key} }
func fromRef(r *pb.Ref) plugin.Ref { return plugin.Ref{Container: r.GetContainer(), Key: r.GetKey()} }
func toPart(p plugin.PartResult) *pb.PartResult {
	return &pb.PartResult{PartNo: int64(p.PartNo), Size: p.Size, Etag: p.ETag, Sha256: p.SHA256}
}
func fromPart(p *pb.PartResult) plugin.PartResult {
	return plugin.PartResult{PartNo: int(p.GetPartNo()), Size: p.GetSize(), ETag: p.GetEtag(), SHA256: p.GetSha256()}
}

func toInfo(i plugin.ObjectInfo) *pb.ObjectInfo {
	return &pb.ObjectInfo{Size: i.Size, Version: i.Version, Checksums: i.Checksums, Metadata: i.Metadata}
}
func fromInfo(i *pb.ObjectInfo) plugin.ObjectInfo {
	return plugin.ObjectInfo{Size: i.GetSize(), Version: i.GetVersion(), Checksums: i.GetChecksums(), Metadata: i.GetMetadata()}
}

func toCaps(c plugin.Capabilities) *pb.Capabilities {
	return &pb.Capabilities{
		Source: c.Source, Sink: c.Sink, RangeRead: c.RangeRead, ResumableUpload: c.ResumableUpload,
		ParallelParts: c.ParallelParts, NativeChecksum: c.NativeChecksum, MaxParts: int64(c.MaxParts),
		MinPartSize: c.MinPartSize, MaxPartSize: c.MaxPartSize, ServerSideCopy: c.ServerSideCopy,
	}
}
func fromCaps(c *pb.Capabilities) plugin.Capabilities {
	return plugin.Capabilities{
		Source: c.GetSource(), Sink: c.GetSink(), RangeRead: c.GetRangeRead(), ResumableUpload: c.GetResumableUpload(),
		ParallelParts: c.GetParallelParts(), NativeChecksum: c.GetNativeChecksum(), MaxParts: int(c.GetMaxParts()),
		MinPartSize: c.GetMinPartSize(), MaxPartSize: c.GetMaxPartSize(), ServerSideCopy: c.GetServerSideCopy(),
	}
}

// toStatus turns a driver's error into a gRPC status, and sets its class in
// the trailer through setTrailer.
func toStatus(err error, setTrailer func(metadata.MD)) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return status.Error(codes.Canceled, err.Error())
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return status.Error(codes.DeadlineExceeded, err.Error())
	}
	class := plugin.ClassOf(err)
	md := metadata.Pairs(trailerClass, strconv.Itoa(int(class)))
	code := codes.Unknown
	switch class {
	case plugin.Transient:
		code = codes.Unavailable
	case plugin.Permanent, plugin.SourceChanged:
		code = codes.FailedPrecondition
	case plugin.ResumeInvalid:
		code = codes.Aborted
	case plugin.Integrity:
		code = codes.DataLoss
	}
	if errors.Is(err, plugin.ErrNotFound) {
		md.Set(trailerNotFound, "1")
		code = codes.NotFound
	}
	setTrailer(md)
	return status.Error(code, err.Error())
}

// fromStatus turns a call's error back into a driver's error: the context's
// error when ctx is done, the class of the trailer, and Transient for a lost
// or crashed plugin (PLG-18).
func fromStatus(ctx context.Context, err error, md metadata.MD) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	st := status.Convert(err)
	msg := errors.New(st.Message())
	if len(md.Get(trailerNotFound)) > 0 {
		return errors.Join(plugin.ErrNotFound, msg)
	}
	if v := md.Get(trailerClass); len(v) > 0 {
		if n, cerr := strconv.Atoi(v[0]); cerr == nil {
			return plugin.Wrap(msg, plugin.Class(n))
		}
	}
	switch st.Code() {
	case codes.Unavailable, codes.Canceled, codes.DeadlineExceeded, codes.Internal:
		return plugin.Wrap(err, plugin.Transient)
	default:
		return err
	}
}

// trailerOf collects a unary call's trailer.
func trailerOf(md *metadata.MD) grpc.CallOption { return grpc.Trailer(md) }
