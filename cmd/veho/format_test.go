package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	vehov1 "github.com/dihedron/veho/api/veho/v1"
	"github.com/jessevdk/go-flags"
	"go.yaml.in/yaml/v3"
	"google.golang.org/protobuf/proto"
)

func failedJob() *vehov1.Job {
	return &vehov1.Job{Id: "j1", State: "FAILED", Source: &vehov1.Source{Endpoint: "a-source", Path: "demo/big.bin"},
		Labels:    map[string]string{"try": "1"},
		Transfers: []*vehov1.Transfer{{Id: "t1", Endpoint: "b-dest-1", State: "SUCCEEDED", Position: map[string]string{"region": "region-b"}}, {Id: "t2", Endpoint: "c-dest-1", State: "FAILED"}}}
}

func printed(t *testing.T, format string, colours bool, msg proto.Message, columns ...string) string {
	t.Helper()
	saved := colour
	colour = func(io.Writer) bool { return colours }
	t.Cleanup(func() { colour = saved })
	var out bytes.Buffer
	o := apiOptions{Format: format, out: &out}
	if err := o.print(msg, columns...); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

// CLI-T5: a list, an object with a nested list and object, an empty list;
// colours only when allowed.
func TestFormatTable(t *testing.T) {
	real := colour
	list := printed(t, "table", false, &vehov1.ListJobsResponse{Jobs: []*vehov1.Job{failedJob(), {Id: "j2", State: "RUNNING"}}}, "id", "state")
	for _, want := range []string{"ID", "STATE", "j1", "FAILED", "j2", "RUNNING", "╭"} {
		if !strings.Contains(list, want) {
			t.Errorf("the list lacks %q:\n%s", want, list)
		}
	}
	if strings.Contains(list, "\x1b[") {
		t.Errorf("colours without a terminal:\n%q", list)
	}
	obj := printed(t, "table", false, failedJob())
	for _, want := range []string{"FIELD", "VALUE", "a-source", "demo/big.bin", "try", "transfers", "b-dest-1", "c-dest-1", "region-b"} {
		if !strings.Contains(obj, want) {
			t.Errorf("the object lacks %q:\n%s", want, obj)
		}
	}
	// each transfer is a table of its own, the rows apart by a line
	if strings.Count(obj, "│ id ") != 3 || !strings.Contains(obj, "├") {
		t.Errorf("not one table per transfer, or no line between rows:\n%s", obj)
	}
	// nested values are tables in the value's cell, and so on within them
	if strings.Contains(obj, "endpoint=a-source") || strings.Contains(obj, "region=region-b") {
		t.Errorf("a nested object as text, not as a table:\n%s", obj)
	}
	for _, line := range strings.Split(obj, "\n") {
		if strings.Contains(line, "region-b") && strings.Count(line, "│") < 6 {
			t.Errorf("the transfer's position is not a table within the transfers' table within the job's:\n%s", obj)
		}
	}
	if i, j := strings.Index(obj, "id "), strings.Index(obj, "labels"); i < 0 || i > j {
		t.Errorf("id is not the first field:\n%s", obj)
	}
	o := apiOptions{Format: "table", Wide: true}
	var wide bytes.Buffer
	o.out = &wide
	if err := o.print(&vehov1.ListJobsResponse{Jobs: []*vehov1.Job{failedJob()}}, "id", "state"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ID", "STATE", "SOURCE", "LABELS", "TRANSFERS", "endpoint=a-source path=demo/big.bin"} {
		if !strings.Contains(wide.String(), want) {
			t.Errorf("--wide lacks %q:\n%s", want, wide.String())
		}
	}
	if i, j := strings.Index(wide.String(), "ID"), strings.Index(wide.String(), "LABELS"); i < 0 || i > j {
		t.Errorf("--wide does not begin with the main columns:\n%s", wide.String())
	}
	if empty := printed(t, "table", false, &vehov1.ListJobsResponse{}, "id", "state"); !strings.Contains(empty, "ID") {
		t.Errorf("an empty list: %q", empty)
	}
	if c := printed(t, "table", true, failedJob()); !strings.Contains(c, "\x1b[") {
		t.Errorf("no colour on a terminal:\n%q", c)
	}
	t.Setenv("NO_COLOR", "1")
	if real(os.Stdout) {
		t.Error("NO_COLOR is not honoured")
	}
}

// CLI-T5: json and yaml decode to the same document, uncoloured.
func TestFormatJSONAndYAML(t *testing.T) {
	var fromJSON, fromYAML map[string]any
	j := printed(t, "json", true, failedJob())
	if err := json.Unmarshal([]byte(j), &fromJSON); err != nil {
		t.Fatalf("json: %v\n%s", err, j)
	}
	y := printed(t, "yaml", true, failedJob())
	if err := yaml.Unmarshal([]byte(y), &fromYAML); err != nil {
		t.Fatalf("yaml: %v\n%s", err, y)
	}
	a, _ := json.Marshal(fromJSON)
	b, _ := json.Marshal(fromYAML)
	if !bytes.Equal(a, b) || strings.Contains(j+y, "\x1b[") {
		t.Errorf("json and yaml differ:\n%s\n%s", a, b)
	}
}

// CLI-T5: an unknown format is refused by the parser.
func TestFormatRefused(t *testing.T) {
	var command Command
	_, err := flags.NewParser(&command, flags.HelpFlag).ParseArgs([]string{"job", "list", "-F", "xml"})
	if err == nil || !strings.Contains(err.Error(), "xml") {
		t.Errorf("-F xml: %v", err)
	}
}
