//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/dihedron/veho/internal/audit"
	"github.com/dihedron/veho/internal/client"
	"github.com/dihedron/veho/internal/engine"
	"github.com/dihedron/veho/internal/secrets"
	"github.com/dihedron/veho/internal/store"
)

// node starts veho serve on the lab and returns its URL and CA file.
func node(t *testing.T) (string, string) {
	t.Helper()
	if os.Getenv("VEHO_TEST_DATABASE_URL") == "" {
		t.Skip("VEHO_TEST_DATABASE_URL is not set: run with test/lab/lab.sh test")
	}
	if err := (&MigrateCommand{dbOptions{DSNEnv: "VEHO_TEST_DATABASE_URL"}}).Execute(nil); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	conn, err := store.Connect(ctx, os.Getenv("VEHO_TEST_DATABASE_URL"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(ctx) })
	dir := t.TempDir()
	key := filepath.Join(dir, "unwrap.key")
	if err := os.WriteFile(key, bytes.Repeat([]byte("c"), 32), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"select pg_advisory_lock(hashtext('veho-test-kek'))",
		"update veho.endpoints set credential_ref = null where credential_ref like 'db://%'",
		"delete from veho.secrets", "delete from veho.kek",
		"select veho.put_location('lab-a', 'region', null)",
		// the nodes of earlier tests, and the lab's veho nodes that lab.sh
		// test stops, are gone though they beat moments ago: an endpoint
		// check waits for this node alone (tests run one at a time)
		"update veho.executors set last_seen = now() - interval '1 day'",
	} {
		if _, err := conn.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if err := secrets.InitKEK(ctx, conn, secrets.FileSource{Path: key}); err != nil {
		t.Fatal(err)
	}
	crt, keyFile := selfSigned(t, dir)
	cfg := filepath.Join(dir, "veho.yaml")
	if err := os.WriteFile(cfg, []byte(standaloneChecks()+`
node: {id: test-cli-`+uuid.New().String()[:8]+`, region: lab-a}
database:
  api: {dsn_env: VEHO_TEST_API_DATABASE_URL}
  worker: {dsn_env: VEHO_TEST_WORKER_DATABASE_URL}
  dbos: {dsn_env: VEHO_TEST_DBOS_DATABASE_URL}
secrets: {unwrap_key: "file:`+key+`"}
audit: {sink: stderr}
drivers: [{name: file, settings: {roots: ["`+filepath.Clean(os.TempDir())+`"]}}]
engine: {part_size_mib: 5}
metrics: {listen: "off"}
notifications: {enabled: true, allow_cidrs: [127.0.0.0/8]}
api: {listen: "127.0.0.1:18445", tls: {cert_file: "`+crt+`", key_file: "`+keyFile+`"}}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	ready := make(chan *engine.Engine, 1)
	rctx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- (&ServeCommand{Config: cfg, ready: ready}).run(rctx) }()
	t.Cleanup(func() { stop(); <-done })
	select {
	case <-ready:
	case err := <-done:
		done <- err // for the cleanup, which waits for serve to end
		t.Fatalf("serve stopped: %v", err)
	case <-time.After(60 * time.Second):
		t.Fatal("serve did not start")
	}
	return "https://localhost:18445", crt
}

// standaloneChecks relaxes STO-C3 on one PostgreSQL without a standby
// (test/ci/postgres.sh); the lab keeps every check.
func standaloneChecks() string {
	if os.Getenv("VEHO_TEST_STANDALONE") == "" {
		return ""
	}
	return "checks: {quorum: warn}\n"
}

// cli runs commands against the node, capturing their output.
type cli struct {
	t      *testing.T
	url    string
	ca     string
	api    string
	output bytes.Buffer
}

func (c *cli) opts(output string) apiOptions {
	c.output.Reset()
	return apiOptions{URL: c.url, CAFile: c.ca, API: c.api, Output: output, Timeout: 30 * time.Second, out: &c.output}
}

func (c *cli) run(cmd interface{ Execute([]string) error }) error {
	c.t.Helper()
	return cmd.Execute(nil)
}

func (c *cli) must(cmd interface{ Execute([]string) error }) string {
	c.t.Helper()
	if err := cmd.Execute(nil); err != nil {
		c.t.Fatalf("%T: %v", cmd, err)
	}
	return c.output.String()
}

func field(t *testing.T, out, name string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	s, _ := m[name].(string)
	return s
}

// CLI-T1 to CLI-T3, over gRPC and over REST.
func TestCLI(t *testing.T) {
	url, ca := node(t)
	tenant := "cli-" + uuid.New().String()[:8]
	bt := &BootstrapTenantCommand{dbOptions: dbOptions{DSNEnv: "VEHO_TEST_DATABASE_URL"}, auditOptions: auditOptions{auditor: &audit.Recorder{}}}
	bt.Args.Name = tenant
	_ = stdout(t, func() error { return bt.Execute(nil) })
	ba := &BootstrapAPIKeyCommand{dbOptions: dbOptions{DSNEnv: "VEHO_TEST_DATABASE_URL"}, auditOptions: auditOptions{auditor: &audit.Recorder{}},
		Tenant: tenant, Role: "admin", Name: "admin"}
	t.Setenv("VEHO_TOKEN", stdout(t, func() error { return ba.Execute(nil) }))

	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "obj"), bytes.Repeat([]byte("v"), 3<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	files := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(files, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for _, transport := range []string{"grpc", "rest"} {
		t.Run(transport, func(t *testing.T) {
			c := &cli{t: t, url: url, ca: ca, api: transport}
			s := transport                     // names of this pass
			d1, d2 := t.TempDir(), t.TempDir() // each pass its own destinations
			for name, root := range map[string]string{"src-" + s: src, "d1-" + s: d1, "d2-" + s: d2} {
				ep := &EndpointCreateCommand{apiOptions: c.opts("json"), Driver: "file",
					endpointFlags: endpointFlags{Location: "lab-a", ConfigFile: write(name+".json", `{"root": "`+root+`"}`)}}
				ep.Args.Name = name
				c.must(ep)
			}
			// SRC-T6: a root outside the file driver's roots is refused
			outside := &EndpointCreateCommand{apiOptions: c.opts("json"), Driver: "file",
				endpointFlags: endpointFlags{Location: "lab-a", ConfigFile: write("outside.json", `{"root": "/etc"}`)}}
			outside.Args.Name = "outside-" + s
			if err := c.run(outside); err == nil {
				t.Error("a file endpoint outside the roots was created")
			}
			test := &EndpointTestCommand{apiOptions: c.opts("text")}
			test.Args.Name = "d1-" + s
			if out := c.must(test); !strings.Contains(out, "ok: true") {
				t.Errorf("endpoint test: %s", out)
			}
			// NC-T4: a check from every executor, waited for; a file
			// endpoint has no flows, so the table is empty
			check := &EndpointCheckCommand{apiOptions: c.opts("text"), Wait: true, Within: time.Minute}
			check.Args.Name = "d1-" + s
			if out := c.must(check); !strings.Contains(out, "state: complete") || !strings.Contains(out, "NODE") {
				t.Errorf("endpoint check: %s", out)
			}
			checkJSON := &EndpointCheckCommand{apiOptions: c.opts("json")}
			checkJSON.Args.Name = "d1-" + s
			if out := c.must(checkJSON); field(t, out, "endpoint") != "d1-"+s || field(t, out, "id") == "" {
				t.Errorf("endpoint check, json: %s", out)
			}
			show := &EndpointShowCommand{apiOptions: c.opts("text")}
			show.Args.Name = "d1-" + s
			if out := c.must(show); !strings.Contains(out, "last_check: ") {
				t.Errorf("endpoint show: %s", out)
			}
			sec := &SecretSetCommand{apiOptions: c.opts("json"), FromFile: write("secret", "canary-value")}
			sec.Args.Name = "s-" + s
			if out := c.must(sec); strings.Contains(out, "canary") || field(t, out, "name") != "s-"+s {
				t.Errorf("secret set: %s", out)
			}
			// webhooks: the secret is shown once; a ping to a receiver whose
			// certificate veho does not trust fails, and exits 1
			hook := &WebhookCreateCommand{apiOptions: c.opts("json"), URL: "https://localhost:1/veho", Events: []string{"job.succeeded", "job.failed"}}
			hook.Args.Name = "hook-" + s
			out := c.must(hook)
			var created struct {
				Secret  string
				Webhook map[string]any
			}
			if err := json.Unmarshal([]byte(out), &created); err != nil || !strings.HasPrefix(created.Secret, "whsec_") || created.Webhook["name"] != "hook-"+s {
				t.Errorf("webhook create: %v %s", err, out)
			}
			hl := &WebhookListCommand{apiOptions: c.opts("text")}
			if out := c.must(hl); !strings.Contains(out, "hook-"+s) || strings.Contains(out, created.Secret) {
				t.Errorf("webhook list: %s", out)
			}
			hu := &WebhookUpdateCommand{apiOptions: c.opts("json"), Events: []string{"job.cancelled"}, Disable: true}
			hu.Args.Name = "hook-" + s
			if out := c.must(hu); !strings.Contains(out, "job.cancelled") || strings.Contains(out, `"enabled": true`) {
				t.Errorf("webhook update: %s", out)
			}
			hu = &WebhookUpdateCommand{apiOptions: c.opts("json"), Enable: true, Disable: true}
			hu.Args.Name = "hook-" + s
			if err := c.run(hu); err == nil {
				t.Error("--enable with --disable was accepted")
			}
			hr := &WebhookRotateCommand{apiOptions: c.opts("json")}
			hr.Args.Name = "hook-" + s
			if out := c.must(hr); field(t, out, "secret") == created.Secret || !strings.HasPrefix(field(t, out, "secret"), "whsec_") {
				t.Errorf("webhook rotate: %s", out)
			}
			ht := &WebhookTestCommand{apiOptions: c.opts("json")}
			ht.Args.Name = "hook-" + s
			if err := c.run(ht); exitCode(err) != 1 {
				t.Errorf("webhook test to a closed port: %v, exit %d", err, exitCode(err))
			}
			hd := &WebhookDeleteCommand{apiOptions: c.opts("json")}
			hd.Args.Name = "hook-" + s
			c.must(hd)
			pol := write("geo.json", `{"destinations": [{"endpoint": "d1-`+s+`", "path": "ref/obj"}, {"endpoint": "d2-`+s+`", "path": "ref/obj"}]}`)
			pc := &PolicyCreateCommand{apiOptions: c.opts("json"), File: pol}
			pc.Args.Name = "geo-" + s
			c.must(pc)
			pg := &PolicyGrantCommand{apiOptions: c.opts("text")}
			pg.Args.Policy, pg.Args.Grantee, pg.Args.Permission = "geo-"+s, "group:ops", "submit"
			if out := c.must(pg); !strings.Contains(out, "group:ops") {
				t.Errorf("policy grant: %s", out)
			}
			if out := c.must(&PolicyListCommand{apiOptions: c.opts("text")}); !strings.Contains(out, "geo-"+s) {
				t.Errorf("policy list: %s", out)
			}

			// a job by reference, and one inline; both settle
			for _, sub := range []*JobSubmitCommand{
				{apiOptions: c.opts("json"), Source: "src-" + s + ":obj", Policy: "geo-" + s, Labels: []string{"run=" + s}},
				{apiOptions: c.opts("json"), Source: "src-" + s + ":obj", Labels: []string{"run=" + s},
					PolicyFile: write("inline.json", `{"destinations": [{"endpoint": "d1-`+s+`", "path": "in/obj"}, {"endpoint": "d2-`+s+`", "path": "in/obj"}]}`)},
			} {
				sub.apiOptions = c.opts("json")
				id := field(t, c.must(sub), "id")
				w := &JobWaitCommand{apiOptions: c.opts("json")}
				w.Args.ID = id
				if err := c.run(w); err != nil {
					t.Fatalf("job wait: %v (%s)", err, c.output.String())
				}
				cancel := &JobCancelCommand{apiOptions: c.opts("json")}
				cancel.Args.ID = id
				if err := c.run(cancel); !isCode(err, "invalid_state") {
					t.Errorf("cancel a settled job: %v", err)
				}
			}
			list := &JobListCommand{apiOptions: c.opts("json"), Labels: []string{"run=" + s}}
			var jobs struct{ Jobs []map[string]any }
			if err := json.Unmarshal([]byte(c.must(list)), &jobs); err != nil || len(jobs.Jobs) != 2 {
				t.Errorf("job list: %v %s", err, c.output.String())
			}

			// CLI-T2: a job that fails exits 3
			fails := &JobSubmitCommand{apiOptions: c.opts("json"), Source: "src-" + s + ":missing", Policy: "geo-" + s}
			id := field(t, c.must(fails), "id")
			w := &JobWaitCommand{apiOptions: c.opts("json")}
			w.Args.ID = id
			if err := c.run(w); exitCode(err) != 3 {
				t.Errorf("job wait on a failed job: %v, exit %d", err, exitCode(err))
			}
		})
	}

	// CLI-T3: a wrong credential, and none
	c := &cli{t: t, url: url, ca: ca, api: "rest"}
	t.Setenv("VEHO_TOKEN", "veho_wrongwrongwr_AAAA")
	if err := c.run(&JobListCommand{apiOptions: c.opts("text")}); !isCode(err, "unauthenticated") || exitCode(err) != 1 {
		t.Errorf("a wrong key: %v", err)
	}
	t.Setenv("VEHO_TOKEN", "")
	if err := c.run(&JobListCommand{apiOptions: c.opts("text")}); err == nil || !strings.Contains(err.Error(), "no credential") {
		t.Errorf("no credential: %v", err)
	}
}

func isCode(err error, code string) bool {
	e, ok := errors.AsType[*client.Error](err)
	return ok && e.Code == code
}
