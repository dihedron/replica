package process

import (
	"bytes"
	"context"
	"errors"
	"io"
	"time"

	"github.com/dihedron/veho/pkg/plugin"
	pb "github.com/dihedron/veho/pkg/plugin/process/proto"
	"google.golang.org/grpc/metadata"
)

// remote is the core's side of a plugin process: a plugin.Driver whose
// calls go over gRPC.
type remote struct {
	rpc  pb.DriverClient
	desc *pb.Description
}

func (d *remote) Name() string                      { return d.desc.GetName() }
func (d *remote) Schemes() []string                 { return d.desc.GetSchemes() }
func (d *remote) Capabilities() plugin.Capabilities { return fromCaps(d.desc.GetCapabilities()) }
func (d *remote) ConfigSchema() []byte              { return d.desc.GetConfigSchema() }

func (d *remote) ParseRef(url string) (plugin.Ref, error) {
	ctx := context.Background()
	var md metadata.MD
	r, err := d.rpc.ParseRef(ctx, &pb.ParseRefRequest{Url: url}, trailerOf(&md))
	if err != nil {
		return plugin.Ref{}, fromStatus(ctx, err, md)
	}
	return fromRef(r), nil
}

func (d *remote) Open(ctx context.Context, cfg plugin.Config, creds plugin.Credentials) (plugin.Client, error) {
	var md metadata.MD
	h, err := d.rpc.Open(ctx, &pb.OpenRequest{Config: cfg, Credentials: creds}, trailerOf(&md))
	if err != nil {
		return nil, fromStatus(ctx, err, md)
	}
	return &remoteClient{rpc: d.rpc, id: h.GetId()}, nil
}

type remoteClient struct {
	rpc pb.DriverClient
	id  string
}

func (c *remoteClient) Close() error {
	ctx := context.Background()
	var md metadata.MD
	_, err := c.rpc.Close(ctx, &pb.Handle{Id: c.id}, trailerOf(&md))
	return fromStatus(ctx, err, md)
}

func (c *remoteClient) Stat(ctx context.Context, ref plugin.Ref) (plugin.ObjectInfo, error) {
	var md metadata.MD
	info, err := c.rpc.Stat(ctx, &pb.StatRequest{Client: c.id, Ref: toRef(ref)}, trailerOf(&md))
	if err != nil {
		return plugin.ObjectInfo{}, fromStatus(ctx, err, md)
	}
	return fromInfo(info), nil
}

// ReadRange receives the first chunk before it returns, so that the
// driver's error (not found, source changed) is ReadRange's.
func (c *remoteClient) ReadRange(ctx context.Context, ref plugin.Ref, off, length int64, ifVersion string) (io.ReadCloser, error) {
	ctx, cancel := context.WithCancel(ctx)
	stream, err := c.rpc.ReadRange(ctx, &pb.ReadRangeRequest{Client: c.id, Ref: toRef(ref), Offset: off, Length: length, IfVersion: ifVersion})
	if err != nil {
		err = fromStatus(ctx, err, nil)
		cancel()
		return nil, err
	}
	first, err := stream.Recv()
	if errors.Is(err, io.EOF) {
		cancel()
		return io.NopCloser(bytes.NewReader(nil)), nil
	}
	if err != nil {
		err = fromStatus(ctx, err, stream.Trailer())
		cancel()
		return nil, err
	}
	return &chunkReader{ctx: ctx, cancel: cancel, recv: stream.Recv, trailer: stream.Trailer, buf: first.GetData()}, nil
}

// chunkReader reads a stream of chunks; Close ends the stream.
type chunkReader struct {
	ctx     context.Context
	cancel  context.CancelFunc
	recv    func() (*pb.Chunk, error)
	trailer func() metadata.MD
	buf     []byte
}

func (r *chunkReader) Read(p []byte) (int, error) {
	for len(r.buf) == 0 {
		m, err := r.recv()
		if errors.Is(err, io.EOF) {
			return 0, io.EOF
		}
		if err != nil {
			return 0, fromStatus(r.ctx, err, r.trailer())
		}
		r.buf = m.GetData()
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}

func (r *chunkReader) Close() error {
	r.cancel()
	return nil
}

func (c *remoteClient) Delete(ctx context.Context, ref plugin.Ref) error {
	var md metadata.MD
	_, err := c.rpc.Delete(ctx, &pb.StatRequest{Client: c.id, Ref: toRef(ref)}, trailerOf(&md))
	return fromStatus(ctx, err, md)
}

func (c *remoteClient) Begin(ctx context.Context, ref plugin.Ref, info plugin.ObjectInfo, opts plugin.PutOptions) (plugin.Upload, error) {
	var md metadata.MD
	h, err := c.rpc.Begin(ctx, &pb.BeginRequest{Client: c.id, Ref: toRef(ref), Info: toInfo(info), ContentType: opts.ContentType, Metadata: opts.Metadata}, trailerOf(&md))
	if err != nil {
		return nil, fromStatus(ctx, err, md)
	}
	return &remoteUpload{rpc: c.rpc, id: h.GetId(), token: h.GetToken(), pullTTL: time.Duration(h.GetPullTtlSeconds()) * time.Second}, nil
}

func (c *remoteClient) Resume(ctx context.Context, ref plugin.Ref, token []byte) (plugin.Upload, error) {
	var md metadata.MD
	h, err := c.rpc.Resume(ctx, &pb.ResumeRequest{Client: c.id, Ref: toRef(ref), Token: token}, trailerOf(&md))
	if err != nil {
		return nil, fromStatus(ctx, err, md)
	}
	return &remoteUpload{rpc: c.rpc, id: h.GetId(), token: h.GetToken(), pullTTL: time.Duration(h.GetPullTtlSeconds()) * time.Second}, nil
}

type remoteUpload struct {
	rpc     pb.DriverClient
	id      string
	token   []byte
	pullTTL time.Duration
}

// Flows asks the plugin; a driver without flows answers none.
func (c *remoteClient) Flows(ctx context.Context) ([]plugin.Flow, error) {
	var md metadata.MD
	l, err := c.rpc.Flows(ctx, &pb.Handle{Id: c.id}, trailerOf(&md))
	if err != nil {
		return nil, fromStatus(ctx, err, md)
	}
	if len(l.GetFlows()) > plugin.MaxFlows {
		return nil, plugin.Wrapf(plugin.Permanent, "the plugin lists %d flows, more than %d", len(l.GetFlows()), plugin.MaxFlows)
	}
	var flows []plugin.Flow
	for _, f := range l.GetFlows() {
		flows = append(flows, plugin.Flow{Purpose: f.GetPurpose(), Host: f.GetHost(), Port: int(f.GetPort()), TLS: f.GetTls(),
			ServerName: f.GetServerName(), Handshake: f.GetHandshake(), CACert: f.GetCaCert(), Unprobed: f.GetUnprobed()})
	}
	return flows, nil
}

// PresignRead asks the plugin; a driver without it answers Permanent.
func (c *remoteClient) PresignRead(ctx context.Context, ref plugin.Ref, version string, ttl time.Duration) (string, error) {
	var md metadata.MD
	u, err := c.rpc.PresignRead(ctx, &pb.PresignRequest{Client: c.id, Ref: toRef(ref), Version: version, TtlSeconds: int64(ttl / time.Second)}, trailerOf(&md))
	if err != nil {
		return "", fromStatus(ctx, err, md)
	}
	return u.GetUrl(), nil
}

func (u *remoteUpload) PullTTL() time.Duration { return u.pullTTL }

func (u *remoteUpload) Pull(ctx context.Context, url string) (plugin.Ref, error) {
	var md metadata.MD
	r, err := u.rpc.Pull(ctx, &pb.PullRequest{Upload: u.id, Url: url}, trailerOf(&md))
	if err != nil {
		return plugin.Ref{}, fromStatus(ctx, err, md)
	}
	return fromRef(r), nil
}

func (u *remoteUpload) Token() []byte { return u.token }

// WritePart sends the header, then the data in chunks; a reader that ends
// early sends less, and the driver reports it.
func (u *remoteUpload) WritePart(ctx context.Context, partNo int, r io.Reader, length int64) (plugin.PartResult, error) {
	stream, err := u.rpc.WritePart(ctx)
	if err != nil {
		return plugin.PartResult{}, fromStatus(ctx, err, nil)
	}
	header := &pb.WritePartRequest{Msg: &pb.WritePartRequest_Header_{Header: &pb.WritePartRequest_Header{Upload: u.id, PartNo: int64(partNo), Length: length}}}
	if err := stream.Send(header); err != nil {
		_, err = stream.CloseAndRecv()
		return plugin.PartResult{}, fromStatus(ctx, err, stream.Trailer())
	}
	buf := make([]byte, chunkSize)
	for sent := int64(0); sent < length; {
		n, rerr := io.ReadFull(r, buf[:min(int64(chunkSize), length-sent)])
		if n > 0 {
			if err := stream.Send(&pb.WritePartRequest{Msg: &pb.WritePartRequest_Data{Data: buf[:n]}}); err != nil {
				break // the server's answer says why
			}
			sent += int64(n)
		}
		if rerr != nil {
			if ctx.Err() != nil {
				return plugin.PartResult{}, ctx.Err()
			}
			break
		}
	}
	res, err := stream.CloseAndRecv()
	if err != nil {
		return plugin.PartResult{}, fromStatus(ctx, err, stream.Trailer())
	}
	return fromPart(res), nil
}

func (u *remoteUpload) ListParts(ctx context.Context) ([]plugin.PartResult, error) {
	var md metadata.MD
	list, err := u.rpc.ListParts(ctx, &pb.Handle{Id: u.id}, trailerOf(&md))
	if err != nil {
		return nil, fromStatus(ctx, err, md)
	}
	parts := make([]plugin.PartResult, len(list.GetParts()))
	for i, p := range list.GetParts() {
		parts[i] = fromPart(p)
	}
	return parts, nil
}

func (u *remoteUpload) Commit(ctx context.Context, parts []plugin.PartResult) (plugin.ObjectInfo, error) {
	req := &pb.CommitRequest{Upload: u.id}
	for _, p := range parts {
		req.Parts = append(req.Parts, toPart(p))
	}
	var md metadata.MD
	info, err := u.rpc.Commit(ctx, req, trailerOf(&md))
	if err != nil {
		return plugin.ObjectInfo{}, fromStatus(ctx, err, md)
	}
	return fromInfo(info), nil
}

func (u *remoteUpload) Abort(ctx context.Context) error {
	var md metadata.MD
	_, err := u.rpc.Abort(ctx, &pb.Handle{Id: u.id}, trailerOf(&md))
	return fromStatus(ctx, err, md)
}
