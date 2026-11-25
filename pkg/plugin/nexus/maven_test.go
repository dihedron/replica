package nexus

import (
	"context"
	"strings"
	"testing"

	"github.com/dihedron/veho/pkg/plugin"
	"github.com/dihedron/veho/pkg/plugin/conformance"
)

// NX-T6: coordinates and paths both ways.
func TestMavenPath(t *testing.T) {
	for key, want := range map[string]string{
		"org.acme:app:1.2@jar":                                "org/acme/app/1.2/app-1.2.jar",
		"org.acme:app:1.2:sources@jar":                        "org/acme/app/1.2/app-1.2-sources.jar",
		"org.acme:app:1.2@pom":                                "org/acme/app/1.2/app-1.2.pom",
		"org.acme:app:1.2@tar.gz":                             "org/acme/app/1.2/app-1.2.tar.gz",
		"acme:app:1.0-SNAPSHOT@jar":                           "acme/app/1.0-SNAPSHOT/app-1.0-SNAPSHOT.jar",
		"acme:app:1.0-20261125.101010-3@jar":                  "acme/app/1.0-SNAPSHOT/app-1.0-20261125.101010-3.jar",
		"org/acme/app/1.2/app-1.2.jar":                        "org/acme/app/1.2/app-1.2.jar",
		"org/acme/app/1.2/app-1.2-sources.jar":                "org/acme/app/1.2/app-1.2-sources.jar",
		"org/acme/app/1.2/app-1.2.jar.sha1":                   "org/acme/app/1.2/app-1.2.jar.sha1",
		"acme/app/1.0-SNAPSHOT/app-1.0-20261125.101010-3.jar": "acme/app/1.0-SNAPSHOT/app-1.0-20261125.101010-3.jar",
	} {
		got, err := mavenPath(key)
		if err != nil || got.path != want {
			t.Errorf("%s: %q, %v; want %q", key, got.path, err, want)
		}
	}
	for _, key := range []string{
		"", "app.jar", "org/acme/app/1.2/other-1.2.jar", "org/acme/app/1.2/app-1.3.jar", "org/acme/maven-metadata.xml",
		"org/acme/app/maven-metadata.xml", "a:b@jar", "a:b:c:d:e@jar", "a:b:1.0", "a:b:1.0@", ".a:b:1.0@jar",
		"a:b/c:1.0@jar", "a:b:1.0@j/ar", "org/../app/1.2/app-1.2.jar", "org/acme/app/1.0-SNAPSHOT/app-1.0-2026.jar",
	} {
		if _, err := mavenPath(key); plugin.ClassOf(err) != plugin.Permanent {
			t.Errorf("%q accepted: %v", key, err)
		}
	}
	for key, generated := range map[string]bool{
		"org/acme/app/1.2/app-1.2.jar.sha1": true, "a:b:1@jar.md5": true, "a:b:1@jar.sha256": true, "a:b:1@jar.sha512": true,
		"a:b:1@jar.asc": false, "a:b:1@jar": false,
	} {
		if p, err := mavenPath(key); err != nil || p.generated() != generated {
			t.Errorf("%s: generated %v, %v", key, p.generated(), err)
		}
	}
}

// NX-T6 on the fake: the suite through coordinates, checksum keys refused
// as a destination, and the version policy.
func TestMaven(t *testing.T) {
	f := newFake(t)
	conformance.Run(t, conformance.Target{
		Driver: driver(t), Config: f.config("releases", `, "format": "maven2"`), Credentials: good,
		BadCredentials: plugin.Credentials{"username": "veho", "password": "wrong"}, PartSize: 1 << 20,
		Key: func(k string) string { return "org.veho:conformance:" + k + "@bin" },
	})
	ctx := context.Background()
	c := open(t, f, "releases", `, "format": "maven2"`, good)
	for _, key := range []string{"org/acme/app/1.2/app-1.2.jar.sha1", "a:b:1@jar.md5"} {
		if _, err := c.Begin(ctx, plugin.Ref{Key: key}, plugin.ObjectInfo{}, plugin.PutOptions{}); plugin.ClassOf(err) != plugin.Permanent {
			t.Errorf("%s as a destination: %v", key, err)
		}
	}
	if _, err := put(t, c, "a:b:1.0-SNAPSHOT@jar", []byte("x")); plugin.ClassOf(err) != plugin.Permanent || !strings.Contains(err.Error(), "version policy") {
		t.Errorf("snapshot into releases: %v", err)
	}
	if _, err := put(t, c, "a:b:1.0@jar", []byte("jar")); err != nil {
		t.Fatal(err)
	}
	if info, err := c.Stat(ctx, plugin.Ref{Key: "a/b/1.0/b-1.0.jar"}); err != nil || info.Size != 3 {
		t.Fatalf("the path of the coordinates: %+v, %v", info, err)
	}
	if err := c.Delete(ctx, plugin.Ref{Key: "a:b:1.0@jar"}); err != nil {
		t.Fatal(err)
	}
	if _, err := driver(t).Open(ctx, f.config("files", `, "format": "maven2"`), good); plugin.ClassOf(err) != plugin.Permanent {
		t.Errorf("maven2 on a raw repository: %v", err)
	}
}

func FuzzMavenPath(f *testing.F) {
	for _, s := range []string{"org.acme:app:1.2:c@jar", "org/acme/app/1.2/app-1.2.jar", "a:b:1.0-20261125.101010-3@jar", "../a:b:1@jar"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, key string) {
		p, err := mavenPath(key)
		if err != nil {
			return
		}
		for _, seg := range strings.Split(p.path, "/") {
			if seg == "" || seg == "." || seg == ".." || strings.ContainsAny(seg, "\\:@%?#") {
				t.Fatalf("%q: segment %q of %q", key, seg, p.path)
			}
		}
		if q, err := mavenPath(p.path); err != nil || q.path != p.path {
			t.Fatalf("%q: %q does not parse back: %q, %v", key, p.path, q.path, err)
		}
	})
}
