// Command openapi adjusts the OpenAPI document generated from the protos to
// what the REST gateway does (.specs/veho-api-server.md §5.2): errors are
// problem+json documents (not google.rpc.Status), answered as 4XX and 5XX;
// the operations that accept work answer 202. make proto runs it.
//
//	go run ./internal/tools/openapi api/openapi/veho.v1.yaml
package main

import (
	"bytes"
	"fmt"
	"os"

	"go.yaml.in/yaml/v3"
)

// accepted are the operations answered 202 (§5).
var accepted = map[string]bool{
	"veho.v1.JobService.CreateJob": true,
	"veho.v1.JobService.CancelJob": true,
	"veho.v1.JobService.RetryJob":  true,
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: openapi FILE")
		os.Exit(2)
	}
	if err := fix(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, "openapi:", err)
		os.Exit(1)
	}
}

func fix(path string) error {
	data, err := os.ReadFile(path) //nolint:gosec // the build's own file
	if err != nil {
		return err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return err
	}
	root := doc.Content[0]
	paths := get(root, "paths")
	if paths == nil {
		return fmt.Errorf("%s: no paths", path)
	}
	for i := 1; i < len(paths.Content); i += 2 {
		ops := paths.Content[i]
		for j := 1; j < len(ops.Content); j += 2 {
			fixResponses(ops.Content[j])
		}
	}
	trimSchemas(root)
	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return err
	}
	return os.WriteFile(path, out.Bytes(), 0o644) //nolint:gosec // a committed document
}

func get(m *yaml.Node, k string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == k {
			return m.Content[i+1]
		}
	}
	return nil
}

func key(m *yaml.Node, k string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == k {
			return m.Content[i]
		}
	}
	return nil
}

func del(m *yaml.Node, k string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == k {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return
		}
	}
}

// collect gathers every $ref of n.
func collect(n *yaml.Node, refs map[string]bool) {
	if n.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == "$ref" {
				refs[n.Content[i+1].Value] = true
			}
		}
	}
	for _, c := range n.Content {
		collect(c, refs)
	}
}

// fixResponses gives an operation its problem+json errors, and 202 where the
// gateway answers it.
func fixResponses(op *yaml.Node) {
	responses := get(op, "responses")
	if op.Kind != yaml.MappingNode || responses == nil {
		return
	}
	del(responses, "default")
	if id := get(op, "operationId"); id != nil && accepted[id.Value] {
		if ok := key(responses, "200"); ok != nil {
			ok.Value = "202"
			if desc := get(get(responses, "202"), "description"); desc != nil {
				desc.Value = "Accepted"
			}
		}
	}
	for _, code := range []string{"4XX", "5XX"} {
		responses.Content = append(responses.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: code, Style: yaml.DoubleQuotedStyle},
			&yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{
				{Kind: yaml.ScalarNode, Value: "$ref"},
				{Kind: yaml.ScalarNode, Value: "#/components/responses/Problem"},
			}})
	}
}

// trimSchemas drops every schema nothing refers to, until none: the
// requests mapped to path and query parameters are left unreferenced.
func trimSchemas(root *yaml.Node) {
	schemas := get(get(root, "components"), "schemas")
	for schemas != nil {
		refs := map[string]bool{}
		collect(root, refs)
		dropped := false
		for i := 0; i+1 < len(schemas.Content); {
			if !refs["#/components/schemas/"+schemas.Content[i].Value] {
				schemas.Content = append(schemas.Content[:i], schemas.Content[i+2:]...)
				dropped = true
				continue
			}
			i += 2
		}
		if !dropped {
			return
		}
	}
}
