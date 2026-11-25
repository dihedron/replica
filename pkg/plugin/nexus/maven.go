package nexus

import (
	"regexp"
	"strings"

	"github.com/dihedron/veho/pkg/plugin"
)

// maven is a key of a maven2 endpoint (veho-nexus.md §2.2): its path in
// the Maven layout and its extension.
type maven struct {
	path, ext string
	snapshot  bool
}

var (
	mavenName      = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9._-]*$`)
	mavenClass     = regexp.MustCompile(`^[A-Za-z0-9_-]+$`) // no '.': it would read as the extension's
	mavenExt       = regexp.MustCompile(`^[A-Za-z0-9]+(\.[A-Za-z0-9]+)*$`)
	mavenTimestamp = regexp.MustCompile(`^(.+)-[0-9]{8}\.[0-9]{6}-[0-9]+$`) // a snapshot's version
	mavenStamp     = regexp.MustCompile(`^-[0-9]{8}\.[0-9]{6}-[0-9]+`)      // and its file name's
)

// generated: Nexus writes these itself, so they are refused as a
// destination (NX-8).
func (m maven) generated() bool {
	for _, s := range []string{"md5", "sha1", "sha256", "sha512"} {
		if m.ext == s || strings.HasSuffix(m.ext, "."+s) {
			return true
		}
	}
	return false
}

func badMaven(key string) error {
	return plugin.Wrapf(plugin.Permanent, "nexus: %q is neither a Maven path (group/artifact/version/artifact-version[-classifier].ext) nor group:artifact:version[:classifier]@ext", key)
}

// mavenPath maps coordinates, or checks a path, to the Maven layout.
func mavenPath(key string) (maven, error) {
	if strings.Contains(key, ":") {
		gav, ext, ok := strings.Cut(key, "@")
		parts := strings.Split(gav, ":")
		if !ok || len(parts) < 3 || len(parts) > 4 || strings.Contains(ext, "@") {
			return maven{}, badMaven(key)
		}
		classifier := ""
		if len(parts) == 4 {
			classifier = parts[3]
		}
		return mavenLayout(key, strings.Split(parts[0], "."), parts[1], parts[2], classifier, ext)
	}
	segs := strings.Split(key, "/")
	if len(segs) < 4 {
		return maven{}, badMaven(key)
	}
	group, artifact, dir, file := segs[:len(segs)-3], segs[len(segs)-3], segs[len(segs)-2], segs[len(segs)-1]
	rest, ok := strings.CutPrefix(file, artifact+"-")
	if !ok {
		return maven{}, badMaven(key)
	}
	// the file's version is the directory's, or a timestamp of a snapshot
	version := dir
	if base, snap := strings.CutSuffix(dir, "-SNAPSHOT"); snap && !strings.HasPrefix(rest, dir) {
		after, ok := strings.CutPrefix(rest, base)
		stamp := mavenStamp.FindString(after)
		if !ok || stamp == "" {
			return maven{}, badMaven(key)
		}
		version = base + stamp
	}
	rest, ok = strings.CutPrefix(rest, version)
	if !ok {
		return maven{}, badMaven(key)
	}
	classifier, ext := "", ""
	switch {
	case strings.HasPrefix(rest, "-"):
		classifier, ext, ok = strings.Cut(rest[1:], ".")
	case strings.HasPrefix(rest, "."):
		ext, ok = rest[1:], true
	default:
		ok = false
	}
	if !ok {
		return maven{}, badMaven(key)
	}
	m, err := mavenLayout(key, group, artifact, version, classifier, ext)
	if err != nil || m.path != key {
		return maven{}, badMaven(key)
	}
	return m, nil
}

// mavenLayout is the path of the coordinates, each part checked.
func mavenLayout(key string, group []string, artifact, version, classifier, ext string) (maven, error) {
	for _, s := range append(group, artifact, version) {
		if !mavenName.MatchString(s) {
			return maven{}, badMaven(key)
		}
	}
	if classifier != "" && !mavenClass.MatchString(classifier) || !mavenExt.MatchString(ext) {
		return maven{}, badMaven(key)
	}
	dir := version
	if m := mavenTimestamp.FindStringSubmatch(version); m != nil {
		dir = m[1] + "-SNAPSHOT"
	}
	file := artifact + "-" + version
	if classifier != "" {
		file += "-" + classifier
	}
	path := strings.Join(append(append([]string{}, group...), artifact, dir, file+"."+ext), "/")
	return maven{path: path, ext: ext, snapshot: strings.HasSuffix(dir, "-SNAPSHOT")}, nil
}
