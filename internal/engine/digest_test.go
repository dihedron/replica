package engine

import (
	"crypto/sha256"
	"testing"

	"github.com/dihedron/veho/pkg/plugin"
)

func TestPartSize(t *testing.T) {
	const mib = 1 << 20
	s3 := plugin.Capabilities{MaxParts: 10000, MinPartSize: 5 * mib, MaxPartSize: 512 * mib}
	for _, c := range []struct {
		size, base, want int64
		sinks            []plugin.Capabilities
	}{
		{100 * mib, 64 * mib, 64 * mib, []plugin.Capabilities{s3, {}}},
		{10000 * 100 * mib, 64 * mib, 100 * mib, []plugin.Capabilities{s3}}, // raised to fit 10,000 parts
		{1, 1, 5 * mib, []plugin.Capabilities{s3}},                          // raised to the minimum
	} {
		if got, err := partSize(c.size, c.base, c.sinks); err != nil || got != c.want {
			t.Errorf("%d bytes: %d, %v, want %d", c.size, got, err, c.want)
		}
	}
	if _, err := partSize(10000*600*mib, 64*mib, []plugin.Capabilities{s3}); plugin.ClassOf(err) != plugin.Permanent {
		t.Errorf("too large for S3's limits: %v", err)
	}
}

// FuzzPlan: a plan covers the object exactly once, in order, with parts of
// the part size but the last; and the digest needs every part.
func FuzzPlan(f *testing.F) {
	f.Add(int64(0), int64(1))
	f.Add(int64(10), int64(3))
	f.Add(int64(1<<30), int64(64<<20))
	f.Fuzz(func(t *testing.T, size, ps int64) {
		if size < 0 || ps <= 0 || size/ps > 100000 {
			return
		}
		parts := plan(size, ps)
		var next int64
		for i, p := range parts {
			if p.No != i || p.Offset != next || p.Length > ps || (i < len(parts)-1 && p.Length != ps) || (size > 0 && p.Length == 0) {
				t.Fatalf("size %d, part size %d: part %+v after %d", size, ps, p, next)
			}
			next += p.Length
		}
		if next != size {
			t.Fatalf("size %d: the parts cover %d", size, next)
		}
		sums := make([][]byte, len(parts))
		for i := range sums {
			s := sha256.Sum256([]byte{byte(i)})
			sums[i] = s[:]
		}
		if _, err := digest(ps, size, sums); err != nil {
			t.Fatal(err)
		}
		if _, err := digest(ps, size, sums[1:]); err == nil {
			t.Fatal("a digest without the first part")
		}
	})
}

func TestDigestDiffers(t *testing.T) {
	a := sha256.Sum256([]byte("a"))
	b := sha256.Sum256([]byte("b"))
	d1, _ := digest(4, 8, [][]byte{a[:], b[:]})
	d2, _ := digest(4, 8, [][]byte{b[:], a[:]})
	d3, _ := digest(8, 8, [][]byte{a[:]})
	if d1 == d2 || d1 == d3 || len(d1) != 64 {
		t.Errorf("%s %s %s", d1, d2, d3)
	}
}
