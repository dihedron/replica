// Package ratelimit keeps the token buckets of the API's rate limits
// (.specs/veho-api-server.md §5.2). Copied from ligo 9f11da9
// (internal/ratelimit): the Limiter as is, plus Empty; ligo's Logins are
// not needed.
package ratelimit

import (
	"sync"
	"time"
)

// Rate is a token bucket's refill per second and size.
type Rate struct{ PerSecond, Burst float64 }

type bucket struct {
	tokens float64
	at     time.Time
	rate   Rate
}

// DefaultMaxKeys is the number of keys a Limiter keeps when Max is 0.
const DefaultMaxKeys = 1_000_000

// Limiter keeps a token bucket per key, up to Max keys; the zero value is
// ready to use. A full bucket is the same as none, so full buckets are
// pruned to make room; while there is none, a new key is refused.
type Limiter struct {
	Max int // default DefaultMaxKeys

	mu      sync.Mutex
	buckets map[string]*bucket
	pruned  time.Time
}

// refill returns key's bucket, refilled up to now, or nil if there is no
// room for a new key; l.mu is held.
func (l *Limiter) refill(key string, r Rate) *bucket {
	now := time.Now()
	if l.buckets == nil {
		l.buckets = map[string]*bucket{}
	}
	b, ok := l.buckets[key]
	if !ok {
		if !l.room(now) {
			return nil
		}
		b = &bucket{tokens: r.Burst, at: now}
		l.buckets[key] = b
	}
	b.tokens = min(r.Burst, b.tokens+now.Sub(b.at).Seconds()*r.PerSecond)
	b.at, b.rate = now, r
	return b
}

// room reports whether a new key fits, pruning full buckets at most once a
// second when the map is full, so a flood of keys cannot make every request
// scan it.
func (l *Limiter) room(now time.Time) bool {
	limit := l.Max
	if limit <= 0 {
		limit = DefaultMaxKeys
	}
	if len(l.buckets) < limit {
		return true
	}
	if now.Sub(l.pruned) < time.Second {
		return false
	}
	l.pruned = now
	for k, b := range l.buckets {
		if b.tokens+now.Sub(b.at).Seconds()*b.rate.PerSecond >= b.rate.Burst {
			delete(l.buckets, k)
		}
	}
	return len(l.buckets) < limit
}

// Allow takes a token from key's bucket, if one is left.
func (l *Limiter) Allow(key string, r Rate) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.refill(key, r)
	if b == nil || b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// Empty reports whether key's bucket has no token left, without taking one:
// for limits that only failures spend.
func (l *Limiter) Empty(key string, r Rate) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[key]
	if !ok {
		return false
	}
	return min(r.Burst, b.tokens+time.Since(b.at).Seconds()*r.PerSecond) < 1
}
