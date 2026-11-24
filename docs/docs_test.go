// Package docs holds the tests that keep the release documents complete
// (.specs/veho-docs.md §4): the documents are Markdown, built to PDF by
// build.sh.
package docs

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/dihedron/veho/internal/config"
)

const (
	configurationReference = "setup-guide/09-configuration-reference.md"
	errorReference         = "operators-guide/05-error-reference.md"
	monitoring             = "operators-guide/02-monitoring.md"
)

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// sources is the text of the non-test Go files under dirs.
func sources(t *testing.T, dirs ...string) string {
	t.Helper()
	var all strings.Builder
	for _, dir := range dirs {
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			data, err := os.ReadFile(filepath.Clean(path))
			all.Write(data)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return all.String()
}

// distinct is the sorted, distinct first groups of re in text.
func distinct(re, text string) []string {
	var out []string
	for _, m := range regexp.MustCompile(re).FindAllStringSubmatch(text, -1) {
		out = append(out, m[1])
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// keys walks a configuration type and returns its YAML keys, dotted; a
// slice of structs gives "name[].field".
func keys(t reflect.Type, prefix string) []string {
	var out []string
	for i := range t.NumField() {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		if name == "" || name == "-" {
			continue
		}
		key := prefix + name
		ft := f.Type
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		switch {
		case ft.Kind() == reflect.Struct && ft.PkgPath() == t.PkgPath():
			out = append(out, keys(ft, key+".")...)
		case ft.Kind() == reflect.Slice && ft.Elem().Kind() == reflect.Struct:
			out = append(out, keys(ft.Elem(), key+"[].")...)
		default:
			out = append(out, key)
		}
	}
	return out
}

// Every veho.yaml key is in the Setup Guide's configuration reference.
func TestConfigurationReference(t *testing.T) {
	ref := read(t, configurationReference)
	all := keys(reflect.TypeFor[config.Config](), "")
	if len(all) < 30 {
		t.Fatalf("only %d keys found: the walk is broken", len(all))
	}
	for _, k := range all {
		if !strings.Contains(ref, "`"+k+"`") {
			t.Errorf("%s: %s is not documented", configurationReference, k)
		}
	}
}

// Every problem code of the API is in the Operator's Guide's error
// reference: the codes of the API's errors and the policy's validation.
func TestErrorReference(t *testing.T) {
	ref := read(t, errorReference)
	codes := distinct(`problem\{codes\.\w+, http\.\w+, "([a-z_]+)"`, sources(t, "../internal/api"))
	codes = append(codes, distinct(`Code\w+\s*= "([a-z_]+)"`, sources(t, "../internal/policy"))...)
	if len(codes) < 15 {
		t.Fatalf("only %d codes found: the patterns are broken", len(codes))
	}
	for _, c := range codes {
		if !strings.Contains(ref, "`"+c+"`") {
			t.Errorf("%s: the code %s is not documented", errorReference, c)
		}
	}
}

// Every metric and every audit action is in the Operator's Guide's
// monitoring chapter.
func TestMonitoring(t *testing.T) {
	ref := read(t, monitoring)
	code := sources(t, "../internal", "../cmd")
	metrics := distinct(`(?:Name|NewDesc\()\s*:?\s*"(veho_[a-z_]+)"`, code)
	if len(metrics) < 5 {
		t.Fatalf("only %d metrics found: the pattern is broken", len(metrics))
	}
	for _, m := range metrics {
		if !strings.Contains(ref, "`"+m+"`") {
			t.Errorf("%s: the metric %s is not documented", monitoring, m)
		}
	}
	actions := distinct(`"((?:JOB|POLICY|ENDPOINT|SECRET|TENANT|APIKEY|ENTITLEMENT|KEK|WEBHOOK|SYSTEM)_[A-Z_]+)"`, code)
	if len(actions) < 20 {
		t.Fatalf("only %d audit actions found: the pattern is broken", len(actions))
	}
	for _, a := range actions {
		if !strings.Contains(ref, "`"+a+"`") {
			t.Errorf("%s: the audit action %s is not documented", monitoring, a)
		}
	}
}

// Every top-level command of veho is in one of the guides.
func TestCommands(t *testing.T) {
	guides := read(t, "setup-guide/07-a-first-replication.md") + read(t, "operators-guide/03-routine-procedures.md") +
		read(t, "setup-guide/04-the-state-store.md") + read(t, "setup-guide/05-the-unwrap-key.md") + read(t, "setup-guide/06-configuring-and-starting-veho.md") +
		read(t, "operators-guide/07-webhooks.md")
	commands := distinct("`command:\"([a-z]+)\"", read(t, "../cmd/veho/command.go"))
	if len(commands) < 10 {
		t.Fatalf("only %d commands found: the pattern is broken", len(commands))
	}
	for _, c := range commands {
		if !strings.Contains(guides, "veho "+c) {
			t.Errorf("the command veho %s is in no guide", c)
		}
	}
}
