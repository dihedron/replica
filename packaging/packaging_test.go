// Package packaging_test checks the packages' hardening
// (.specs/veho-packaging.md): the AppArmor profiles and what the packages
// carry and depend on.
package packaging_test

import (
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// PK-T1: both profiles parse, with the system's abstractions; the plugins'
// profile denies veho's secrets.
func TestAppArmorProfiles(t *testing.T) {
	b, err := os.ReadFile("apparmor/veho")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"profile veho /usr/bin/veho", "profile veho-plugin", "rPx -> veho-plugin",
		"deny /etc/veho/** rwklx,", "deny /run/credentials/** rwklx,", "include if exists <local/veho>", "include if exists <local/veho-plugin>"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("the profile lacks %q", want)
		}
	}
	// veho writes nothing but go-plugin's sockets and the system log (PK-2)
	veho := string(b[:strings.Index(string(b), "profile veho-plugin")])
	for _, line := range strings.Split(veho, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "/") && strings.Contains(strings.Fields(line)[len(strings.Fields(line))-1], "w") &&
			!strings.HasPrefix(line, "/dev/log") && !strings.HasPrefix(line, "/run/systemd/journal/") {
			t.Errorf("veho may write: %s", line)
		}
	}
	if !strings.Contains(veho, "owner /tmp/plugin* rw,") {
		t.Error("veho's go-plugin sockets")
	}
	parser, err := exec.LookPath("apparmor_parser")
	if err != nil {
		t.Skip("no apparmor_parser here")
	}
	if out, err := exec.Command(parser, "-Q", "-T", "-K", "apparmor/veho").CombinedOutput(); err != nil {
		t.Fatalf("apparmor_parser: %v: %s", err, out)
	}
}

type nfpm struct {
	ID           string   `yaml:"id"`
	Dependencies []string `yaml:"dependencies"`
	Contents     []struct {
		Src      string `yaml:"src"`
		Dst      string `yaml:"dst"`
		Packager string `yaml:"packager"`
	} `yaml:"contents"`
	Overrides map[string]struct {
		Dependencies []string `yaml:"dependencies"`
		Recommends   []string `yaml:"recommends"`
	} `yaml:"overrides"`
	Scripts map[string]string `yaml:"scripts"`
}

// PK-T2: the veho package carries the profile (deb), the firewalld
// service (both) and the scripts that load and unload the profile, and
// depends on the CA roots; the deb recommends apparmor.
func TestPackages(t *testing.T) {
	b, err := os.ReadFile("../.goreleaser.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		NFPMs []nfpm `yaml:"nfpms"`
	}
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(cfg.NFPMs, func(n nfpm) bool { return n.ID == "veho" })
	if i < 0 {
		t.Fatal("no nfpm veho")
	}
	n := cfg.NFPMs[i]
	carries := func(dst, packager string) bool {
		return slices.ContainsFunc(n.Contents, func(c struct {
			Src      string `yaml:"src"`
			Dst      string `yaml:"dst"`
			Packager string `yaml:"packager"`
		}) bool {
			return c.Dst == dst && c.Packager == packager
		})
	}
	if !carries("/etc/apparmor.d/veho", "deb") || !carries("/usr/lib/firewalld/services/veho.xml", "") {
		t.Errorf("contents: %+v", n.Contents)
	}
	for _, f := range []string{"postinstall", "preremove"} {
		if n.Scripts[f] == "" {
			t.Errorf("no %s script", f)
		}
	}
	if !slices.Contains(n.Overrides["deb"].Dependencies, "ca-certificates") || !slices.Contains(n.Overrides["deb"].Recommends, "apparmor") ||
		!slices.Contains(n.Overrides["rpm"].Dependencies, "ca-certificates") {
		t.Errorf("dependencies: %+v", n.Overrides)
	}
}
