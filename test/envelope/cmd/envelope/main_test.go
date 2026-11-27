package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dihedron/veho/test/envelope"
)

// The command's verbs and their errors, as test/lab/lib/envelope.sh calls
// them.
func TestRun(t *testing.T) {
	results := filepath.Join(t.TempDir(), "results.csv")
	data := filepath.Join(t.TempDir(), "envelope.csv")
	f, err := os.Create(results)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(envelope.Header + "\nc1,0.3.0,2026-11-26,A-rtt25-j0,regional,true,0,0,12,40,35,70,40,0,1,0\n")
	_ = f.Close()
	for _, args := range [][]string{
		{"plan", "--phase", "A", "--rtt", "25", "--results", results, "--commit", "c1"},
		{"plan", "--phase", "B", "--results", filepath.Join(t.TempDir(), "none.csv")},
		{"profile", "--rtt", "400"},
		{"report", "--results", results},
		{"report", "--results", results, "--phase", "B"},
		{"publish", "--results", results, "--data", data},
		{"publish", "--results", results, "--data", data}, // again: the same row
	} {
		if err := run(args); err != nil {
			t.Errorf("%v: %v", args, err)
		}
	}
	for _, args := range [][]string{
		nil, {"nonsense"}, {"plan", "--phase", "C"}, {"plan", "--phase", "A", "--rtt", "x"},
		{"profile"}, {"profile", "--rtt", "25,50"}, {"plan", "--bogus"}, {"publish", "--results", results},
	} {
		if err := run(args); err == nil {
			t.Errorf("%v: no error", args)
		}
	}
	bad := filepath.Join(t.TempDir(), "bad.csv")
	_ = os.WriteFile(bad, []byte("nonsense\n"), 0o600)
	if err := run([]string{"report", "--results", bad}); err == nil {
		t.Error("a results.csv without its header")
	}
	if err := run([]string{"publish", "--results", results, "--data", bad}); err == nil {
		t.Error("data without its header")
	}
	b, err := os.ReadFile(data)
	if err != nil || strings.Count(string(b), "\n") != 2 {
		t.Errorf("the data: %q, %v", b, err)
	}
	if b, err := os.ReadFile(strings.TrimSuffix(data, ".csv") + ".md"); err != nil || !strings.Contains(string(b), "| A-rtt25-j0 | 25 |") {
		t.Errorf("the table: %q, %v", b, err)
	}
}
