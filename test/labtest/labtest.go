// Package labtest drives the lab (test/lab/lab.sh) from integration tests.
package labtest

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// labSh is test/lab/lab.sh, found from this file.
func labSh() string {
	_, here, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(here), "..", "lab", "lab.sh")
}

type member struct {
	Member string `json:"Member"`
	Role   string `json:"Role"`
	State  string `json:"State"`
}

func members(t *testing.T) []member {
	t.Helper()
	out, err := exec.Command(labSh(), "patronictl", "list", "-f", "json").Output() //nolint:gosec // the lab's own script
	if err != nil {
		t.Fatalf("patronictl list: %v", err)
	}
	var ms []member
	if err := json.Unmarshal(out, &ms); err != nil {
		t.Fatalf("patronictl list: %v: %s", err, out)
	}
	return ms
}

// Switchover moves the primary to a quorum standby and waits until the new
// primary runs; it skips the test without the lab.
func Switchover(t *testing.T) string {
	t.Helper()
	if os.Getenv("VEHO_TEST_DATABASE_URL") == "" {
		t.Skip("the lab is not running: run with test/lab/lab.sh test")
	}
	var leader, candidate string
	for _, m := range members(t) {
		switch {
		case m.Role == "Leader":
			leader = m.Member
		case candidate == "" && m.State == "streaming":
			candidate = m.Member
		}
	}
	if leader == "" || candidate == "" {
		t.Fatalf("no leader or no streaming standby: %+v", members(t))
	}
	if out, err := exec.Command(labSh(), "patronictl", "switchover", "--leader", leader, "--candidate", candidate, "--force").CombinedOutput(); err != nil { //nolint:gosec // the lab's own script
		t.Fatalf("switchover %s to %s: %v: %s", leader, candidate, err, out)
	}
	for deadline := time.Now().Add(90 * time.Second); time.Now().Before(deadline); time.Sleep(time.Second) {
		for _, m := range members(t) {
			if m.Member == candidate && m.Role == "Leader" && m.State == "running" {
				return candidate
			}
		}
	}
	t.Fatalf("%s did not become the leader", candidate)
	return ""
}
