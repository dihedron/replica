//go:build integration

package api

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
	"uuid"

	vehov1 "github.com/dihedron/veho/api/veho/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
)

// nodeCA is a CA of the nodes' API certificates; issue makes one for hosts,
// for server authentication, and client authentication when client.
func nodeCA(t *testing.T) (*x509.CertPool, func(cn string, client bool, hosts ...string) tls.Certificate) {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "nodes CA"}, IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	serial := int64(1)
	return pool, func(cn string, client bool, hosts ...string) tls.Certificate {
		lk, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		serial++
		eku := []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		if client {
			eku = append(eku, x509.ExtKeyUsageClientAuth)
		}
		leaf := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: cn}, DNSNames: hosts,
			NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature,
			ExtKeyUsage: eku}
		d, err := x509.CreateCertificate(rand.Reader, leaf, ca, &lk.PublicKey, k)
		if err != nil {
			t.Fatal(err)
		}
		return tls.Certificate{Certificate: [][]byte{d}, PrivateKey: lk}
	}
}

// forwarding is two servers on one database: a, far (50 ms), and b, near
// (0.3 ms), each a forwarder; fa and fb are the fixture as their clients see
// them.
type forwarding struct {
	f, fa, fb      *fixture
	a, b           *Server
	stopB          func()
	pool           *x509.CertPool
	issue          func(cn string, client bool, hosts ...string) tls.Certificate
	nodeA, nodeB   string
	peerHost, addr string
}

func setupForwarding(t *testing.T) *forwarding {
	t.Helper()
	f := setup(t, Options{})
	w := &forwarding{f: f, nodeA: "fw-a-" + uuid.New().String()[:8], nodeB: "fw-b-" + uuid.New().String()[:8]}
	w.pool, w.issue = nodeCA(t)
	cb, ca := w.issue("b", true, "localhost"), w.issue("a", true, "localhost")
	var addrB, addrA string
	w.b, addrB, w.stopB = f.start(t, Options{Forward: &Forward{Node: w.nodeB, PeerCAs: w.pool, Certificate: cb, Every: 100 * time.Millisecond}}, cb)
	w.a, addrA, _ = f.start(t, Options{Forward: &Forward{Node: w.nodeA, PeerCAs: w.pool, Certificate: ca, Every: 100 * time.Millisecond}}, ca)
	_, portA, _ := net.SplitHostPort(addrA)
	_, portB, _ := net.SplitHostPort(addrB)
	ctx := context.Background()
	// last seen in the future: live for the whole test
	for _, r := range []struct {
		id, url string
		rtt     int
	}{{w.nodeA, "https://localhost:" + portA, 50000}, {w.nodeB, "https://localhost:" + portB, 300}} {
		if _, err := f.migrator.Exec(ctx, `insert into veho.executors (id, region, app_version, api_url, db_rtt_us, last_seen)
			values ($1, 'lab-a', 'test', $2, $3, now() + interval '1 hour')`, r.id, r.url, r.rtt); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_, _ = f.migrator.Exec(context.Background(), "delete from veho.executors where id in ($1, $2)", w.nodeA, w.nodeB)
	})
	fa, fb := *f, *f
	fa.addr, fa.roots, fb.addr, fb.roots = addrA, w.pool, addrB, w.pool
	w.fa, w.fb = &fa, &fb
	// a has chosen b, and b knows a for a peer (§2.4)
	knows := func() bool {
		w.b.fw.mu.Lock()
		defer w.b.fw.mu.Unlock()
		return len(w.b.fw.hosts) > 0
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if c := w.a.fw.current(); c != nil && c.GetState() == connectivity.Ready && knows() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a never chose b")
		}
	}
	if w.b.fw.current() != nil {
		t.Fatal("b, the nearest, forwards")
	}
	return w
}

// FW-T2, FW-T3: calls through the far node run on the near one, and answer
// as it does; a forwarded call is not forwarded again.
func TestForwarding(t *testing.T) {
	w := setupForwarding(t)
	key := w.f.keys["a-operator"]
	before := count("forwarded")
	r := w.fa.rest(t, "POST", "/v1/jobs", key, job("out/fw"), "X-Request-Id", "0198c9a5-0000-7000-8000-000000000001")
	if r.code != http.StatusAccepted || r.header.Get("Location") == "" || r.header.Get("X-Request-Id") != "0198c9a5-0000-7000-8000-000000000001" || r.body["state"] != "ACCEPTED" {
		t.Fatalf("create through a: %d %v %v", r.code, r.header, r.body)
	}
	if got := count("forwarded") - before; got != 1 {
		t.Errorf("%v calls forwarded, want 1", got)
	}
	id := r.body["id"].(string)
	// the forwarder's key: a local retry with it finds the job (FW-4)
	again := w.fb.rest(t, "POST", "/v1/jobs", key, job("out/fw"), "Idempotency-Key", "fw-0198c9a5-0000-7000-8000-000000000001")
	if again.body["id"] != id {
		t.Errorf("the retry with the forwarder's key: %d %v, want job %s", again.code, again.body, id)
	}
	g := w.fa.rest(t, "GET", "/v1/jobs/"+id, w.f.keys["a-viewer"], nil)
	if g.code != http.StatusOK || g.header.Get("ETag") == "" || g.body["id"] != id {
		t.Errorf("get through a: %d %v %v", g.code, g.header, g.body)
	}
	l, err := w.fa.grpc(t).ListJobs(as(w.f.keys["a-viewer"]), &vehov1.ListJobsRequest{})
	if err != nil || len(l.GetJobs()) == 0 {
		t.Errorf("list through a: %v, %v", l, err)
	}
	// errors come back whole: the status, its ErrorInfo, the problem
	pol, _ := structpb.NewStruct(map[string]any{"destinations": []any{map[string]any{"endpoint": "d1", "path": "x"}}})
	_, errA := w.fa.grpc(t).CreateJob(as(w.f.keys["a-viewer"]), &vehov1.CreateJobRequest{Source: &vehov1.ObjectRef{Endpoint: "source", Path: "obj"}, Policy: pol})
	_, errB := w.fb.grpc(t).CreateJob(as(w.f.keys["a-viewer"]), &vehov1.CreateJobRequest{Source: &vehov1.ObjectRef{Endpoint: "source", Path: "obj"}, Policy: pol})
	if status.Code(errA) != codes.PermissionDenied || reason(errA) != reason(errB) || reason(errA) == "" || status.Convert(errA).Message() != status.Convert(errB).Message() {
		t.Errorf("a viewer's create: through a %v (%s), directly %v (%s)", errA, reason(errA), errB, reason(errB))
	}
	if p := w.fa.rest(t, "POST", "/v1/jobs", w.f.keys["a-viewer"], job("out/x")); p.code != http.StatusForbidden || p.body["type"] == nil {
		t.Errorf("a viewer's create over REST through a: %d %v", p.code, p.body)
	}
	// FW-T3: marked, a call is served where it lands
	before = count("forwarded")
	ctx := metadata.AppendToOutgoingContext(as(key), forwardedMD, "elsewhere")
	if _, err := w.fa.grpc(t).ListJobs(ctx, &vehov1.ListJobsRequest{}); err != nil {
		t.Fatal(err)
	}
	if got := count("forwarded") - before; got != 0 {
		t.Errorf("a marked call was forwarded (%v)", got)
	}
}

// FW-T4: with b down, a serves the calls itself.
func TestForwardingPeerDown(t *testing.T) {
	w := setupForwarding(t)
	w.stopB()
	for deadline := time.Now().Add(10 * time.Second); w.a.fw.current().GetState() == connectivity.Ready; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the connection to b stays ready")
		}
	}
	before := count("fallback")
	r := w.fa.rest(t, "POST", "/v1/jobs", w.f.keys["a-operator"], job("out/down"))
	if r.code != http.StatusAccepted {
		t.Fatalf("create with b down: %d %v", r.code, r.body)
	}
	if count("fallback") <= before {
		t.Error("no fallback counted")
	}
}

// FW-T6: x-veho-forwarded-for is believed from a peer node's certificate
// only.
func TestForwardedFor(t *testing.T) {
	w := setupForwarding(t)
	_, otherIssue := nodeCA(t)
	for name, c := range map[string]struct {
		cert *tls.Certificate
		want string
	}{
		"a peer":           {ptr(w.issue("a", true, "localhost")), "203.0.113.9"},
		"no certificate":   {nil, "127.0.0.1"},
		"another CA":       {ptr(otherIssue("a", true, "localhost")), "127.0.0.1"},
		"not a live peer":  {ptr(w.issue("x", true, "other.example")), "127.0.0.1"},
		"server auth only": {ptr(w.issue("a", false, "localhost")), "127.0.0.1"},
	} {
		t.Run(name, func(t *testing.T) {
			fb := *w.fb
			fb.client = c.cert
			ctx := metadata.AppendToOutgoingContext(as("veho_bogus_"+strings.Repeat("x", 40)), forwardedForMD, "203.0.113.9", forwardedMD, "a")
			_, _ = fb.grpc(t).ListJobs(ctx, &vehov1.ListJobsRequest{})
			w.b.refused.mu.Lock()
			defer w.b.refused.mu.Unlock()
			found := false
			for k := range w.b.refused.counts {
				found = found || k.address == c.want
				if k.address != c.want {
					t.Errorf("a refusal from %q, want %q", k.address, c.want)
				}
			}
			if !found {
				t.Errorf("no refusal from %q: %v", c.want, w.b.refused.counts)
			}
			w.b.refused.counts = nil
		})
	}
}

func ptr[T any](v T) *T { return &v }
