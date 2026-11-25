//go:build acceptance

package acceptance

import (
	"strings"
	"testing"

	"github.com/dihedron/veho/test/labtest"
)

// PK-T3 (veho-packaging.md §4): on the six nodes veho runs under the
// profile veho and its plugin under veho-plugin, both enforcing; the short
// scenarios pass with them (this suite's other tests).
func TestAppArmorConfined(t *testing.T) {
	l := labtest.New(t)
	for _, node := range l.Nodes() {
		out := l.Sh("ssh", node, "sh", "-c", `for p in $(pgrep -x veho) $(pgrep -f veho-plugin-s3); do cat /proc/$p/attr/current; echo; done`)
		if !strings.Contains(out, "veho (enforce)") || !strings.Contains(out, "veho-plugin (enforce)") {
			t.Errorf("%s: %q", node, out)
		}
	}
}

// PK-T4: the confinement holds: a probe among the plugins (a copy of cat),
// run under veho-plugin as veho would run it, reads the CA roots but not
// veho's environment file, configuration or unwrap key.
func TestAppArmorDenies(t *testing.T) {
	l := labtest.New(t)
	probe := "/usr/libexec/veho/plugins/veho-plugin-probe"
	l.Sh("ssh", "veho-a1", "cp", "/usr/bin/cat", probe)
	t.Cleanup(func() { _, _ = l.TrySh("ssh", "veho-a1", "rm", "-f", probe) })
	read := func(path string) (string, error) {
		return l.TrySh("ssh", "veho-a1", "aa-exec", "-p", "veho-plugin", "--", probe, path)
	}
	if out, err := read("/etc/ssl/certs/ca-certificates.crt"); err != nil {
		t.Fatalf("the CA roots, allowed: %v, %.200q", err, out)
	}
	for _, path := range []string{"/etc/veho/veho.env", "/etc/veho/veho.yaml", "/etc/veho/unwrap.key"} {
		if out, err := read(path); err == nil || !strings.Contains(out+err.Error(), "ermission denied") {
			t.Errorf("%s: %v, %.200q", path, err, out)
		}
	}
}
