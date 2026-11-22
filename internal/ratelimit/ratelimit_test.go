package ratelimit

import "testing"

func TestLimiter(t *testing.T) {
	var l Limiter
	r := Rate{PerSecond: 0.001, Burst: 3}
	for i := range 3 {
		if !l.Allow("a", r) {
			t.Fatalf("token %d refused", i)
		}
	}
	if l.Allow("a", r) || !l.Empty("a", r) {
		t.Error("a fourth token, or not empty")
	}
	if !l.Allow("b", r) || l.Empty("c", r) {
		t.Error("another key is limited")
	}
}

func TestMaxKeys(t *testing.T) {
	l := Limiter{Max: 2}
	r := Rate{PerSecond: 0, Burst: 1}
	l.Allow("a", r)
	l.Allow("b", r)
	if l.Allow("c", r) {
		t.Error("a third key while two buckets are spent")
	}
}
