package engine

import (
	"strings"
	"testing"
)

// FuzzGrantee: a grantee names its issuer, and never two things at once.
func FuzzGrantee(f *testing.F) {
	for _, s := range []string{"key:a", "oidc:https://i.example#alice", "group:https://i.example#ops", "spiffe://td/ns/a", "oidc:alice",
		"group:#x", "oidc:i#", "key:", "spiffe:x", "oidc:a#b#c", "key:a\nb", ""} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if !validGrantee.MatchString(s) {
			return
		}
		kind, rest, _ := strings.Cut(s, ":")
		switch kind {
		case "key":
			if rest == "" {
				t.Errorf("%q accepted", s)
			}
		case "oidc", "group":
			if issuer, name, ok := strings.Cut(rest, "#"); !ok || issuer == "" || name == "" {
				t.Errorf("%q accepted without an issuer and a name", s)
			}
		case "spiffe":
			if !strings.HasPrefix(rest, "//") || len(rest) < 3 {
				t.Errorf("%q accepted", s)
			}
		default:
			t.Errorf("%q accepted", s)
		}
	})
}
