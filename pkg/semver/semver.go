// Package semver parses Semantic Versioning 2.0 versions and ranges of them
// in interval notation, as the manifests of veho's plugins declare the
// versions of veho they support (.specs/veho-plugin-distribution.md §3.1).
// Standard library only, for publishers' tools as well as veho.
package semver

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Version is a Semantic Versioning 2.0 version, MAJOR.MINOR.PATCH with an
// optional pre-release and build metadata; the build metadata is kept for
// String but takes no part in the precedence.
type Version struct {
	Major, Minor, Patch uint64
	Pre                 []string // the dot-separated pre-release identifiers
	raw                 string
}

// Parse parses s, which has no leading "v".
func Parse(s string) (Version, error) {
	v := Version{raw: s}
	rest := s
	if i := strings.IndexByte(rest, '+'); i >= 0 {
		if err := identifiers(rest[i+1:], false); err != nil {
			return Version{}, fmt.Errorf("version %q: build metadata: %w", s, err)
		}
		rest = rest[:i]
	}
	if i := strings.IndexByte(rest, '-'); i >= 0 {
		if err := identifiers(rest[i+1:], true); err != nil {
			return Version{}, fmt.Errorf("version %q: pre-release: %w", s, err)
		}
		v.Pre = strings.Split(rest[i+1:], ".")
		rest = rest[:i]
	}
	parts := strings.Split(rest, ".")
	if len(parts) != 3 {
		return Version{}, fmt.Errorf("version %q: want MAJOR.MINOR.PATCH", s)
	}
	for i, dst := range []*uint64{&v.Major, &v.Minor, &v.Patch} {
		n, err := number(parts[i])
		if err != nil {
			return Version{}, fmt.Errorf("version %q: %w", s, err)
		}
		*dst = n
	}
	return v, nil
}

// MustParse is Parse that panics, for constants and tests.
func MustParse(s string) Version {
	v, err := Parse(s)
	if err != nil {
		panic(err)
	}
	return v
}

func (v Version) String() string { return v.raw }

// Compare returns -1, 0 or 1 as v precedes, equals or follows w.
func (v Version) Compare(w Version) int {
	for _, c := range [][2]uint64{{v.Major, w.Major}, {v.Minor, w.Minor}, {v.Patch, w.Patch}} {
		if c[0] != c[1] {
			return cmp(c[0] < c[1])
		}
	}
	switch {
	case len(v.Pre) == 0 && len(w.Pre) == 0:
		return 0
	case len(v.Pre) == 0:
		return 1 // a release follows its pre-releases
	case len(w.Pre) == 0:
		return -1
	}
	for i := 0; i < len(v.Pre) && i < len(w.Pre); i++ {
		a, b := v.Pre[i], w.Pre[i]
		if a == b {
			continue
		}
		na, ea := strconv.ParseUint(a, 10, 64)
		nb, eb := strconv.ParseUint(b, 10, 64)
		switch {
		case ea == nil && eb == nil:
			return cmp(na < nb)
		case ea == nil:
			return -1 // numeric identifiers precede alphanumeric ones
		case eb == nil:
			return 1
		}
		return cmp(a < b)
	}
	if len(v.Pre) == len(w.Pre) {
		return 0
	}
	return cmp(len(v.Pre) < len(w.Pre))
}

func cmp(less bool) int {
	if less {
		return -1
	}
	return 1
}

// number parses a numeric identifier: digits, no leading zero.
func number(s string) (uint64, error) {
	if s == "" || (len(s) > 1 && s[0] == '0') || strings.TrimLeft(s, "0123456789") != "" {
		return 0, fmt.Errorf("%q is not a number without leading zeros", s)
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%q: %w", s, err)
	}
	return n, nil
}

const alphabet = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ-"

// identifiers checks dot-separated identifiers of [0-9A-Za-z-], not empty;
// in a pre-release, numeric ones have no leading zero.
func identifiers(s string, pre bool) error {
	for _, id := range strings.Split(s, ".") {
		if id == "" {
			return errors.New("an empty identifier")
		}
		for _, c := range id {
			if !strings.ContainsRune(alphabet, c) {
				return fmt.Errorf("%q: only [0-9A-Za-z-]", id)
			}
		}
		if pre && strings.TrimLeft(id, "0123456789") == "" {
			if _, err := number(id); err != nil {
				return err
			}
		}
	}
	return nil
}

// Range is an interval of versions: "[0.5.0,0.6.0)" includes 0.5.0 and
// excludes 0.6.0; an empty bound is unbounded and takes "(" or ")";
// "[0.4.0]" is 0.4.0 alone.
type Range struct {
	lo, hi     *Version
	loIn, hiIn bool
	raw        string
}

// ParseRange parses s; spaces around the bounds are allowed.
func ParseRange(s string) (Range, error) {
	r := Range{raw: s}
	bad := func(why string) (Range, error) {
		return Range{}, fmt.Errorf("range %q: %s", s, why)
	}
	if len(s) < 2 {
		return bad("want [ or ( LOW , HIGH ] or ), or [VERSION]")
	}
	open, closing := s[0], s[len(s)-1]
	if open != '[' && open != '(' || closing != ']' && closing != ')' {
		return bad("want [ or ( LOW , HIGH ] or ), or [VERSION]")
	}
	r.loIn, r.hiIn = open == '[', closing == ']'
	bounds := strings.Split(s[1:len(s)-1], ",")
	switch len(bounds) {
	case 1:
		if !r.loIn || !r.hiIn {
			return bad("a single version takes [ and ]")
		}
		v, err := Parse(strings.TrimSpace(bounds[0]))
		if err != nil {
			return bad(err.Error())
		}
		r.lo, r.hi = &v, &v
		return r, nil
	case 2:
	default:
		return bad("one comma between the bounds")
	}
	for i, b := range bounds {
		b = strings.TrimSpace(b)
		if b == "" {
			if i == 0 && r.loIn || i == 1 && r.hiIn {
				return bad("an unbounded side takes ( or )")
			}
			continue
		}
		v, err := Parse(b)
		if err != nil {
			return bad(err.Error())
		}
		if i == 0 {
			r.lo = &v
		} else {
			r.hi = &v
		}
	}
	if r.lo != nil && r.hi != nil {
		if c := r.lo.Compare(*r.hi); c > 0 || c == 0 && (!r.loIn || !r.hiIn) {
			return bad("no version is in it")
		}
	}
	return r, nil
}

// Contains reports whether v is in r.
func (r Range) Contains(v Version) bool {
	if r.lo != nil {
		if c := v.Compare(*r.lo); c < 0 || c == 0 && !r.loIn {
			return false
		}
	}
	if r.hi != nil {
		if c := v.Compare(*r.hi); c > 0 || c == 0 && !r.hiIn {
			return false
		}
	}
	return true
}

func (r Range) String() string { return r.raw }
