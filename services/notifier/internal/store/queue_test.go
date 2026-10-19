package store_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Bowenn/gap-project_private/services/notifier/internal/pgtest"
	"github.com/Bowenn/gap-project_private/services/notifier/internal/store"
)

func TestOldestDueAge(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()

	age := func() float64 {
		t.Helper()
		a, err := s.OldestDueAge(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	// due moves a delivery's next attempt into the past (or the future, for a negative ago).
	due := func(id uuid.UUID, ago time.Duration) {
		t.Helper()
		if _, err := s.Pool.Exec(ctx, "UPDATE deliveries SET next_attempt_at = now() - make_interval(secs => $2) WHERE id = $1", id, ago.Seconds()); err != nil {
			t.Fatal(err)
		}
	}

	if a := age(); a != 0 {
		t.Fatalf("empty queue: age = %v, want 0", a)
	}

	live, _ := s.CreateEndpoint(ctx, endpoint("u_q1", "sale.completed"), time.Now())
	later := delivery(live, "evt-later")
	waiting := delivery(live, "evt-waiting")
	if _, err := s.InsertDeliveries(ctx, []store.Delivery{later, waiting}); err != nil {
		t.Fatal(err)
	}
	due(later.ID, -time.Hour) // a retry scheduled for later isn't waiting yet
	due(waiting.ID, 2*time.Minute)
	if a := age(); a < 119 || a > 180 {
		t.Fatalf("one due delivery, 2 min old: age = %v", a)
	}

	// Deliveries Claim never takes don't count: a deleted or disabled endpoint's, and finished ones.
	deleted, _ := s.CreateEndpoint(ctx, endpoint("u_q2", "sale.completed"), time.Now())
	disabled, _ := s.CreateEndpoint(ctx, endpoint("u_q3", "sale.completed"), time.Now())
	dead := delivery(live, "evt-dead")
	others := []store.Delivery{delivery(deleted, "evt-deleted"), delivery(disabled, "evt-disabled"), dead}
	if _, err := s.InsertDeliveries(ctx, others); err != nil {
		t.Fatal(err)
	}
	for _, d := range others {
		due(d.ID, time.Hour)
	}
	if err := s.DeleteEndpoint(ctx, "u_q2", deleted.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RevokeCredentials(ctx, "u_q3", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, "UPDATE deliveries SET status = 'DEAD', finished_at = now() WHERE id = $1", dead.ID); err != nil {
		t.Fatal(err)
	}
	if a := age(); a > 180 {
		t.Fatalf("age = %v, counted a delivery no worker would take", a)
	}

	// A claimed delivery is leased into the future, so the queue is empty again.
	if _, err := s.Claim(ctx, 10, time.Minute); err != nil {
		t.Fatal(err)
	}
	if a := age(); a != 0 {
		t.Fatalf("after Claim: age = %v, want 0", a)
	}
}
