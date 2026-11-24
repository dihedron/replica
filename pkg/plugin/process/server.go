package process

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/dihedron/veho/pkg/plugin"
	pb "github.com/dihedron/veho/pkg/plugin/process/proto"
	goplugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// chunkSize is the size of the data messages.
const chunkSize = 1 << 20

// Factory builds a driver from its settings, a JSON object
// (veho-plugin-distribution.md §4); it refuses settings it does not know.
type Factory func(settings json.RawMessage) (plugin.Driver, error)

// Serve builds the driver from the settings in SettingsEnv and serves it as
// a process plugin, on the channel go-plugin sets up with the core. Settings
// the factory refuses end the process before the handshake.
func Serve(factory Factory) {
	settings := os.Getenv(SettingsEnv)
	if settings == "" {
		settings = "{}"
	}
	d, err := factory(json.RawMessage(settings))
	if err != nil {
		// before the handshake: Load reports this line
		fmt.Fprintf(os.Stderr, "settings refused: %v\n", err)
		os.Exit(1)
	}
	goplugin.Serve(&goplugin.ServeConfig{
		HandshakeConfig: Handshake,
		Plugins:         goplugin.PluginSet{pluginName: &grpcPlugin{impl: d}},
		GRPCServer:      goplugin.DefaultGRPCServer,
	})
}

// server holds the clients and uploads of the core, by handle. They live as
// long as the process; a crash loses them, and the core begins again from
// its tokens (PLG-18).
type server struct {
	pb.UnimplementedDriverServer
	d       plugin.Driver
	mu      sync.Mutex
	clients map[string]plugin.Client
	uploads map[string]upload
}

type upload struct {
	client string
	up     plugin.Upload
}

func newServer(d plugin.Driver) *server {
	return &server{d: d, clients: map[string]plugin.Client{}, uploads: map[string]upload{}}
}

func handle() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b) // never fails
	return hex.EncodeToString(b)
}

var errNoHandle = status.Error(codes.FailedPrecondition, "plugin: unknown handle")

func (s *server) client(id string) (plugin.Client, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.clients[id]
	if !ok {
		return nil, errNoHandle
	}
	return c, nil
}

func (s *server) upload(id string) (plugin.Upload, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.uploads[id]
	if !ok {
		return nil, errNoHandle
	}
	return u.up, nil
}

// unary returns err as a status, with its class in the call's trailer.
func unary(ctx context.Context, err error) error {
	return toStatus(err, func(md metadata.MD) { _ = grpc.SetTrailer(ctx, md) })
}

func (s *server) Describe(context.Context, *pb.Empty) (*pb.Description, error) {
	return &pb.Description{Name: s.d.Name(), Schemes: s.d.Schemes(), Capabilities: toCaps(s.d.Capabilities()), ConfigSchema: s.d.ConfigSchema()}, nil
}

func (s *server) ParseRef(ctx context.Context, r *pb.ParseRefRequest) (*pb.Ref, error) {
	ref, err := s.d.ParseRef(r.GetUrl())
	if err != nil {
		return nil, unary(ctx, err)
	}
	return toRef(ref), nil
}

func (s *server) Open(ctx context.Context, r *pb.OpenRequest) (*pb.Handle, error) {
	c, err := s.d.Open(ctx, r.GetConfig(), r.GetCredentials())
	if err != nil {
		return nil, unary(ctx, err)
	}
	id := handle()
	s.mu.Lock()
	s.clients[id] = c
	s.mu.Unlock()
	return &pb.Handle{Id: id}, nil
}

// Close closes a client and forgets its uploads.
func (s *server) Close(ctx context.Context, h *pb.Handle) (*pb.Empty, error) {
	s.mu.Lock()
	c, ok := s.clients[h.GetId()]
	delete(s.clients, h.GetId())
	for id, u := range s.uploads {
		if u.client == h.GetId() {
			delete(s.uploads, id)
		}
	}
	s.mu.Unlock()
	if !ok {
		return &pb.Empty{}, nil
	}
	return &pb.Empty{}, unary(ctx, c.Close())
}

func (s *server) Stat(ctx context.Context, r *pb.StatRequest) (*pb.ObjectInfo, error) {
	c, err := s.client(r.GetClient())
	if err != nil {
		return nil, err
	}
	info, err := c.Stat(ctx, fromRef(r.GetRef()))
	if err != nil {
		return nil, unary(ctx, err)
	}
	return toInfo(info), nil
}

func (s *server) ReadRange(r *pb.ReadRangeRequest, stream grpc.ServerStreamingServer[pb.Chunk]) error {
	ctx := stream.Context()
	fail := func(err error) error { return toStatus(err, stream.SetTrailer) }
	c, err := s.client(r.GetClient())
	if err != nil {
		return err
	}
	rc, err := c.ReadRange(ctx, fromRef(r.GetRef()), r.GetOffset(), r.GetLength(), r.GetIfVersion())
	if err != nil {
		return fail(err)
	}
	defer func() { _ = rc.Close() }()
	buf := make([]byte, chunkSize)
	for {
		n, err := io.ReadFull(rc, buf)
		if n > 0 {
			if serr := stream.Send(&pb.Chunk{Data: buf[:n]}); serr != nil {
				return serr
			}
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil
		}
		if err != nil {
			return fail(err)
		}
	}
}

func (s *server) Delete(ctx context.Context, r *pb.StatRequest) (*pb.Empty, error) {
	c, err := s.client(r.GetClient())
	if err != nil {
		return nil, err
	}
	return &pb.Empty{}, unary(ctx, c.Delete(ctx, fromRef(r.GetRef())))
}

func (s *server) keep(client string, up plugin.Upload) *pb.UploadHandle {
	id := handle()
	s.mu.Lock()
	s.uploads[id] = upload{client: client, up: up}
	s.mu.Unlock()
	return &pb.UploadHandle{Id: id, Token: up.Token()}
}

func (s *server) Begin(ctx context.Context, r *pb.BeginRequest) (*pb.UploadHandle, error) {
	c, err := s.client(r.GetClient())
	if err != nil {
		return nil, err
	}
	up, err := c.Begin(ctx, fromRef(r.GetRef()), fromInfo(r.GetInfo()), plugin.PutOptions{ContentType: r.GetContentType(), Metadata: r.GetMetadata()})
	if err != nil {
		return nil, unary(ctx, err)
	}
	return s.keep(r.GetClient(), up), nil
}

func (s *server) Resume(ctx context.Context, r *pb.ResumeRequest) (*pb.UploadHandle, error) {
	c, err := s.client(r.GetClient())
	if err != nil {
		return nil, err
	}
	up, err := c.Resume(ctx, fromRef(r.GetRef()), r.GetToken())
	if err != nil {
		return nil, unary(ctx, err)
	}
	return s.keep(r.GetClient(), up), nil
}

// streamReader reads the data messages of a WritePart stream.
type streamReader struct {
	stream grpc.ClientStreamingServer[pb.WritePartRequest, pb.PartResult]
	buf    []byte
}

func (r *streamReader) Read(p []byte) (int, error) {
	for len(r.buf) == 0 {
		m, err := r.stream.Recv()
		if err != nil {
			return 0, err // io.EOF when the core has sent everything
		}
		r.buf = m.GetData()
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}

func (s *server) WritePart(stream grpc.ClientStreamingServer[pb.WritePartRequest, pb.PartResult]) error {
	ctx := stream.Context()
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	h := first.GetHeader()
	if h == nil {
		return status.Error(codes.InvalidArgument, "plugin: WritePart starts with its header")
	}
	up, err := s.upload(h.GetUpload())
	if err != nil {
		return err
	}
	res, err := up.WritePart(ctx, int(h.GetPartNo()), &streamReader{stream: stream}, h.GetLength())
	if err != nil {
		return toStatus(err, stream.SetTrailer)
	}
	return stream.SendAndClose(toPart(res))
}

func (s *server) ListParts(ctx context.Context, h *pb.Handle) (*pb.PartList, error) {
	up, err := s.upload(h.GetId())
	if err != nil {
		return nil, err
	}
	parts, err := up.ListParts(ctx)
	if err != nil {
		return nil, unary(ctx, err)
	}
	out := &pb.PartList{}
	for _, p := range parts {
		out.Parts = append(out.Parts, toPart(p))
	}
	return out, nil
}

func (s *server) Commit(ctx context.Context, r *pb.CommitRequest) (*pb.ObjectInfo, error) {
	up, err := s.upload(r.GetUpload())
	if err != nil {
		return nil, err
	}
	parts := make([]plugin.PartResult, len(r.GetParts()))
	for i, p := range r.GetParts() {
		parts[i] = fromPart(p)
	}
	info, err := up.Commit(ctx, parts)
	if err != nil {
		return nil, unary(ctx, err)
	}
	return toInfo(info), nil
}

func (s *server) Abort(ctx context.Context, h *pb.Handle) (*pb.Empty, error) {
	up, err := s.upload(h.GetId())
	if err != nil {
		return nil, err
	}
	return &pb.Empty{}, unary(ctx, up.Abort(ctx))
}
