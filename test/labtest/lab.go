package labtest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	vehov1 "github.com/dihedron/veho/api/veho/v1"
	"github.com/dihedron/veho/pkg/plugin"
	s3driver "github.com/dihedron/veho/pkg/plugin/s3"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	grpccreds "google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"
)

// Regions are the lab's regions.
var Regions = []string{"a", "b", "c"}

// Buckets are the buckets of each region's Garage.
var Buckets = []string{"source", "dest-1", "dest-2", "dest-3"}

// Lab drives lab L3 (.specs/veho-lab-l3.md §2.3) from a test: the nodes, the
// state store, the WAN and the fault proxies.
type Lab struct {
	t     *testing.T
	nodes map[string]string // name to host:port
	ca    *x509.CertPool
	veho  string
	db    *pgxpool.Pool
	start time.Time
	jobs  []string
}

// New returns the lab, or skips the test when its nodes are not deployed. At
// the end of a failed test it saves the evidence in test/lab/.evidence.
func New(t *testing.T) *Lab {
	t.Helper()
	nodes := os.Getenv("VEHO_TEST_LAB_NODES")
	if nodes == "" {
		t.Skip("the lab's nodes are not deployed: run with test/lab/lab.sh acceptance")
	}
	l := &Lab{t: t, nodes: map[string]string{}, veho: os.Getenv("VEHO_TEST_LAB_VEHO"), start: time.Now()}
	for _, kv := range strings.Fields(nodes) {
		name, addr, _ := strings.Cut(kv, "=")
		l.nodes[name] = addr
	}
	pem, err := os.ReadFile(os.Getenv("VEHO_TEST_LAB_CA"))
	if err != nil {
		t.Fatalf("lab CA: %v", err)
	}
	l.ca = x509.NewCertPool()
	l.ca.AppendCertsFromPEM(pem)
	l.db, err = pgxpool.New(context.Background(), os.Getenv("VEHO_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatalf("database: %v", err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			l.evidence()
		}
		l.db.Close()
	})
	return l
}

// Nodes are the nodes' names, veho-a1 to veho-c2.
func (l *Lab) Nodes() []string {
	var names []string
	for _, x := range Regions {
		names = append(names, "veho-"+x+"1", "veho-"+x+"2")
	}
	return names
}

// StateDir is the lab's state directory (test/lab/.state).
func (l *Lab) StateDir() string { return filepath.Join(filepath.Dir(labSh()), ".state") }

// DB is the state store, as veho_migrator (no row-level security).
func (l *Lab) DB() *pgxpool.Pool { return l.db }

// Sh runs lab.sh with args and returns its standard output.
func (l *Lab) Sh(args ...string) string {
	l.t.Helper()
	cmd := exec.Command(labSh(), args...) //nolint:gosec // the lab's own script
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		l.t.Fatalf("lab.sh %s: %v: %s", strings.Join(args, " "), err, stderr.String())
	}
	return string(out)
}

// Kill sends SIGKILL to node's veho; with stayDown systemd does not restart it.
func (l *Lab) Kill(node string, stayDown bool) {
	l.t.Helper()
	args := []string{"node", "kill", node}
	if stayDown {
		args = append(args, "--stay-down")
		l.t.Cleanup(func() { l.Sh("node", "start", node) })
	}
	l.Sh(args...)
}

// Start, Stop, Pause and Resume act on node's veho service.
func (l *Lab) Start(node string) { l.t.Helper(); l.Sh("node", "start", node) }

// Stop stops node's veho cleanly; the test's end starts it again.
func (l *Lab) Stop(node string) {
	l.t.Helper()
	l.Sh("node", "stop", node)
	l.t.Cleanup(func() { l.Sh("node", "start", node) })
}

// Pause sends SIGSTOP; the test's end resumes it.
func (l *Lab) Pause(node string) {
	l.t.Helper()
	l.Sh("node", "pause", node)
	l.t.Cleanup(func() { l.Sh("node", "resume", node) })
}

// Resume sends SIGCONT.
func (l *Lab) Resume(node string) { l.t.Helper(); l.Sh("node", "resume", node) }

// KillDB force-stops db-x; the test's end starts it again.
func (l *Lab) KillDB(x string) {
	l.t.Helper()
	l.Sh("db", "kill", x)
	l.t.Cleanup(func() { l.Sh("db", "start", x) })
}

// Fault sets the fault proxy of region x ("status", "503", "--bucket",
// "dest-1"...); the test's end clears it.
func (l *Lab) Fault(x string, args ...string) {
	l.t.Helper()
	l.Sh(append([]string{"fault", x}, args...)...)
	l.t.Cleanup(func() { l.Sh("fault", x, "clear") })
}

// Partition cuts region x from the others; the test's end heals it.
func (l *Lab) Partition(x string) {
	l.t.Helper()
	l.Sh("wan", "partition", x)
	l.t.Cleanup(func() { l.Sh("wan", "heal") })
}

// Leader is the running Patroni leader, db-x, or "" while there is none or
// Patroni does not answer (during a failover).
func (l *Lab) Leader() string {
	out, err := exec.Command(labSh(), "patronictl", "list", "-f", "json").Output() //nolint:gosec // the lab's own script
	if err != nil {
		return ""
	}
	var ms []member
	_ = json.Unmarshal(out, &ms)
	for _, m := range ms {
		if m.Role == "Leader" && m.State == "running" {
			return m.Member
		}
	}
	return ""
}

// Metric sums the samples of a Prometheus metric on node.
func (l *Lab) Metric(node, name string) float64 {
	l.t.Helper()
	sum := 0.0
	for _, line := range strings.Split(l.Sh("ssh", node, "curl", "-fsS", "http://127.0.0.1:9464/metrics"), "\n") {
		if line == "" || line[0] == '#' {
			continue
		}
		metric, value, ok := strings.Cut(line, " ")
		if !ok || (metric != name && !strings.HasPrefix(metric, name+"{")) {
			continue
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err == nil {
			sum += v
		}
	}
	return sum
}

// Track records a job for the evidence of a failed test.
func (l *Lab) Track(id string) { l.jobs = append(l.jobs, id) }

// evidence saves the jobs, the nodes' journals, Patroni and the proxies'
// logs of the test's time window.
func (l *Lab) evidence() {
	dir := filepath.Join(filepath.Dir(labSh()), ".evidence", strings.ReplaceAll(l.t.Name(), "/", "_"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		l.t.Logf("evidence: %v", err)
		return
	}
	since := "@" + strconv.FormatInt(l.start.Unix(), 10)
	save := func(name string, args ...string) {
		out, _ := exec.Command(labSh(), args...).CombinedOutput() //nolint:gosec // the lab's own script
		_ = os.WriteFile(filepath.Join(dir, name), out, 0o600)
	}
	for _, n := range l.Nodes() {
		save(n+".log", "ssh", n, "journalctl", "-u", "veho", "--since", since, "--no-pager")
	}
	for _, x := range Regions {
		save("faultproxy-"+x+".log", "ssh", "s3-"+x, "journalctl", "-u", "faultproxy", "--since", since, "--no-pager")
	}
	save("patroni.txt", "patronictl", "list")
	for _, id := range l.jobs {
		var b []byte
		_ = l.db.QueryRow(context.Background(), `select jsonb_build_object('job', to_jsonb(j), 'transfers',
			(select jsonb_agg(to_jsonb(t)) from veho.transfers t where t.job_id = j.id)) from veho.jobs j where j.id = $1`, id).Scan(&b)
		_ = os.WriteFile(filepath.Join(dir, "job-"+id+".json"), b, 0o600)
	}
	l.t.Logf("evidence saved in %s", dir)
}

// bearer is an API key as gRPC per-call credentials.
type bearer string

func (b bearer) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + string(b)}, nil
}

func (bearer) RequireTransportSecurity() bool { return true }

// Clients are a node's services, as one tenant's admin.
type Clients struct {
	Jobs      vehov1.JobServiceClient
	Policies  vehov1.PolicyServiceClient
	Endpoints vehov1.EndpointServiceClient
	Secrets   vehov1.SecretServiceClient
	Admin     vehov1.AdminServiceClient
}

// Tenant is a fresh tenant of one test, with its admin key, a secret per
// region and an endpoint per region and bucket: "<x>-<bucket>", through the
// region's fault proxy. Its objects are under Prefix.
type Tenant struct {
	lab    *Lab
	Name   string
	ID     string
	Prefix string
	key    string
}

// Tenant creates a tenant with veho bootstrap and registers its endpoints
// through veho-b1.
func (l *Lab) Tenant() *Tenant {
	l.t.Helper()
	name := "ac-" + uuid.New().String()[:8]
	tn := &Tenant{lab: l, Name: name, Prefix: name + "/"}
	// registered first, so it runs last: after the faults are cleared
	l.t.Cleanup(tn.purge)
	tn.ID = strings.TrimSpace(l.bootstrap("tenant", name))
	tn.key = strings.TrimSpace(l.bootstrap("apikey", "--tenant", name, "--role", "admin", "--name", "admin"))
	c := tn.Clients("veho-b1")
	ctx := context.Background()
	for _, x := range Regions {
		up := strings.ToUpper(x)
		secret, _ := json.Marshal(map[string]string{"access_key_id": os.Getenv("VEHO_TEST_S3_" + up + "_ACCESS_KEY"),
			"secret_access_key": os.Getenv("VEHO_TEST_S3_" + up + "_SECRET_KEY")})
		if _, err := c.Secrets.PutSecret(ctx, &vehov1.PutSecretRequest{Name: "s3-" + x, Value: secret}); err != nil {
			l.t.Fatalf("put secret s3-%s: %v", x, err)
		}
		for _, b := range Buckets {
			cfg, _ := structpb.NewStruct(map[string]any{"endpoint": fmt.Sprintf("http://10.60.%d.20:3910", RegionNum(x)),
				"region": "region-" + x, "bucket": b, "path_style": true})
			if _, err := c.Endpoints.CreateEndpoint(ctx, &vehov1.Endpoint{Name: x + "-" + b, Driver: "s3", Location: "region-" + x,
				Config: cfg, CredentialRef: "db://s3-" + x}); err != nil {
				l.t.Fatalf("create endpoint %s-%s: %v", x, b, err)
			}
		}
	}
	return tn
}

func (l *Lab) bootstrap(args ...string) string {
	l.t.Helper()
	cmd := exec.Command(l.veho, append([]string{"bootstrap"}, append(args, "--audit-sink", "stderr")...)...) //nolint:gosec // the lab's build
	cmd.Env = append(os.Environ(), "VEHO_MIGRATOR_DATABASE_URL="+os.Getenv("VEHO_TEST_DATABASE_URL"))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		l.t.Fatalf("veho bootstrap %v: %v: %s", args, err, stderr.String())
	}
	return string(out)
}

// RegionNum is a region's number: a is 1.
func RegionNum(x string) int { return int(x[0]-'a') + 1 }

// Clients connects to node with the tenant's key; closed at the test's end.
func (tn *Tenant) Clients(node string) Clients {
	tn.lab.t.Helper()
	addr, ok := tn.lab.nodes[node]
	if !ok {
		tn.lab.t.Fatalf("no node %s", node)
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(grpccreds.NewTLS(&tls.Config{RootCAs: tn.lab.ca, MinVersion: tls.VersionTLS13})),
		grpc.WithPerRPCCredentials(bearer(tn.key)))
	if err != nil {
		tn.lab.t.Fatalf("connect %s: %v", node, err)
	}
	tn.lab.t.Cleanup(func() { _ = conn.Close() })
	return Clients{Jobs: vehov1.NewJobServiceClient(conn), Policies: vehov1.NewPolicyServiceClient(conn),
		Endpoints: vehov1.NewEndpointServiceClient(conn), Secrets: vehov1.NewSecretServiceClient(conn), Admin: vehov1.NewAdminServiceClient(conn)}
}

// Dest is a destination of a policy: endpoint "<x>-<bucket>".
type Dest struct{ Region, Bucket string }

// Policy is an inline policy over dests, each at the tenant's prefix + path,
// with extra fields (min_copies, spread...).
func (tn *Tenant) Policy(path string, dests []Dest, extra map[string]any) *structpb.Struct {
	tn.lab.t.Helper()
	ds := make([]any, 0, len(dests))
	for _, d := range dests {
		ds = append(ds, map[string]any{"endpoint": d.Region + "-" + d.Bucket, "path": tn.Prefix + path})
	}
	m := map[string]any{"destinations": ds}
	for k, v := range extra {
		m[k] = v
	}
	st, err := structpb.NewStruct(m)
	if err != nil {
		tn.lab.t.Fatalf("policy: %v", err)
	}
	return st
}

// Submit creates a job from "<x>-source" at the tenant's prefix + path.
func (tn *Tenant) Submit(c Clients, x, path string, policy *structpb.Struct) *vehov1.Job {
	tn.lab.t.Helper()
	j, err := c.Jobs.CreateJob(context.Background(), &vehov1.CreateJobRequest{
		Source: &vehov1.ObjectRef{Endpoint: x + "-source", Path: tn.Prefix + path}, Policy: policy})
	if err != nil {
		tn.lab.t.Fatalf("create job: %v", err)
	}
	tn.lab.Track(j.GetId())
	return j
}

// Wait long-polls the job until it settles or max passes, over any node
// that answers.
func (tn *Tenant) Wait(c Clients, id string, within time.Duration) *vehov1.Job {
	tn.lab.t.Helper()
	deadline := time.Now().Add(within)
	var last *vehov1.Job
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		j, err := c.Jobs.GetJob(ctx, &vehov1.GetJobRequest{Id: id, Wait: 30})
		cancel()
		if err != nil {
			time.Sleep(2 * time.Second) // a node or the database is down: try again
			continue
		}
		last = j
		if j.GetSettledAt() != nil {
			return j
		}
	}
	b, _ := protojson.Marshal(last)
	tn.lab.t.Fatalf("job %s did not settle within %v: %s", id, within, b)
	return nil
}

// s3 returns the shipped S3 driver on region x's bucket, through Garage's
// published port (not the fault proxy).
func s3Client(t *testing.T, x, bucket string) plugin.Client {
	t.Helper()
	up := strings.ToUpper(x)
	cfg, _ := json.Marshal(map[string]any{"endpoint": os.Getenv("VEHO_TEST_S3_" + up + "_ENDPOINT"), "region": "region-" + x,
		"bucket": bucket, "path_style": true})
	c, err := s3driver.Driver{}.Open(context.Background(), cfg, plugin.Credentials{
		"access_key_id": os.Getenv("VEHO_TEST_S3_" + up + "_ACCESS_KEY"), "secret_access_key": os.Getenv("VEHO_TEST_S3_" + up + "_SECRET_KEY")})
	if err != nil {
		t.Fatalf("s3 %s/%s: %v", x, bucket, err)
	}
	return c
}

// stream is the pseudo-random content of a test object, from its seed.
type stream struct{ r *rand.ChaCha8 }

func (s stream) Read(b []byte) (int, error) {
	var w [8]byte
	for i := 0; i < len(b); i += 8 {
		binary.LittleEndian.PutUint64(w[:], s.r.Uint64())
		copy(b[i:], w[:])
	}
	return len(b), nil
}

// Put writes size pseudo-random bytes to region x's source at the tenant's
// prefix + path, in 64 MiB parts, and returns their SHA-256.
func (tn *Tenant) Put(x, path string, size int64) string { return tn.PutSeed(x, path, size, "") }

// PutSeed is Put with other content: the bytes come from path and seed.
func (tn *Tenant) PutSeed(x, path string, size int64, seed string) string {
	t := tn.lab.t
	t.Helper()
	c := s3Client(t, x, "source")
	defer func() { _ = c.Close() }()
	ctx := context.Background()
	ref := plugin.Ref{Key: tn.Prefix + path}
	up, err := c.Begin(ctx, ref, plugin.ObjectInfo{Size: size}, plugin.PutOptions{})
	if err != nil {
		t.Fatalf("put %s: %v", path, err)
	}
	h := sha256.New()
	src := io.TeeReader(io.LimitReader(stream{rand.NewChaCha8(sha256.Sum256([]byte(tn.Prefix + path + "\x00" + seed)))}, size), h)
	const part = 64 << 20
	var parts []plugin.PartResult
	for n := 0; int64(n)*part < size; n++ {
		length := min(part, size-int64(n)*part)
		p, err := up.WritePart(ctx, n, io.LimitReader(src, length), length)
		if err != nil {
			t.Fatalf("put %s part %d: %v", path, n, err)
		}
		parts = append(parts, p)
	}
	if _, err := up.Commit(ctx, parts); err != nil {
		t.Fatalf("put %s: commit: %v", path, err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// SHA256 reads an object of region x's bucket at the tenant's prefix + path
// and returns its SHA-256, or "" when there is none.
func (tn *Tenant) SHA256(x, bucket, path string) string {
	t := tn.lab.t
	t.Helper()
	c := s3Client(t, x, bucket)
	defer func() { _ = c.Close() }()
	ctx := context.Background()
	ref := plugin.Ref{Key: tn.Prefix + path}
	info, err := c.Stat(ctx, ref)
	if err != nil {
		return ""
	}
	r, err := c.ReadRange(ctx, ref, 0, info.Size, "")
	if err != nil {
		t.Fatalf("read %s/%s: %v", bucket, path, err)
	}
	defer func() { _ = r.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		t.Fatalf("read %s/%s: %v", bucket, path, err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// rawS3 is an SDK client of region x's Garage, through its published port.
func rawS3(x string) *s3.Client {
	up := strings.ToUpper(x)
	return s3.New(s3.Options{Region: "region-" + x, UsePathStyle: true, BaseEndpoint: aws.String(os.Getenv("VEHO_TEST_S3_" + up + "_ENDPOINT")),
		Credentials: credentials.NewStaticCredentialsProvider(os.Getenv("VEHO_TEST_S3_"+up+"_ACCESS_KEY"), os.Getenv("VEHO_TEST_S3_"+up+"_SECRET_KEY"), "")})
}

// purge deletes the tenant's objects and aborts its uploads in every bucket,
// so that the tests do not fill the lab's disk.
func (tn *Tenant) purge() {
	ctx := context.Background()
	for _, x := range Regions {
		c := rawS3(x)
		for _, b := range Buckets {
			objs, err := c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(b), Prefix: aws.String(tn.Prefix)})
			if err != nil {
				tn.lab.t.Logf("purge %s/%s: %v", x, b, err)
				continue
			}
			for _, o := range objs.Contents {
				if _, err := c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(b), Key: o.Key}); err != nil {
					tn.lab.t.Logf("purge %s/%s/%s: %v", x, b, aws.ToString(o.Key), err)
				}
			}
			ups, err := c.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{Bucket: aws.String(b), Prefix: aws.String(tn.Prefix)})
			if err != nil {
				continue
			}
			for _, u := range ups.Uploads {
				_, _ = c.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: aws.String(b), Key: u.Key, UploadId: u.UploadId})
			}
		}
	}
}

// Uploads counts the multipart uploads left in region x's bucket under the
// tenant's prefix.
func (tn *Tenant) Uploads(x, bucket string) int {
	t := tn.lab.t
	t.Helper()
	out, err := rawS3(x).ListMultipartUploads(context.Background(), &s3.ListMultipartUploadsInput{Bucket: aws.String(bucket), Prefix: aws.String(tn.Prefix)})
	if err != nil {
		t.Fatalf("list uploads %s/%s: %v", x, bucket, err)
	}
	return len(out.Uploads)
}
