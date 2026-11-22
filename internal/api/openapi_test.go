package api

import (
	"os"
	"strings"
	"testing"

	vehov1 "github.com/dihedron/veho/api/veho/v1"
	"go.yaml.in/yaml/v3"
	"google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// API-T23: the OpenAPI document has every REST route of the services, with
// the method the gateway serves.
func TestOpenAPIRoutes(t *testing.T) {
	data, err := os.ReadFile("../../api/openapi/veho.v1.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, fd := range []protoreflect.FileDescriptor{vehov1.File_veho_v1_jobs_proto, vehov1.File_veho_v1_policies_proto, vehov1.File_veho_v1_admin_proto} {
		for i := range fd.Services().Len() {
			sd := fd.Services().Get(i)
			for j := range sd.Methods().Len() {
				m := sd.Methods().Get(j)
				rule, _ := proto.GetExtension(m.Options(), annotations.E_Http).(*annotations.HttpRule)
				method, path := route(rule)
				if path == "" {
					t.Errorf("%s has no REST route", m.FullName())
					continue
				}
				if _, ok := doc.Paths[path][method]; !ok {
					t.Errorf("%s: %s %s is not in the OpenAPI document", m.FullName(), method, path)
				}
				if _, ok := roles["/"+string(sd.FullName())+"/"+string(m.Name())]; !ok {
					t.Errorf("%s has no role", m.FullName())
				}
				n++
			}
		}
	}
	if n < 30 {
		t.Errorf("only %d methods", n)
	}
}

func route(r *annotations.HttpRule) (string, string) {
	switch p := r.GetPattern().(type) {
	case *annotations.HttpRule_Get:
		return "get", p.Get
	case *annotations.HttpRule_Post:
		return "post", p.Post
	case *annotations.HttpRule_Put:
		return "put", p.Put
	case *annotations.HttpRule_Delete:
		return "delete", p.Delete
	case *annotations.HttpRule_Patch:
		return "patch", strings.TrimSpace(p.Patch)
	}
	return "", ""
}
