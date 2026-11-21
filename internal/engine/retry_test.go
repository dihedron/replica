package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/dihedron/veho/pkg/plugin"
)

func TestFail(t *testing.T) {
	for _, c := range []struct {
		err  error
		want plugin.Class
	}{
		{errors.New("connection refused"), plugin.Transient},
		{fmt.Errorf("x: %w", ErrFenced), plugin.Transient},
		{drv(errors.New("odd")), plugin.Unknown},
		{drv(fmt.Errorf("read: %w", context.Canceled)), plugin.Transient},
		{drv(plugin.Wrap(errors.New("403"), plugin.Permanent)), plugin.Permanent},
		{fmt.Errorf("step: %w", drv(plugin.Wrap(errors.New("gone"), plugin.ResumeInvalid))), plugin.ResumeInvalid},
	} {
		if r := fail(c.err); r.Class != c.want || !r.failed() {
			t.Errorf("%v: %v", c.err, r.Class)
		}
	}
}

func TestSanitize(t *testing.T) {
	got := sanitize(`GET https://user:pw@s3.example/b/k?X-Amz-Signature=abc&X-Amz-Credential=AKIA: 403 ` + strings.Repeat("x", 2000))
	if strings.Contains(got, "abc") || strings.Contains(got, "AKIA") || strings.Contains(got, "pw") || len(got) > 1100 {
		t.Errorf("%q", got[:200])
	}
}

// FR-12: exponential, capped, jittered by ±20 %, the same on replay.
func TestBackoff(t *testing.T) {
	const initial, ceiling = 2 * time.Second, 15 * time.Minute
	for n := range 20 {
		d := backoff("t1", n, initial, ceiling)
		base := min(initial<<n, ceiling)
		if n > 12 {
			base = ceiling
		}
		if d < base*8/10 || d > base*12/10 {
			t.Errorf("attempt %d: %v, want %v ± 20 %%", n, d, base)
		}
		if d != backoff("t1", n, initial, ceiling) {
			t.Errorf("attempt %d: not deterministic", n)
		}
	}
	if backoff("t1", 3, initial, ceiling) == backoff("t2", 3, initial, ceiling) {
		t.Error("two transfers wait the same")
	}
}
