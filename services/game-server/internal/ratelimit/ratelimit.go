// Package ratelimit is a token bucket: up to Capacity actions at once, refilled at Rate per
// second. The per-player guess limit lives here, in the game server, because it needs game
// context (design doc, D12); the gateway's coarse limit_req is a leaky bucket.
package ratelimit

import "time"

// Bucket is a token bucket. It has no lock: exactly one goroutine owns it (the room actor for
// guesses, a connection's read loop for its flood guard). Time is passed in, so tests don't
// sleep.
type Bucket struct {
	capacity float64
	rate     float64 // tokens per second
	tokens   float64
	last     time.Time
}

// New returns a full bucket.
func New(capacity int, perSecond float64) Bucket {
	return Bucket{capacity: float64(capacity), rate: perSecond, tokens: float64(capacity)}
}

// Allow takes one token if there is one.
func (b *Bucket) Allow(now time.Time) bool {
	if !b.last.IsZero() && now.After(b.last) {
		b.tokens = min(b.capacity, b.tokens+now.Sub(b.last).Seconds()*b.rate)
	}
	if b.last.IsZero() || now.After(b.last) {
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
