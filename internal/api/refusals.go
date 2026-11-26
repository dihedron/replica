package api

import (
	"context"
	"sync"
	"time"

	"github.com/dihedron/veho/internal/audit"
)

// refusalsEvery is how often the refused credentials are audited, summed so
// that a flood does not fill the log (a variable for the tests).
var refusalsEvery = time.Minute

// maxRefusals bounds the refusals kept between two flushes; the rest are
// counted together.
const maxRefusals = 10000

// refusal is what refused credentials are summed by. principal is set only
// when the identity was verified.
type refusal struct{ address, kind, principal, reason string }

// refusalDetails are the details of an AUTH_REFUSED event (T-32).
type refusalDetails struct {
	Address string `json:"address"`
	Kind    string `json:"kind"`
	Reason  string `json:"reason"`
	Count   int    `json:"count"`
}

// refusals sums the refused credentials.
type refusals struct {
	mu     sync.Mutex
	counts map[refusal]int
}

func (r *refusals) add(k refusal) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.counts == nil {
		r.counts = map[refusal]int{}
	}
	if _, ok := r.counts[k]; !ok && len(r.counts) >= maxRefusals {
		k = refusal{address: "others", kind: "others", reason: "too many to list"}
	}
	r.counts[k]++
}

// flush emits the sums and starts again.
func (r *refusals) flush(ctx context.Context, a audit.Auditor) {
	r.mu.Lock()
	counts := r.counts
	r.counts = nil
	r.mu.Unlock()
	for k, n := range counts {
		a.Emit(ctx, audit.Event{Action: "AUTH_REFUSED", Outcome: audit.Denied, Principal: k.principal, ResourceType: "credential",
			Details: refusalDetails{Address: k.address, Kind: k.kind, Reason: k.reason, Count: n}})
	}
}

// run flushes every refusalsEvery, and at the end of ctx.
func (r *refusals) run(ctx context.Context, a audit.Auditor) {
	t := time.NewTicker(refusalsEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			r.flush(context.WithoutCancel(ctx), a)
			return
		case <-t.C:
			r.flush(ctx, a)
		}
	}
}
