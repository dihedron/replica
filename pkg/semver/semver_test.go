package semver

import (
	"testing"
)

func TestParse(t *testing.T) {
	for _, s := range []string{"0.0.0", "1.2.3", "10.20.30", "1.0.0-rc.1", "1.0.0-0.3.7", "1.0.0-x-y.z", "1.0.0+build.5", "1.0.0-alpha+001", "0.4.1-SNAPSHOT-29bb882"} {
		if _, err := Parse(s); err != nil {
			t.Errorf("%q: %v", s, err)
		}
	}
	for _, s := range []string{"", "1", "1.2", "1.2.3.4", "v1.2.3", "01.2.3", "1.02.3", "1.2.03", "1.2.3-", "1.2.3-01", "1.2.3-a..b", "1.2.3+", "1.2.3+a..b", "1.2.3-a_b", "-1.2.3", "1.2.x", " 1.2.3", "99999999999999999999.0.0"} {
		if _, err := Parse(s); err == nil {
			t.Errorf("%q: accepted", s)
		}
	}
}

// TestPrecedence: the order of Semantic Versioning 2.0, §11.
func TestPrecedence(t *testing.T) {
	order := []string{"1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.beta", "1.0.0-beta", "1.0.0-beta.2", "1.0.0-beta.11", "1.0.0-rc.1", "1.0.0", "1.0.1", "1.1.0", "2.0.0"}
	for i := range order {
		for j := range order {
			a, b := MustParse(order[i]), MustParse(order[j])
			want := 0
			if i < j {
				want = -1
			} else if i > j {
				want = 1
			}
			if got := a.Compare(b); got != want {
				t.Errorf("Compare(%s, %s) = %d, want %d", a, b, got, want)
			}
		}
	}
	if MustParse("1.0.0+a").Compare(MustParse("1.0.0+b")) != 0 {
		t.Error("build metadata is not ignored")
	}
	if s := MustParse("1.0.0-rc.1+b").String(); s != "1.0.0-rc.1+b" {
		t.Errorf("String: %q", s)
	}
}

func TestRange(t *testing.T) {
	cases := []struct {
		r       string
		in, out []string
	}{
		{"[0.5.0,0.6.0)", []string{"0.5.0", "0.5.1-rc.1", "0.5.9", "0.6.0-rc.1"}, []string{"0.4.9", "0.5.0-rc.1", "0.6.0"}},
		{"(0.5.0,0.6.0]", []string{"0.5.1", "0.6.0"}, []string{"0.5.0", "0.6.1"}},
		{"[0.5.0,)", []string{"0.5.0", "9.0.0"}, []string{"0.4.0"}},
		{"(,1.0.0)", []string{"0.0.0", "0.9.9"}, []string{"1.0.0"}},
		{"(,)", []string{"0.0.0", "9.9.9"}, nil},
		{"[0.4.0]", []string{"0.4.0", "0.4.0+b"}, []string{"0.4.1", "0.4.0-rc.1"}},
		{"[0.5.0, 0.6.0)", []string{"0.5.0"}, []string{"0.6.0"}},
		{"[1.0.0,1.0.0]", []string{"1.0.0"}, []string{"1.0.1"}},
	}
	for _, c := range cases {
		r, err := ParseRange(c.r)
		if err != nil {
			t.Errorf("%q: %v", c.r, err)
			continue
		}
		for _, v := range c.in {
			if !r.Contains(MustParse(v)) {
				t.Errorf("%q does not contain %s", c.r, v)
			}
		}
		for _, v := range c.out {
			if r.Contains(MustParse(v)) {
				t.Errorf("%q contains %s", c.r, v)
			}
		}
		if r.String() != c.r {
			t.Errorf("String: %q, want %q", r.String(), c.r)
		}
	}
	for _, s := range []string{"", "[", "[]", "()", "0.5.0", "[0.5.0", "0.5.0]", "(0.5.0)", "[0.5.0)", "[,1.0.0)", "(1.0.0,]", "[0.5.0,0.6.0,0.7.0)", "[0.6.0,0.5.0)", "[1.0.0,1.0.0)", "(1.0.0,1.0.0]", "[x,1.0.0)", "{0.5.0,0.6.0}"} {
		if _, err := ParseRange(s); err == nil {
			t.Errorf("%q: accepted", s)
		}
	}
}

// FuzzParseRange: a range that parses prints back to one that parses to the
// same range, and the parser never panics.
func FuzzParseRange(f *testing.F) {
	for _, s := range []string{"[0.5.0,0.6.0)", "[0.4.0]", "(,1.0.0-rc.1)", "[1.0.0+b,)", "(,)", "[0.6.0,0.5.0)"} {
		f.Add(s, "0.5.5")
	}
	f.Fuzz(func(t *testing.T, s, v string) {
		r, err := ParseRange(s)
		if err != nil {
			return
		}
		again, err := ParseRange(r.String())
		if err != nil {
			t.Fatalf("%q prints %q, which does not parse: %v", s, r.String(), err)
		}
		if ver, err := Parse(v); err == nil && r.Contains(ver) != again.Contains(ver) {
			t.Fatalf("%q and %q differ on %s", s, r.String(), v)
		}
	})
}
