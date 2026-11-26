package api

import (
	"context"
	"fmt"
	"testing"

	"github.com/dihedron/veho/internal/audit"
)

// A flood of addresses is bounded, and every refusal is still counted.
func TestRefusalsBounded(t *testing.T) {
	var r refusals
	for i := range maxRefusals + 500 {
		r.add(refusal{address: fmt.Sprint(i), kind: "jwt", reason: "x"})
	}
	rec := &audit.Recorder{}
	r.flush(context.Background(), rec)
	total := 0
	for _, e := range rec.Events() {
		total += e.Details.(refusalDetails).Count
	}
	if n := len(rec.Events()); n != maxRefusals+1 || total != maxRefusals+500 {
		t.Errorf("%d events for %d refusals", n, total)
	}
	r.flush(context.Background(), rec)
	if n := len(rec.Events()); n != maxRefusals+1 {
		t.Errorf("a second flush emitted again: %d", n)
	}
}
