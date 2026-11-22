package policy

import (
	"math/rand/v2"
	"testing"

	"github.com/dihedron/veho/internal/topology"
)

func pos(region, az string) topology.Position {
	return topology.Position{"region": region, "az": az}
}

func tr(s State, region, az string) Transfer { return Transfer{State: s, Position: pos(region, az)} }

func TestEvaluateExamples(t *testing.T) {
	spread3 := Completion{MinCopies: 5, Spread: []Statement{{Level: "region", MinDistinct: 3}}}
	tests := []struct {
		name string
		c    Completion
		ts   []Transfer
		want Outcome
	}{
		{"5 over 3 regions: 2+2+1 is done", spread3, []Transfer{
			tr(Succeeded, "A", "a1"), tr(Succeeded, "A", "a2"), tr(Succeeded, "B", "b1"), tr(Succeeded, "B", "b2"), tr(Succeeded, "C", "c1"), tr(Pending, "C", "c2")}, Satisfied},
		{"5 over 3 regions: 3+2+0 is not done, and still open", spread3, []Transfer{
			tr(Succeeded, "A", "a1"), tr(Succeeded, "A", "a2"), tr(Succeeded, "A", "a3"), tr(Succeeded, "B", "b1"), tr(Succeeded, "B", "b2"), tr(Copying, "C", "c1")}, Open},
		{"5 over 3 regions: 3+2+0 with C failed is unsatisfiable", spread3, []Transfer{
			tr(Succeeded, "A", "a1"), tr(Succeeded, "A", "a2"), tr(Succeeded, "A", "a3"), tr(Succeeded, "B", "b1"), tr(Succeeded, "B", "b2"), tr(Failed, "C", "c1")}, Unsatisfiable},
		{"7 of 9: seven done", Completion{MinCopies: 7}, nine(7, 0, 2), Satisfied},
		{"7 of 9: three failed", Completion{MinCopies: 7}, nine(4, 3, 2), Unsatisfiable},
		{"7 of 9: two failed, still reachable", Completion{MinCopies: 7}, nine(4, 2, 3), Open},
		{"default is all: one failed", Completion{}, nine(8, 1, 0), Unsatisfiable},
		{"default is all: all done", Completion{}, nine(9, 0, 0), Satisfied},
		{"5 azs across 3 regions", Completion{MinCopies: 5, Spread: []Statement{{Level: "region", MinDistinct: 3}, {Level: "az", MinDistinct: 5}}}, []Transfer{
			tr(Succeeded, "A", "a1"), tr(Succeeded, "A", "a1"), tr(Succeeded, "B", "b1"), tr(Succeeded, "C", "c1"), tr(Succeeded, "C", "c2"), tr(Pending, "B", "b2")}, Open},
		{"5 azs across 3 regions: all copies in, only 4 azs", Completion{MinCopies: 5, Spread: []Statement{{Level: "az", MinDistinct: 5}}}, []Transfer{
			tr(Succeeded, "A", "a1"), tr(Succeeded, "A", "a1"), tr(Succeeded, "B", "b1"), tr(Succeeded, "C", "c1"), tr(Succeeded, "C", "c2")}, Unsatisfiable},
	}
	for _, tc := range tests {
		got, err := Evaluate(Policy{Completion: tc.c}, tc.ts)
		if err != nil || got.Outcome != tc.want {
			t.Errorf("%s: got %v (%v), want %v", tc.name, got.Outcome, err, tc.want)
		}
	}
}

// nine builds 9 transfers, 3 per region: done succeeded, failed failed, the
// rest pending, assigned in order so that the regions fill evenly.
func nine(done, failed, pending int) []Transfer {
	var ts []Transfer
	regions := []string{"A", "B", "C"}
	azs := []string{"1", "2", "3"}
	add := func(s State, n int) {
		for range n {
			i := len(ts)
			ts = append(ts, tr(s, regions[i%3], regions[i%3]+azs[i/3]))
		}
	}
	add(Succeeded, done)
	add(Failed, failed)
	add(Pending, pending)
	return ts
}

func TestEvaluateMilestones(t *testing.T) {
	v, _ := Evaluate(Policy{Completion: Completion{MinCopies: 1}}, []Transfer{tr(Succeeded, "A", "a1"), tr(Copying, "B", "b1")})
	if v.Outcome != Satisfied || v.Complete || v.Settled || v.Copies != 1 {
		t.Errorf("one done, one copying: %+v", v)
	}
	v, _ = Evaluate(Policy{Completion: Completion{MinCopies: 1}}, []Transfer{tr(Succeeded, "A", "a1"), tr(Failed, "B", "b1")})
	if v.Outcome != Satisfied || v.Complete || !v.Settled {
		t.Errorf("one done, one failed: settled but not complete: %+v", v)
	}
	v, _ = Evaluate(Policy{}, []Transfer{tr(Succeeded, "A", "a1"), tr(Succeeded, "B", "b1")})
	if !v.Complete || !v.Settled {
		t.Errorf("all done: %+v", v)
	}
	if v.ByLevel["region"]["A"] != 1 || v.ByLevel["az"]["b1"] != 1 {
		t.Errorf("ByLevel: %+v", v.ByLevel)
	}
}

func TestEvaluateRejectsUnknownState(t *testing.T) {
	if _, err := Evaluate(Policy{}, []Transfer{{State: "BOGUS"}}); err == nil {
		t.Error("an unknown state must be an error, not silently classified")
	}
}

// oracle decides the verdict by brute force: it tries every way the
// transfers that are not terminal can end.
func oracle(c Completion, ts []Transfer) Outcome {
	var open []int
	for i, x := range ts {
		if !x.State.Terminal() {
			open = append(open, i)
		}
	}
	satisfied := func(succ []bool) bool {
		n := 0
		seen := map[string]map[string]bool{}
		for i, ok := range succ {
			if !ok {
				continue
			}
			n++
			for l, id := range ts[i].Position {
				if seen[l] == nil {
					seen[l] = map[string]bool{}
				}
				seen[l][id] = true
			}
		}
		min := c.MinCopies
		if min == 0 {
			min = len(ts)
		}
		if n < min {
			return false
		}
		for _, s := range c.Spread {
			if len(seen[s.Level]) < s.MinDistinct {
				return false
			}
		}
		return true
	}
	base := make([]bool, len(ts))
	for i, x := range ts {
		base[i] = x.State == Succeeded
	}
	if satisfied(base) {
		return Satisfied
	}
	for mask := 0; mask < 1<<len(open); mask++ {
		succ := append([]bool(nil), base...)
		for j, i := range open {
			if mask&(1<<j) != 0 {
				succ[i] = true
			}
		}
		if satisfied(succ) {
			return Open
		}
	}
	return Unsatisfiable
}

var states = []State{Pending, Copying, Verifying, RetryWait, Succeeded, Failed, Cancelled}

func randomCase(r *rand.Rand) (Completion, []Transfer) {
	n := 2 + r.IntN(7)
	ts := make([]Transfer, n)
	for i := range ts {
		region := string(rune('A' + r.IntN(3)))
		ts[i] = tr(states[r.IntN(len(states))], region, region+string(rune('1'+r.IntN(3))))
	}
	c := Completion{MinCopies: r.IntN(n + 1)}
	if r.IntN(2) == 0 {
		c.Spread = append(c.Spread, Statement{Level: "region", MinDistinct: 1 + r.IntN(3)})
	}
	if r.IntN(2) == 0 {
		c.Spread = append(c.Spread, Statement{Level: "az", MinDistinct: 1 + r.IntN(5)})
	}
	return c, ts
}

func TestEvaluateAgainstOracle(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for i := 0; i < 20000; i++ {
		c, ts := randomCase(r)
		got, err := Evaluate(Policy{Completion: c}, ts)
		if err != nil {
			t.Fatal(err)
		}
		if want := oracle(c, ts); got.Outcome != want {
			t.Fatalf("case %d: completion %+v transfers %+v: got %v, oracle %v", i, c, ts, got.Outcome, want)
		}
	}
}

// A transfer that succeeds never turns a satisfied verdict into another one
// (DP-8), and never turns an unsatisfiable one into an open one.
func TestEvaluateMonotonic(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))
	for i := 0; i < 20000; i++ {
		c, ts := randomCase(r)
		before, _ := Evaluate(Policy{Completion: c}, ts)
		j := r.IntN(len(ts))
		if ts[j].State.Terminal() {
			continue
		}
		ts[j].State = Succeeded
		after, _ := Evaluate(Policy{Completion: c}, ts)
		if before.Outcome == Satisfied && after.Outcome != Satisfied {
			t.Fatalf("case %d: satisfied became %v", i, after.Outcome)
		}
		if before.Outcome == Unsatisfiable && after.Outcome != Unsatisfiable {
			t.Fatalf("case %d: unsatisfiable became %v", i, after.Outcome)
		}
	}
}

func FuzzEvaluate(f *testing.F) {
	f.Add([]byte{3, 4, 0, 0, 4, 1, 1, 5, 2, 2, 2, 1, 1})
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) < 3 {
			return
		}
		n := 2 + int(data[0])%7
		c := Completion{MinCopies: int(data[1]) % (n + 1)}
		if data[2]&1 != 0 {
			c.Spread = append(c.Spread, Statement{Level: "region", MinDistinct: 1 + int(data[2]>>1)%3})
		}
		if data[2]&0x80 != 0 {
			c.Spread = append(c.Spread, Statement{Level: "az", MinDistinct: 1 + int(data[2]>>2)%5})
		}
		rest := data[3:]
		ts := make([]Transfer, n)
		for i := range ts {
			var s, rg, az byte
			if len(rest) > 0 {
				s, rest = rest[0], rest[1:]
			}
			if len(rest) > 0 {
				rg, rest = rest[0], rest[1:]
			}
			if len(rest) > 0 {
				az, rest = rest[0], rest[1:]
			}
			ts[i] = tr(states[int(s)%len(states)], string(rune('A'+rg%3)), string(rune('a'+az%5)))
		}
		got, err := Evaluate(Policy{Completion: c}, ts)
		if err != nil {
			t.Fatal(err)
		}
		if want := oracle(c, ts); got.Outcome != want {
			t.Fatalf("completion %+v transfers %+v: got %v, oracle %v", c, ts, got.Outcome, want)
		}
	})
}
