package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dihedron/veho/test/envelope"
)

// The command's verbs and their errors, as test/lab/lib/envelope.sh calls
// them.
func TestRun(t *testing.T) {
	results := filepath.Join(t.TempDir(), "results.csv")
	f, err := os.Create(results)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(envelope.Header + "\nA-rtt25-j0,regional,true,0,0,12,40,35,70,40,0,1,0\n")
	_ = f.Close()
	for _, args := range [][]string{
		{"plan", "--phase", "A", "--rtt", "25", "--results", results},
		{"plan", "--phase", "B", "--results", filepath.Join(t.TempDir(), "none.csv")},
		{"profile", "--rtt", "400"},
		{"report", "--results", results},
		{"report", "--results", results, "--phase", "B"},
	} {
		if err := run(args); err != nil {
			t.Errorf("%v: %v", args, err)
		}
	}
	for _, args := range [][]string{
		nil, {"nonsense"}, {"plan", "--phase", "C"}, {"plan", "--phase", "A", "--rtt", "x"},
		{"profile"}, {"profile", "--rtt", "25,50"}, {"plan", "--bogus"},
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
}
