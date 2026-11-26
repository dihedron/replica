package client

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	vehov1 "github.com/dihedron/veho/api/veho/v1"
	"google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// CLI-T4: for every method, the path's variables come from the request and
// the rest goes in the body or the query, as the annotation says.
func TestRequestEveryMethod(t *testing.T) {
	n := 0
	for _, fd := range []protoreflect.FileDescriptor{vehov1.File_veho_v1_jobs_proto, vehov1.File_veho_v1_policies_proto, vehov1.File_veho_v1_admin_proto} {
		for i := range fd.Services().Len() {
			sd := fd.Services().Get(i)
			for j := range sd.Methods().Len() {
				m := sd.Methods().Get(j)
				req := dynamicpb.NewMessage(m.Input())
				// every string field set: path variables must be replaced
				for k := range m.Input().Fields().Len() {
					f := m.Input().Fields().Get(k)
					if f.Kind() == protoreflect.StringKind && !f.IsList() && !f.IsMap() {
						req.Set(f, protoreflect.ValueOfString("v-"+string(f.Name())))
					}
				}
				method, path, query, body, err := Request(m, req)
				if err != nil {
					t.Errorf("%s: %v", m.FullName(), err)
					continue
				}
				rule := proto.GetExtension(m.Options(), annotations.E_Http).(*annotations.HttpRule)
				if strings.ContainsAny(path, "{}") || method == "" {
					t.Errorf("%s: %s %s", m.FullName(), method, path)
				}
				if (rule.GetBody() == "*") != (body != nil) || (body == nil && query == nil) {
					t.Errorf("%s: body %q, query %v", m.FullName(), body, query)
				}
				n++
			}
		}
	}
	if n < 30 {
		t.Errorf("%d methods", n)
	}
}

func TestRequestQuery(t *testing.T) {
	m, err := Method("veho.v1.JobService.ListJobs")
	if err != nil {
		t.Fatal(err)
	}
	method, path, q, body, err := Request(m, &vehov1.ListJobsRequest{State: "RUNNING", Label: map[string]string{"ticket": "OPS-1"}, Limit: 5})
	if err != nil || method != "GET" || path != "/v1/jobs" || body != nil || q.Get("label[ticket]") != "OPS-1" || q.Get("state") != "RUNNING" || q.Get("limit") != "5" {
		t.Errorf("%s %s %v %q %v", method, path, q, body, err)
	}
	m, _ = Method("veho.v1.JobService.CancelJob")
	_, path, _, body, _ = Request(m, &vehov1.JobRef{Id: "a/b"})
	if path != "/v1/jobs/a%2Fb:cancel" || body == nil {
		t.Errorf("cancel: %s %q", path, body)
	}
}

// C8-3: a token file others can read is refused.
func TestTokenFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(p, []byte("veho_x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Options{URL: "https://localhost:8443", TokenFile: p}); err == nil {
		t.Error("a 0644 token file: no error")
	}
	if err := os.Chmod(p, 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := New(Options{URL: "https://localhost:8443", TokenFile: p})
	if err != nil || c.token != "veho_x" {
		t.Errorf("a 0600 token file: %v", err)
	}
	if _, err := New(Options{URL: "http://localhost:8443", Token: "x"}); err == nil {
		t.Error("a plain http URL: no error")
	}
	if _, err := New(Options{URL: "https://localhost:8443"}); err == nil {
		t.Error("no credential: no error")
	}
}

// A step-up challenge's parameters (RFC 9470).
func TestAuthParams(t *testing.T) {
	m := authParams(`Bearer error="insufficient_user_authentication", acr_values="2 3", max_age="600"`)
	if m["error"] != "insufficient_user_authentication" || m["acr_values"] != "2 3" || m["max_age"] != "600" {
		t.Errorf("params: %v", m)
	}
	if authParams(`Basic realm="x"`) != nil || authParams("") != nil {
		t.Error("a challenge that is not Bearer")
	}
	e := &Error{Code: "step_up_required", Metadata: m}
	if acr, age, ok := e.StepUp(); !ok || acr != "2 3" || age != "600" {
		t.Errorf("StepUp: %q %q %v", acr, age, ok)
	}
	if _, _, ok := (&Error{Code: "unauthenticated"}).StepUp(); ok {
		t.Error("StepUp of another error")
	}
}
