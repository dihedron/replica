package main

import (
	"os/exec"
	"strings"
	"testing"
)

// TestLeanCore: PD-T4, the drivers with third-party dependencies are
// plugins, so veho links no AWS SDK (veho-plugin-distribution.md §1).
func TestLeanCore(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, pkg := range strings.Fields(string(out)) {
		if strings.HasPrefix(pkg, "github.com/aws/") {
			t.Fatalf("veho depends on %s", pkg)
		}
	}
}
