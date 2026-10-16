package ratelimit

import (
	"testing"
	"time"
)

func TestBucket(t *testing.T) {
	t0 := time.Unix(1000, 0)
	b := New(5, 1)
	for i := range 5 {
		if !b.Allow(t0) {
			t.Fatalf("burst: request %d denied", i+1)
		}
	}
	if b.Allow(t0) {
		t.Fatal("6th request in the same instant allowed")
	}
	if b.Allow(t0.Add(500 * time.Millisecond)) {
		t.Fatal("allowed before a whole token refilled")
	}
	if !b.Allow(t0.Add(time.Second)) {
		t.Fatal("denied after a token refilled")
	}
	// A long idle time refills to capacity, not beyond.
	later := t0.Add(time.Hour)
	for i := range 5 {
		if !b.Allow(later) {
			t.Fatalf("after idle: request %d denied", i+1)
		}
	}
	if b.Allow(later) {
		t.Fatal("bucket grew past its capacity")
	}
	// A clock that goes backwards never adds tokens.
	if b.Allow(later.Add(-time.Minute)) {
		t.Fatal("going back in time refilled the bucket")
	}
}
