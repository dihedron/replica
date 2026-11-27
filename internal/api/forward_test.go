package api

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"strings"
	"testing"
	"time"

	vehov1 "github.com/dihedron/veho/api/veho/v1"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
)

// FW-T1: the peer to forward to.
func TestChoose(t *testing.T) {
	ms := func(f float64) time.Duration { return time.Duration(f * float64(time.Millisecond)) }
	near := executor{id: "b1", url: "https://b1:8443", rtt: ms(0.3), live: true}
	for name, c := range map[string]struct {
		all  []executor
		want string
	}{
		"far: to the nearest":            {[]executor{{id: "a1", rtt: ms(50), live: true}, near, {id: "c1", url: "https://c1:8443", rtt: ms(25), live: true}}, "b1"},
		"near itself":                    {[]executor{{id: "a1", rtt: ms(0.4), live: true}, near}, ""},
		"under 5 ms":                     {[]executor{{id: "a1", rtt: ms(4), live: true}, near}, ""},
		"not a quarter":                  {[]executor{{id: "a1", rtt: ms(40), live: true}, {id: "c1", url: "https://c1:8443", rtt: ms(10), live: true}}, ""},
		"dead peer":                      {[]executor{{id: "a1", rtt: ms(50), live: true}, {id: "b1", url: "https://b1:8443", rtt: ms(0.3)}}, ""},
		"no URL":                         {[]executor{{id: "a1", rtt: ms(50), live: true}, {id: "b1", rtt: ms(0.3), live: true}}, ""},
		"not measured":                   {[]executor{{id: "a1", rtt: ms(50), live: true}, {id: "b1", url: "https://b1:8443", rtt: -1, live: true}}, ""},
		"self not measured":              {[]executor{{id: "a1", rtt: -1, live: true}, near}, ""},
		"self missing":                   {[]executor{near}, ""},
		"tie by ID":                      {[]executor{{id: "a1", rtt: ms(50), live: true}, {id: "b2", url: "https://b2:8443", rtt: ms(0.3), live: true}, near}, "b1"},
		"self with a URL is no peer":     {[]executor{{id: "a1", url: "https://a1:8443", rtt: ms(50), live: true}}, ""},
		"dead self still decides":        {[]executor{{id: "a1", rtt: ms(50)}, near}, "b1"},
		"a far peer is still the target": {[]executor{{id: "a1", rtt: ms(100), live: true}, {id: "c1", url: "https://c1:8443", rtt: ms(20), live: true}}, "c1"},
	} {
		got, ok := choose("a1", c.all)
		if ok != (c.want != "") || got.id != c.want {
			t.Errorf("%s: %q %v, want %q", name, got.id, ok, c.want)
		}
	}
}

// FW-T8: a forwarded call carries no transport, pipe or forwarding key,
// whatever the client sent.
func FuzzOutgoing(f *testing.F) {
	for _, k := range []string{"authorization", "x-forwarded-for", "X-Veho-Forwarded-For", "grpc-timeout", ":authority", "x-veho-peer-chain", "Content-Type", "x-veho-tenant"} {
		f.Add(k, "v")
	}
	f.Fuzz(func(t *testing.T, key, value string) {
		in := metadata.MD{"authorization": {"Bearer x"}}
		in[key] = []string{value}
		out := outgoing(in)
		for k := range out {
			if dropped(k) || k != strings.ToLower(k) {
				t.Fatalf("%q kept from %q", k, key)
			}
		}
		if out.Get("authorization") == nil && strings.ToLower(key) != "authorization" {
			t.Fatal("the credential was dropped")
		}
	})
}

func TestResponseOf(t *testing.T) {
	m, err := responseOf("/veho.v1.JobService/CreateJob")
	if _, ok := m.(*vehov1.Job); err != nil || !ok {
		t.Errorf("CreateJob: %T, %v", m, err)
	}
	for _, bad := range []string{"/veho.v1.JobService/Nope", "/veho.v1.Nope/CreateJob", "nonsense"} {
		if _, err := responseOf(bad); err == nil {
			t.Errorf("%s: no error", bad)
		}
	}
}

// FW-T5, FW-T3: an X.509-SVID caller, a public method and a marked call are
// served where they land, without even a look at the peer.
func TestForwardKeepsLocal(t *testing.T) {
	conn, err := grpc.NewClient("passthrough:///nowhere", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	s := &Server{fw: &forwarder{target: conn}, ids: &Identities{x509: &reloading[*x509bundle.Bundle]{}}}
	svid := peer.NewContext(context.Background(), &peer.Peer{Addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)},
		AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{PeerCertificates: []*x509.Certificate{{}}}}})
	marked := metadata.NewIncomingContext(context.Background(), metadata.Pairs(forwardedMD, "elsewhere"))
	for name, c := range map[string]struct {
		ctx    context.Context
		method string
	}{
		"an X.509-SVID": {svid, "/veho.v1.JobService/ListJobs"},
		"public":        {context.Background(), "/veho.v1.AuthService/GetConfig"},
		"marked":        {marked, "/veho.v1.JobService/ListJobs"},
	} {
		before := count("fallback") + count("forwarded") + count("failed")
		called := false
		_, _ = s.forward(c.ctx, &vehov1.ListJobsRequest{}, &grpc.UnaryServerInfo{FullMethod: c.method}, func(context.Context, any) (any, error) {
			called = true
			return nil, nil
		})
		if !called || count("fallback")+count("forwarded")+count("failed") != before {
			t.Errorf("%s: not served here (called %v)", name, called)
		}
	}
}

func count(outcome string) float64 { return testutil.ToFloat64(forwardedTotal.WithLabelValues(outcome)) }
