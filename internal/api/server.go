package api

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	vehov1 "github.com/dihedron/veho/api/veho/v1"
	"github.com/dihedron/veho/internal/engine"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// The limits of §2.
const (
	maxBody       = 1 << 20
	maxMessage    = 4 << 20
	defaultCall   = 30 * time.Second
	maxWait       = 60 * time.Second
	watchInterval = time.Second
)

// Options configure a Server.
type Options struct {
	Engine *engine.Engine
	// API is veho_api's pool, for authentication.
	API *pgxpool.Pool
	// MaxWatches bounds the WatchJob streams of the node.
	MaxWatches int
	// Ready says whether the node can serve (/readyz).
	Ready func(context.Context) error
	// Reauthorise is how often a stream re-checks its caller (API-11);
	// default a minute.
	Reauthorise time.Duration
	// Identities verifies OIDC and SPIFFE credentials; nil accepts API
	// keys only.
	Identities *Identities
}

// Server is the API of a node.
type Server struct {
	vehov1.UnimplementedJobServiceServer
	pol     policies
	engine  *engine.Engine
	api     *pgxpool.Pool
	grpc    *grpc.Server
	gateway http.Handler
	ready   func(context.Context) error
	health  *health.Server
	watches atomic.Int64
	max     int64
	reauth  time.Duration
	ids     *Identities
	pipe    *bufconn.Listener
	client  *grpc.ClientConn
}

// New builds the gRPC server, and the REST gateway that reaches it through
// an in-process connection, so that REST goes through the same
// interceptors (API-8, SEC-23).
func New(o Options) (*Server, error) {
	s := &Server{engine: o.Engine, api: o.API, ready: o.Ready, max: int64(o.MaxWatches), health: health.NewServer(), reauth: o.Reauthorise, ids: o.Identities}
	if s.reauth <= 0 {
		s.reauth = time.Minute
	}
	s.grpc = grpc.NewServer(
		grpc.ChainUnaryInterceptor(s.deadline, s.unary),
		grpc.ChainStreamInterceptor(s.stream),
		grpc.MaxRecvMsgSize(maxMessage), grpc.MaxSendMsgSize(maxMessage),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: 30 * time.Second, PermitWithoutStream: false}),
		grpc.KeepaliveParams(keepalive.ServerParameters{MaxConnectionIdle: 15 * time.Minute, Time: 2 * time.Minute, Timeout: 20 * time.Second}),
	)
	s.pol = policies{s: s}
	vehov1.RegisterJobServiceServer(s.grpc, s)
	vehov1.RegisterPolicyServiceServer(s.grpc, s.pol)
	healthpb.RegisterHealthServer(s.grpc, s.health)

	s.pipe = bufconn.Listen(1 << 20)
	go func() { _ = s.grpc.Serve(s.pipe) }()
	conn, err := grpc.NewClient("passthrough:///in-process",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return s.pipe.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(maxMessage)))
	if err != nil {
		return nil, fmt.Errorf("api: gateway: %w", err)
	}
	s.client = conn
	mux := runtime.NewServeMux(
		runtime.WithIncomingHeaderMatcher(func(h string) (string, bool) {
			switch k := strings.ToLower(h); k {
			case "authorization", "idempotency-key", "x-request-id", "if-none-match":
				return k, true
			}
			return "", false
		}),
		runtime.WithOutgoingHeaderMatcher(func(h string) (string, bool) {
			switch h {
			case "x-request-id", "etag", "location", "retry-after":
				return h, true
			}
			return "", false
		}),
		runtime.WithErrorHandler(problemJSON),
		runtime.WithMetadata(func(_ context.Context, r *http.Request) metadata.MD {
			if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
				return nil
			}
			ders := make([]string, len(r.TLS.PeerCertificates))
			for i, c := range r.TLS.PeerCertificates {
				ders[i] = base64.StdEncoding.EncodeToString(c.Raw)
			}
			return metadata.Pairs(peerChainMD, strings.Join(ders, ","))
		}),
		runtime.WithForwardResponseOption(httpCode),
		runtime.WithMarshalerOption(runtime.MIMEWildcard, &runtime.JSONPb{
			MarshalOptions:   protojson.MarshalOptions{UseProtoNames: true},
			UnmarshalOptions: protojson.UnmarshalOptions{DiscardUnknown: false},
		}),
	)
	if err := vehov1.RegisterJobServiceHandlerClient(context.Background(), mux, vehov1.NewJobServiceClient(conn)); err != nil {
		return nil, fmt.Errorf("api: gateway: %w", err)
	}
	if err := vehov1.RegisterPolicyServiceHandlerClient(context.Background(), mux, vehov1.NewPolicyServiceClient(conn)); err != nil {
		return nil, fmt.Errorf("api: gateway: %w", err)
	}
	s.gateway = mux
	s.health.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	return s, nil
}

// deadline gives a unary call without one the default (§2).
func (s *Server) deadline(ctx context.Context, req any, _ *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultCall)
		defer cancel()
	}
	return h(ctx, req)
}

// httpCode applies the status a handler chose (202 for an accepted job).
func httpCode(ctx context.Context, w http.ResponseWriter, _ proto.Message) error {
	md, ok := runtime.ServerMetadataFromContext(ctx)
	if !ok {
		return nil
	}
	if v := md.HeaderMD.Get("x-http-code"); len(v) > 0 {
		md.HeaderMD.Delete("x-http-code")
		w.Header().Del("Grpc-Metadata-X-Http-Code")
		if code, err := strconv.Atoi(v[0]); err == nil {
			w.WriteHeader(code)
		}
	}
	return nil
}

// Handler serves gRPC, the health endpoints and REST on one port (§2).
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.ProtoMajor == 2 && strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc"):
			s.grpc.ServeHTTP(w, r)
		case r.URL.Path == "/healthz":
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/readyz":
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			defer cancel()
			if err := s.ready(ctx); err != nil {
				http.Error(w, "not ready", http.StatusServiceUnavailable)
				return
			}
			w.WriteHeader(http.StatusOK)
		default:
			if r.ContentLength > maxBody {
				http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, maxBody)
			s.gateway.ServeHTTP(w, r)
		}
	})
}

// Serve serves on ln with TLS until ctx ends.
func (s *Server) Serve(ctx context.Context, ln net.Listener, cert tls.Certificate) error {
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       5 * time.Minute,
		// a client certificate is asked for, not required, and verified as
		// an X.509-SVID by the interceptors (§4.4)
		TLSConfig: &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}, NextProtos: []string{"h2", "http/1.1"},
			ClientAuth: tls.RequestClientCert},
		ErrorLog: slog.NewLogLogger(slog.Default().Handler(), slog.LevelWarn),
	}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		s.health.Shutdown()
		_ = srv.Shutdown(sctx)
	}()
	if err := srv.ServeTLS(ln, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("api: %w", err)
	}
	return nil
}

// Close stops the gRPC server and the gateway's connection.
func (s *Server) Close() {
	_ = s.client.Close()
	s.grpc.Stop()
}
