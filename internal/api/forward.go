package api

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"
	"uuid"

	vehov1 "github.com/dihedron/veho/api/veho/v1"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

// Forwarding sends a far node's unary calls to the node nearest the primary,
// where their statements do not cross the WAN (veho-forwarding.md).

const (
	// forwardedMD marks a forwarded call, with the forwarder's node ID: it
	// is never forwarded again, and authenticates by its bearer token
	forwardedMD = "x-veho-forwarded"
	// forwardedForMD is the client's address, believed only from a peer
	// node (§2.4)
	forwardedForMD = "x-veho-forwarded-for"
	// a node nearer than this to the primary never forwards
	forwardMinRTT = 5 * time.Millisecond
)

var forwardedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "veho_api_forwarded_total",
	Help: "Unary API calls sent to the node nearest the state store, by outcome: forwarded, fallback (served here after a failure) or failed."},
	[]string{"outcome"})

// Register adds the API's metrics to reg.
func Register(reg prometheus.Registerer) error { return reg.Register(forwardedTotal) }

// Forward turns forwarding on (Options.Forward).
type Forward struct {
	// Node is this node's ID, its row of veho.executors.
	Node string
	// PeerCAs verify the other nodes' API certificates, as servers and as
	// clients.
	PeerCAs *x509.CertPool
	// Certificate is this node's API certificate, its client certificate
	// towards its peers.
	Certificate tls.Certificate
	// Every is how often the peers are read: the heartbeat.
	Every time.Duration
}

// executor is a row of veho.executors, as forwarding sees it.
type executor struct {
	id, url string
	rtt     time.Duration // negative when not measured
	live    bool
}

// choose is the peer to forward to (§2.1): the live peer with a URL that is
// nearest the primary, ties by ID, when self is over forwardMinRTT from it
// and the peer under a quarter of self's round trip.
func choose(self string, all []executor) (executor, bool) {
	var (
		me, best    executor
		found, mine bool
	)
	for _, e := range all {
		if e.id == self {
			me, mine = e, true
			continue
		}
		if !e.live || e.url == "" || e.rtt < 0 {
			continue
		}
		if !found || e.rtt < best.rtt || (e.rtt == best.rtt && e.id < best.id) {
			best, found = e, true
		}
	}
	if !mine || !found || me.rtt <= forwardMinRTT || best.rtt*4 >= me.rtt {
		return executor{}, false
	}
	return best, true
}

// forwarder keeps the peers and the connection to the chosen one.
type forwarder struct {
	o   Forward
	api *pgxpool.Pool

	mu     sync.Mutex
	target *grpc.ClientConn            // nil: serve here
	conns  map[string]*grpc.ClientConn // by URL
	hosts  []string                    // the live peers' hosts (§2.4)
}

// refresh reads the executors and chooses the target.
func (f *forwarder) refresh(ctx context.Context) error {
	rows, err := f.api.Query(ctx, `select id, coalesce(api_url, ''), coalesce(db_rtt_us, -1), last_seen > now() - make_interval(secs => $1)
		from veho.executors`, (3 * f.o.Every).Seconds())
	if err != nil {
		return fmt.Errorf("api: forwarding: %w", err)
	}
	var all []executor
	for rows.Next() {
		var (
			e  executor
			us int64
		)
		if err := rows.Scan(&e.id, &e.url, &us, &e.live); err != nil {
			rows.Close()
			return fmt.Errorf("api: forwarding: %w", err)
		}
		e.rtt = time.Duration(us) * time.Microsecond
		if us < 0 {
			e.rtt = -1
		}
		all = append(all, e)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("api: forwarding: %w", err)
	}
	t, ok := choose(f.o.Node, all)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hosts = f.hosts[:0]
	keep := map[string]bool{}
	for _, e := range all {
		if e.live && e.url != "" && e.id != f.o.Node {
			if u, err := url.Parse(e.url); err == nil {
				f.hosts = append(f.hosts, u.Hostname())
			}
			keep[e.url] = true
		}
	}
	for u, c := range f.conns {
		if !keep[u] {
			_ = c.Close()
			delete(f.conns, u)
		}
	}
	f.target = nil
	if !ok {
		return nil
	}
	c, err := f.dial(t.url)
	if err != nil {
		return err
	}
	c.Connect()
	f.target = c
	return nil
}

// dial is the connection to a peer's URL, kept open (f.mu held).
func (f *forwarder) dial(peerURL string) (*grpc.ClientConn, error) {
	if c := f.conns[peerURL]; c != nil {
		return c, nil
	}
	u, err := url.Parse(peerURL)
	if err != nil {
		return nil, fmt.Errorf("api: forwarding: %q: %w", peerURL, err)
	}
	c, err := grpc.NewClient(u.Host, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS13,
		RootCAs: f.o.PeerCAs, Certificates: []tls.Certificate{f.o.Certificate}, ServerName: u.Hostname()})),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(maxMessage)))
	if err != nil {
		return nil, fmt.Errorf("api: forwarding: %q: %w", peerURL, err)
	}
	if f.conns == nil {
		f.conns = map[string]*grpc.ClientConn{}
	}
	f.conns[peerURL] = c
	return c, nil
}

// run refreshes every f.o.Every until ctx ends.
func (f *forwarder) run(ctx context.Context) {
	t := time.NewTicker(f.o.Every)
	defer t.Stop()
	for {
		if err := f.refresh(ctx); err != nil && ctx.Err() == nil {
			slog.WarnContext(ctx, "cannot choose the node to forward to", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (f *forwarder) close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for u, c := range f.conns {
		_ = c.Close()
		delete(f.conns, u)
	}
	f.target = nil
}

func (f *forwarder) current() *grpc.ClientConn {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.target
}

// fromPeer is true when the call comes from a peer node: a client
// certificate of the nodes' CA, for client authentication, naming the host
// of a live peer (§2.4).
func (f *forwarder) fromPeer(ctx context.Context) bool {
	p, ok := peer.FromContext(ctx)
	if !ok || p.Addr == nil || p.Addr.Network() == "bufconn" {
		return false
	}
	info, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || len(info.State.PeerCertificates) == 0 {
		return false
	}
	leaf := info.State.PeerCertificates[0]
	inter := x509.NewCertPool()
	for _, c := range info.State.PeerCertificates[1:] {
		inter.AddCert(c)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: f.o.PeerCAs, Intermediates: inter, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, h := range f.hosts {
		if leaf.VerifyHostname(h) == nil {
			return true
		}
	}
	return false
}

// dropped are the metadata keys a forwarded call does not carry: the
// transport's, the in-process pipe's and the forwarding's own (§2.2).
func dropped(key string) bool {
	switch key {
	case ":authority", "content-type", "user-agent", "te", "x-forwarded-for", "x-forwarded-host", peerChainMD, forwardedMD, forwardedForMD:
		return true
	}
	return strings.HasPrefix(key, "grpc-") || strings.HasPrefix(key, ":")
}

// outgoing is the metadata of a forwarded call.
func outgoing(in metadata.MD) metadata.MD {
	out := metadata.MD{}
	for k, v := range in {
		if k = strings.ToLower(k); !dropped(k) {
			out[k] = append([]string(nil), v...)
		}
	}
	return out
}

// passBack keeps the receiver's headers and trailers, without the
// transport's.
func passBack(md metadata.MD) metadata.MD {
	out := metadata.MD{}
	for k, v := range md {
		if k != "content-type" && !strings.HasPrefix(k, "grpc-") {
			out[k] = v
		}
	}
	return out
}

// responseOf is an empty response of a method, for the forwarded answer.
func responseOf(method string) (proto.Message, error) {
	svc, name, ok := strings.Cut(strings.TrimPrefix(method, "/"), "/")
	if !ok {
		return nil, fmt.Errorf("api: forwarding: method %q", method)
	}
	d, err := protoregistry.GlobalFiles.FindDescriptorByName(protoreflect.FullName(svc))
	if err != nil {
		return nil, fmt.Errorf("api: forwarding: %w", err)
	}
	sd, ok := d.(protoreflect.ServiceDescriptor)
	if !ok || sd.Methods().ByName(protoreflect.Name(name)) == nil {
		return nil, fmt.Errorf("api: forwarding: method %q", method)
	}
	mt, err := protoregistry.GlobalTypes.FindMessageByName(sd.Methods().ByName(protoreflect.Name(name)).Output().FullName())
	if err != nil {
		return nil, fmt.Errorf("api: forwarding: %w", err)
	}
	return mt.New().Interface(), nil
}

// svidCaller is true when the call's credential is a client certificate,
// which cannot be relayed.
func (s *Server) svidCaller(ctx context.Context) bool {
	return s.ids != nil && s.ids.x509 != nil && len(peerChain(ctx)) > 0
}

// forward is the interceptor that forwards a unary call (§2.2, §2.3).
func (s *Server) forward(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
	if s.fw == nil || public[info.FullMethod] || first(ctx, forwardedMD) != "" || s.svidCaller(ctx) {
		return h(ctx, req)
	}
	conn := s.fw.current()
	if conn == nil {
		return h(ctx, req)
	}
	in, _ := metadata.FromIncomingContext(ctx)
	in = in.Copy()
	id := first(ctx, "x-request-id")
	if _, err := uuid.Parse(id); err != nil {
		id = uuid.New().String()
	}
	in.Set("x-request-id", id)
	// a local retry finds the job the peer may have made (§2.3)
	if r, ok := req.(*vehov1.CreateJobRequest); ok && r.GetIdempotencyKey() == "" && len(in.Get("idempotency-key")) == 0 {
		in.Set("idempotency-key", "fw-"+id)
	}
	ctx = metadata.NewIncomingContext(ctx, in)
	if conn.GetState() != connectivity.Ready {
		conn.Connect()
		forwardedTotal.WithLabelValues("fallback").Inc()
		return h(ctx, req)
	}
	resp, err := responseOf(info.FullMethod)
	if err != nil {
		return h(ctx, req)
	}
	out := outgoing(in)
	out.Set(forwardedMD, s.fw.o.Node)
	if addr := s.clientAddr(ctx); addr != "" {
		out.Set(forwardedForMD, addr)
	}
	var header, trailer metadata.MD
	err = conn.Invoke(metadata.NewOutgoingContext(ctx, out), info.FullMethod, req, resp, grpc.Header(&header), grpc.Trailer(&trailer))
	// the peer answered when it sent metadata (admit always sets
	// x-request-id; an error alone comes back in the trailers); anything
	// else may not have reached it
	if err == nil || header.Len() > 0 || trailer.Len() > 0 {
		_ = grpc.SetHeader(ctx, passBack(header))
		_ = grpc.SetTrailer(ctx, passBack(trailer))
		forwardedTotal.WithLabelValues("forwarded").Inc()
		return resp, err
	}
	if ctx.Err() != nil {
		return nil, err
	}
	slog.WarnContext(ctx, "cannot forward the call", "request_id", id, "method", info.FullMethod, "error", err)
	if _, ok := req.(*vehov1.CreateJobRequest); ok {
		forwardedTotal.WithLabelValues("fallback").Inc()
		return h(ctx, req)
	}
	forwardedTotal.WithLabelValues("failed").Inc()
	return nil, status.Error(codes.Unavailable, "the node nearest the state store cannot be reached: retry")
}

// forwardedFor is the client's address a peer node vouches for, if any.
func (s *Server) forwardedFor(ctx context.Context) (string, bool) {
	v := first(ctx, forwardedForMD)
	if v == "" || s.fw == nil || (net.ParseIP(v) == nil && v != "gateway") {
		return "", false
	}
	if !s.fw.fromPeer(ctx) {
		return "", false
	}
	return v, true
}
