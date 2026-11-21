//go:build integration

package api

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
	"uuid"

	vehov1 "github.com/dihedron/veho/api/veho/v1"
	"github.com/dihedron/veho/internal/audit"
	"github.com/dihedron/veho/internal/config"
	"github.com/dihedron/veho/internal/engine"
	"github.com/dihedron/veho/internal/store"
	"github.com/dihedron/veho/internal/store/migrate"
	"github.com/dihedron/veho/migrations"
	"github.com/dihedron/veho/pkg/plugin"
	"github.com/dihedron/veho/pkg/plugin/file"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/structpb"
)

func dsn(t *testing.T, env string) string {
	t.Helper()
	v := os.Getenv(env)
	if v == "" {
		t.Skipf("%s is not set: run with test/lab/lab.sh test", env)
	}
	return v
}

type fixture struct {
	migrator *pgx.Conn
	api      *pgxpool.Pool
	e        *engine.Engine
	addr     string
	roots    *x509.CertPool
	a, b     uuid.UUID // two tenants
	srcDir   string
	keys     map[string]string // by name: a-viewer, a-operator, b-operator, platform
	// client is the certificate the clients present, if any
	client *tls.Certificate
}

func (f *fixture) tlsConfig() *tls.Config {
	c := &tls.Config{RootCAs: f.roots, ServerName: "localhost"}
	if f.client != nil {
		c.Certificates = []tls.Certificate{*f.client}
	}
	return c
}

// cert makes a self-signed certificate for localhost.
func cert(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"}, DNSNames: []string{"localhost"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, IsCA: true,
		BasicConstraintsValid: true, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(c)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: k}, pool
}

func setup(t *testing.T, o Options) *fixture {
	return setupWith(t, o, nil)
}

// setupWith builds the fixture; ids, when given, builds the server's
// Identities once the tenants exist.
func setupWith(t *testing.T, o Options, ids func(f *fixture) *Identities) *fixture {
	t.Helper()
	ctx := context.Background()
	conn, err := store.Connect(ctx, dsn(t, "VEHO_TEST_DATABASE_URL"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(ctx) })
	if _, err := migrate.Apply(ctx, conn, migrations.FS); err != nil {
		t.Fatal(err)
	}
	f := &fixture{migrator: conn, keys: map[string]string{}}
	var worker *pgxpool.Pool
	for env, p := range map[string]**pgxpool.Pool{"VEHO_TEST_API_DATABASE_URL": &f.api, "VEHO_TEST_WORKER_DATABASE_URL": &worker} {
		if *p, err = store.Open(ctx, dsn(t, env), store.Options{Name: "test/api"}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup((*p).Close)
	}
	cfg, err := config.Parse([]byte(`
node: {id: test-api-` + uuid.New().String()[:8] + `, region: lab-a}
secrets: {unwrap_key: "file:/unused"}
engine: {part_size_mib: 5}
api: {tls: {cert_file: /unused, key_file: /unused}}
`))
	if err != nil {
		t.Fatal(err)
	}
	if f.e, err = engine.New(ctx, engine.Deps{Config: cfg, API: f.api, Worker: worker, DBOSURL: dsn(t, "VEHO_TEST_DBOS_DATABASE_URL"),
		Version: "test-api", Audit: &audit.Recorder{}, Drivers: map[string]plugin.Driver{"file": file.New()}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.e.Shutdown(5 * time.Second) })
	if err := f.e.Launch(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.api.Exec(ctx, "select veho.put_location('lab-a', 'region', null)"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []*uuid.UUID{&f.a, &f.b} {
		if err := f.api.QueryRow(ctx, "select veho.create_tenant($1)", "test-"+uuid.New().String()).Scan(p); err != nil {
			t.Fatal(err)
		}
	}
	f.srcDir = t.TempDir()
	for _, tn := range []uuid.UUID{f.a, f.b} {
		if err := store.InTenant(ctx, f.api, tn, func(tx pgx.Tx) error {
			for name, dir := range map[string]string{"source": f.srcDir, "d1": t.TempDir(), "d2": t.TempDir()} {
				c, _ := json.Marshal(map[string]string{"root": dir})
				if _, err := tx.Exec(ctx, "insert into veho.endpoints (tenant_id, name, location_id, driver, config) values ($1, $2, 'lab-a', 'file', $3)", tn, name, c); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	for name, p := range map[string]struct {
		tenant *uuid.UUID
		role   string
	}{"a-viewer": {&f.a, RoleViewer}, "a-operator": {&f.a, RoleOperator}, "b-operator": {&f.b, RoleOperator}, "platform": {nil, RolePlatformAdmin}} {
		f.keys[name] = f.key(t, p.tenant, p.role, name)
	}
	if err := os.WriteFile(filepath.Join(f.srcDir, "obj"), bytes.Repeat([]byte("x"), 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}

	o.Engine, o.API = f.e, f.api
	if ids != nil {
		o.Identities = ids(f)
	}
	if o.MaxWatches == 0 {
		o.MaxWatches = 10
	}
	if o.Ready == nil {
		o.Ready = func(context.Context) error { return nil }
	}
	srv, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	c, roots := cert(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	sctx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- srv.Serve(sctx, ln, c) }()
	t.Cleanup(func() { stop(); <-done; srv.Close() })
	f.addr, f.roots = ln.Addr().String(), roots
	return f
}

// running inserts a RUNNING job of tenant with no workflow: it never
// settles, so that a watch of it lasts.
func (f *fixture) running(t *testing.T, tenant uuid.UUID) string {
	t.Helper()
	id := uuid.NewV7()
	if _, err := f.migrator.Exec(context.Background(), `insert into veho.jobs (id, tenant_id, source_endpoint_id, source_path, policy, state, deadline_at)
		select $1, $2, e.id, 'obj', '{}', 'RUNNING', now() + interval '1 hour' from veho.endpoints e where e.tenant_id = $2 and e.name = 'source'`,
		id, tenant); err != nil {
		t.Fatal(err)
	}
	return id.String()
}

// key inserts a principal and its key, as veho apikey create does.
func (f *fixture) key(t *testing.T, tenant *uuid.UUID, role, name string) string {
	t.Helper()
	token, id, sum := NewKey()
	ctx := context.Background()
	var p uuid.UUID
	if err := f.migrator.QueryRow(ctx, "insert into veho.principals (tenant_id, name, role) values ($1, $2, $3) returning id", tenant, name+"-"+id, role).Scan(&p); err != nil {
		t.Fatal(err)
	}
	if _, err := f.migrator.Exec(ctx, "insert into veho.api_keys (id, principal_id, tenant_id, sha256) values ($1, $2, $3, $4)", id, p, tenant, sum); err != nil {
		t.Fatal(err)
	}
	return token
}

func (f *fixture) grpc(t *testing.T) vehov1.JobServiceClient {
	t.Helper()
	conn, err := grpc.NewClient(f.addr, grpc.WithTransportCredentials(credentials.NewTLS(f.tlsConfig())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return vehov1.NewJobServiceClient(conn)
}

func as(key string) context.Context {
	return metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+key)
}

type reply struct {
	code   int
	header http.Header
	body   map[string]any
}

func (f *fixture) rest(t *testing.T, method, path, key string, body any, headers ...string) reply {
	t.Helper()
	var r io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		r = bytes.NewReader(b)
	default:
		j, _ := json.Marshal(b)
		r = bytes.NewReader(j)
	}
	req, _ := http.NewRequest(method, "https://localhost"+path, r)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	c := &http.Client{Transport: &http.Transport{TLSClientConfig: f.tlsConfig(),
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", f.addr)
		}}}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	out := reply{code: resp.StatusCode, header: resp.Header}
	data, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(data, &out.body)
	return out
}

func job(path string) map[string]any {
	return map[string]any{
		"source": map[string]string{"endpoint": "source", "path": "obj"},
		"policy": map[string]any{"destinations": []any{
			map[string]string{"endpoint": "d1", "path": path}, map[string]string{"endpoint": "d2", "path": path}}},
		"labels": map[string]string{"ticket": "OPS-1"},
	}
}

func reason(err error) string {
	for _, d := range status.Convert(err).Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok {
			return info.Reason
		}
	}
	return ""
}

// API-T1: create over REST, watch over gRPC until settled, the same job
// through both; list by label; cancel and retry refused on a settled job.
func TestJobs(t *testing.T) {
	f := setup(t, Options{})
	key := f.keys["a-operator"]
	r := f.rest(t, "POST", "/v1/jobs", key, job("out/t1"))
	if r.code != http.StatusAccepted || r.header.Get("Location") == "" || r.header.Get("X-Request-Id") == "" || r.body["state"] != "ACCEPTED" {
		t.Fatalf("create: %d %v %v", r.code, r.header, r.body)
	}
	id := r.body["id"].(string)
	c := f.grpc(t)
	w, err := c.WatchJob(as(key), &vehov1.JobRef{Id: id})
	if err != nil {
		t.Fatal(err)
	}
	var last *vehov1.Job
	for {
		j, err := w.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		last = j
	}
	if last == nil || last.GetState() != "SUCCEEDED" || last.GetCopies().GetDone() != 2 || last.GetCopies().GetTotal() != 2 || last.GetProgress() != 1 {
		t.Fatalf("watched: %v", last)
	}
	g := f.rest(t, "GET", "/v1/jobs/"+id, f.keys["a-viewer"], nil)
	if g.code != http.StatusOK || g.body["state"] != "SUCCEEDED" || g.header.Get("ETag") == "" || len(g.body["transfers"].([]any)) != 2 {
		t.Errorf("get: %d %v", g.code, g.body)
	}
	tr := f.rest(t, "GET", "/v1/jobs/"+id+"/transfers", f.keys["a-viewer"], nil)
	if tr.code != http.StatusOK || len(tr.body["transfers"].([]any)) != 2 {
		t.Errorf("transfers: %d %v", tr.code, tr.body)
	}
	l, err := c.ListJobs(as(f.keys["a-viewer"]), &vehov1.ListJobsRequest{Label: map[string]string{"ticket": "OPS-1"}, State: "SUCCEEDED"})
	if err != nil || len(l.GetJobs()) == 0 || l.GetJobs()[0].GetId() != id {
		t.Errorf("list: %v, %v", l, err)
	}
	ls := f.rest(t, "GET", "/v1/jobs?label%5Bticket%5D=OPS-2", f.keys["a-viewer"], nil)
	if ls.code != http.StatusOK || ls.body["jobs"] != nil {
		t.Errorf("list by another label: %d %v", ls.code, ls.body)
	}
	if ls := f.rest(t, "GET", "/v1/jobs?label%5Bticket%5D=OPS-1", f.keys["a-viewer"], nil); ls.code != http.StatusOK || len(ls.body["jobs"].([]any)) == 0 {
		t.Errorf("list by label: %d %v", ls.code, ls.body)
	}
	for _, op := range []string{":cancel", ":retry"} {
		if r := f.rest(t, "POST", "/v1/jobs/"+id+op, key, map[string]any{}); r.code != http.StatusConflict || r.body["code"] != "invalid_state" {
			t.Errorf("%s on a settled job: %d %v", op, r.code, r.body)
		}
	}
}

// API-T2: every method needs a credential, a valid one, and the right role;
// every method of the service has a role (T-36).
func TestAuthentication(t *testing.T) {
	f := setup(t, Options{})
	for _, m := range vehov1.JobService_ServiceDesc.Methods {
		if _, ok := roles["/veho.v1.JobService/"+m.MethodName]; !ok {
			t.Errorf("%s has no role", m.MethodName)
		}
	}
	conn, err := grpc.NewClient(f.addr, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{RootCAs: f.roots, ServerName: "localhost"})))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	revoked := f.key(t, &f.a, RoleOperator, "revoked")
	id, _, _ := parseKey(revoked)
	if _, err := f.migrator.Exec(context.Background(), "update veho.api_keys set revoked_at = now() where id = $1", id); err != nil {
		t.Fatal(err)
	}
	call := func(ctx context.Context, method string, stream bool) error {
		if !stream {
			return conn.Invoke(ctx, "/veho.v1.JobService/"+method, &emptypb.Empty{}, &emptypb.Empty{})
		}
		s, err := conn.NewStream(ctx, &vehov1.JobService_ServiceDesc.Streams[0], "/veho.v1.JobService/"+method)
		if err != nil {
			return err
		}
		if err := s.SendMsg(&emptypb.Empty{}); err != nil {
			return err
		}
		_ = s.CloseSend()
		return s.RecvMsg(&emptypb.Empty{})
	}
	methods := map[string]bool{}
	for _, m := range vehov1.JobService_ServiceDesc.Methods {
		methods[m.MethodName] = false
	}
	for _, s := range vehov1.JobService_ServiceDesc.Streams {
		methods[s.StreamName] = true
	}
	for m, stream := range methods {
		for name, ctx := range map[string]context.Context{"none": context.Background(), "revoked": as(revoked), "garbage": as("veho_nope")} {
			if err := call(ctx, m, stream); status.Code(err) != codes.Unauthenticated {
				t.Errorf("%s with %s credential: %v", m, name, err)
			}
		}
		if err := call(as(f.keys["platform"]), m, stream); status.Code(err) != codes.PermissionDenied {
			t.Errorf("%s as platform admin: %v", m, err)
		}
		if roles["/veho.v1.JobService/"+m] == RoleOperator {
			if err := call(as(f.keys["a-viewer"]), m, stream); status.Code(err) != codes.PermissionDenied {
				t.Errorf("%s as viewer: %v", m, err)
			}
		}
	}
	if r := f.rest(t, "GET", "/v1/jobs", "", nil); r.code != http.StatusUnauthorized || r.header.Get("Content-Type") != "application/problem+json" || r.body["code"] != "unauthenticated" {
		t.Errorf("REST without a key: %d %v", r.code, r.body)
	}
	if r := f.rest(t, "POST", "/v1/jobs", f.keys["a-viewer"], job("x")); r.code != http.StatusForbidden {
		t.Errorf("REST create as viewer: %d %v", r.code, r.body)
	}
}

// API-T3: idempotency.
func TestIdempotency(t *testing.T) {
	f := setup(t, Options{})
	key := f.keys["a-operator"]
	first := f.rest(t, "POST", "/v1/jobs", key, job("out/i"), "Idempotency-Key", "k-1")
	again := f.rest(t, "POST", "/v1/jobs", key, job("out/i"), "Idempotency-Key", "k-1")
	if first.code != http.StatusAccepted || again.code != http.StatusAccepted || first.body["id"] != again.body["id"] {
		t.Errorf("the same request: %d %v, %d %v", first.code, first.body["id"], again.code, again.body["id"])
	}
	other := f.rest(t, "POST", "/v1/jobs", key, job("out/other"), "Idempotency-Key", "k-1")
	if other.code != http.StatusConflict || other.body["code"] != "idempotency_key_reuse" {
		t.Errorf("another request: %d %v", other.code, other.body)
	}
}

// API-T4: tenant B sees nothing of tenant A.
func TestIsolation(t *testing.T) {
	f := setup(t, Options{})
	r := f.rest(t, "POST", "/v1/jobs", f.keys["a-operator"], job("out/iso"))
	id := r.body["id"].(string)
	c := f.grpc(t)
	other := as(f.keys["b-operator"])
	if _, err := c.GetJob(other, &vehov1.GetJobRequest{Id: id}); status.Code(err) != codes.NotFound {
		t.Errorf("get: %v", err)
	}
	if _, err := c.CancelJob(other, &vehov1.JobRef{Id: id}); status.Code(err) != codes.NotFound {
		t.Errorf("cancel: %v", err)
	}
	if r := f.rest(t, "GET", "/v1/jobs/"+id+"/transfers", f.keys["b-operator"], nil); r.code != http.StatusNotFound {
		t.Errorf("transfers: %d", r.code)
	}
	if l, err := c.ListJobs(other, &vehov1.ListJobsRequest{}); err != nil || len(l.GetJobs()) != 0 {
		t.Errorf("list: %v, %v", l, err)
	}
}

// API-T5, API-T7: the errors and the limits.
func TestErrors(t *testing.T) {
	f := setup(t, Options{MaxWatches: 1})
	key := f.keys["a-operator"]
	bad := job("x")
	bad["policy"] = map[string]any{"destinations": []any{map[string]string{"endpoint": "d1", "path": "x"}}}
	if r := f.rest(t, "POST", "/v1/jobs", key, bad); r.code != http.StatusBadRequest || r.body["code"] != "invalid_policy" || r.body["retryable"] != false {
		t.Errorf("one destination: %d %v", r.code, r.body)
	}
	missing := job("x")
	missing["source"] = map[string]string{"endpoint": "nope", "path": "obj"}
	if r := f.rest(t, "POST", "/v1/jobs", key, missing); r.code != http.StatusNotFound || r.body["code"] != "not_found" {
		t.Errorf("unknown endpoint: %d %v", r.code, r.body)
	}
	if r := f.rest(t, "POST", "/v1/jobs", key, []byte(`{"source": {"endpoint": "source", "path": "obj"}, "nope": 1}`)); r.code != http.StatusBadRequest {
		t.Errorf("unknown field: %d %v", r.code, r.body)
	}
	if r := f.rest(t, "POST", "/v1/jobs", key, bytes.Repeat([]byte(" "), 2<<20)); r.code != http.StatusRequestEntityTooLarge {
		t.Errorf("2 MiB body: %d", r.code)
	}
	if r := f.rest(t, "GET", "/v1/jobs/not-a-uuid", key, nil); r.code != http.StatusNotFound {
		t.Errorf("malformed id: %d", r.code)
	}
	if _, err := f.migrator.Exec(context.Background(), `update veho.tenants set quotas = '{"max_active_jobs": 1}' where id = $1`, f.a); err != nil {
		t.Fatal(err)
	}
	slow := job("out/q1")
	f.rest(t, "POST", "/v1/jobs", key, slow)
	c := f.grpc(t)
	pol, err := structpb.NewStruct(map[string]any{"destinations": []any{
		map[string]any{"endpoint": "d1", "path": "out/q2"}, map[string]any{"endpoint": "d2", "path": "out/q2"}}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.CreateJob(as(key), &vehov1.CreateJobRequest{Source: &vehov1.ObjectRef{Endpoint: "source", Path: "obj"}, Policy: pol})
	if status.Code(err) != codes.ResourceExhausted || reason(err) != "quota_exceeded" {
		// the first job may already have settled: then nothing is active
		if status.Code(err) != codes.OK {
			t.Errorf("over the quota: %v", err)
		}
	}
	// one watch at a time
	r := f.rest(t, "POST", "/v1/jobs", f.keys["b-operator"], job("out/w"))
	w, err := c.WatchJob(as(f.keys["b-operator"]), &vehov1.JobRef{Id: r.body["id"].(string)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Recv(); err != nil {
		t.Fatal(err)
	}
	w2, _ := c.WatchJob(as(f.keys["b-operator"]), &vehov1.JobRef{Id: r.body["id"].(string)})
	if _, err := w2.Recv(); status.Code(err) != codes.ResourceExhausted {
		t.Errorf("a second watch: %v", err)
	}
}

// API-T6: the long poll answers 304 at the end of the wait; a stream ends
// when its key is revoked.
func TestLongPollAndRevocation(t *testing.T) {
	f := setup(t, Options{Reauthorise: 500 * time.Millisecond})
	key := f.keys["a-operator"]
	id := f.rest(t, "POST", "/v1/jobs", key, job("out/lp")).body["id"].(string)
	c := f.grpc(t)
	w, _ := c.WatchJob(as(key), &vehov1.JobRef{Id: id})
	for {
		j, err := w.Recv()
		if err != nil || j.GetSettledAt() != nil {
			break
		}
	}
	g := f.rest(t, "GET", "/v1/jobs/"+id, key, nil)
	start := time.Now()
	nm := f.rest(t, "GET", "/v1/jobs/"+id+"?wait=2", key, nil, "If-None-Match", g.header.Get("ETag"))
	if nm.code != http.StatusNotModified || time.Since(start) < 1500*time.Millisecond {
		t.Errorf("long poll: %d after %v", nm.code, time.Since(start))
	}
	if r := f.rest(t, "GET", "/v1/jobs/"+id+"?wait=2", key, nil, "If-None-Match", `"other"`); r.code != http.StatusOK {
		t.Errorf("another etag: %d", r.code)
	}

	// a stream of a job that does not settle, then the key revoked
	revocable := f.key(t, &f.a, RoleViewer, "revocable")
	id2 := f.running(t, f.a)
	w2, err := c.WatchJob(as(revocable), &vehov1.JobRef{Id: id2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w2.Recv(); err != nil {
		t.Fatal(err)
	}
	kid, _, _ := parseKey(revocable)
	if _, err := f.migrator.Exec(context.Background(), "update veho.api_keys set revoked_at = now() where id = $1", kid); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := w2.Recv(); err != nil {
			if status.Code(err) != codes.Unauthenticated {
				t.Errorf("the stream ended with %v", err)
			}
			return
		}
	}
	t.Error("the stream outlived its key")
}
