package store_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Bowenn/doodle-arena/services/notifier/internal/pgtest"
	"github.com/Bowenn/doodle-arena/services/notifier/internal/store"
)

func endpoint(owner string, types ...string) store.Endpoint {
	return store.Endpoint{
		ID: uuid.New(), OwnerID: owner, APIKeyID: ptr(uuid.New()), URL: "https://example.com/hook",
		EventTypes: types, SecretCiphertext: []byte("ct"), SecretNonce: []byte("nonce"),
	}
}

func delivery(e store.Endpoint, eventID string) store.Delivery {
	return store.Delivery{ID: uuid.New(), EndpointID: e.ID, SourceEventID: eventID, EventType: "sale.completed", Payload: `{"id":"` + eventID + `"}`}
}

func TestStore(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()

	t.Run("endpoints belong to their owner", func(t *testing.T) {
		e, err := s.CreateEndpoint(ctx, endpoint("u_ada", "sale.completed"), time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.GetEndpoint(ctx, "u_bob", e.ID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("other owner: err = %v", err)
		}
		list, _ := s.ListEndpoints(ctx, "u_ada")
		if len(list) != 1 || list[0].ID != e.ID {
			t.Fatalf("list = %v", list)
		}
		if err := s.DeleteEndpoint(ctx, "u_bob", e.ID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("delete by other owner: err = %v", err)
		}
	})

	t.Run("at most ten endpoints per owner, even when registered concurrently", func(t *testing.T) {
		var wg sync.WaitGroup
		errs := make(chan error, 15)
		for range 15 {
			wg.Go(func() {
				_, err := s.CreateEndpoint(ctx, endpoint("u_many", "sale.completed"), time.Now())
				errs <- err
			})
		}
		wg.Wait()
		close(errs)
		tooMany := 0
		for err := range errs {
			if errors.Is(err, store.ErrTooManyEndpoints) {
				tooMany++
			} else if err != nil {
				t.Fatal(err)
			}
		}
		list, _ := s.ListEndpoints(ctx, "u_many")
		if len(list) != store.MaxEndpointsPerOwner || tooMany != 5 {
			t.Fatalf("live = %d, refused = %d", len(list), tooMany)
		}
	})

	t.Run("subscribers are the owners' live endpoints of that type", func(t *testing.T) {
		sales, _ := s.CreateEndpoint(ctx, endpoint("u_sub1", "sale.completed"), time.Now())
		both, _ := s.CreateEndpoint(ctx, endpoint("u_sub2", "sale.completed", "coins.credited"), time.Now())
		_, _ = s.CreateEndpoint(ctx, endpoint("u_sub3", "sale.completed"), time.Now()) // not an owner here
		deleted, _ := s.CreateEndpoint(ctx, endpoint("u_sub1", "sale.completed"), time.Now())
		_ = s.DeleteEndpoint(ctx, "u_sub1", deleted.ID)

		got, err := s.Subscribers(ctx, []string{"u_sub1", "u_sub2"}, "sale.completed")
		if err != nil {
			t.Fatal(err)
		}
		ids := map[uuid.UUID]bool{}
		for _, e := range got {
			ids[e.ID] = true
		}
		if len(got) != 2 || !ids[sales.ID] || !ids[both.ID] {
			t.Fatalf("subscribers = %v", got)
		}
		credits, _ := s.Subscribers(ctx, []string{"u_sub1", "u_sub2"}, "coins.credited")
		if len(credits) != 1 || credits[0].ID != both.ID {
			t.Fatalf("coins.credited subscribers = %v", credits)
		}
	})

	t.Run("a redelivered event queues nothing new", func(t *testing.T) {
		e, _ := s.CreateEndpoint(ctx, endpoint("u_dup", "sale.completed"), time.Now())
		n1, err := s.InsertDeliveries(ctx, []store.Delivery{delivery(e, "evt-1")})
		if err != nil {
			t.Fatal(err)
		}
		n2, _ := s.InsertDeliveries(ctx, []store.Delivery{delivery(e, "evt-1")})
		if n1 != 1 || n2 != 0 {
			t.Fatalf("inserted %d then %d", n1, n2)
		}
	})

	t.Run("claims skip each other and come back after the lease", func(t *testing.T) {
		e, _ := s.CreateEndpoint(ctx, endpoint("u_claim", "sale.completed"), time.Now())
		_, _ = s.InsertDeliveries(ctx, []store.Delivery{delivery(e, "evt-c1"), delivery(e, "evt-c2"), delivery(e, "evt-c3")})

		first, err := s.Claim(ctx, 2, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		second, _ := s.Claim(ctx, 10, time.Second)
		if len(first)+len(second) < 3 {
			t.Fatalf("claimed %d + %d", len(first), len(second))
		}
		seen := map[uuid.UUID]bool{}
		for _, c := range append(first, second...) {
			if seen[c.ID] {
				t.Fatalf("%s claimed twice", c.ID)
			}
			seen[c.ID] = true
			if c.URL != "https://example.com/hook" {
				t.Fatalf("claimed without its endpoint: %+v", c)
			}
		}
		if again, _ := s.Claim(ctx, 10, time.Second); len(again) != 0 {
			t.Fatalf("claimed %d during the lease", len(again))
		}
		time.Sleep(1100 * time.Millisecond) // a worker that died: its lease runs out
		again, _ := s.Claim(ctx, 10, time.Second)
		if len(again) < 3 {
			t.Fatalf("after the lease: claimed %d", len(again))
		}
	})

	t.Run("attempts move a delivery on and are logged", func(t *testing.T) {
		e, _ := s.CreateEndpoint(ctx, endpoint("u_att", "sale.completed"), time.Now())
		d := delivery(e, "evt-a")
		_, _ = s.InsertDeliveries(ctx, []store.Delivery{d})

		retryAt := time.Now().Add(time.Hour)
		if err := s.RecordAttempt(ctx, d.ID, 1, store.AttemptResult{StatusCode: 500, Error: "HTTP 500", Duration: 12 * time.Millisecond, Outcome: store.Retry, RetryAt: retryAt}); err != nil {
			t.Fatal(err)
		}
		if err := s.RecordAttempt(ctx, d.ID, 2, store.AttemptResult{StatusCode: 0, Error: "timeout", Duration: 5 * time.Second, Outcome: store.Dead}); err != nil {
			t.Fatal(err)
		}
		if err := s.RecordAttempt(ctx, d.ID, 3, store.AttemptResult{Outcome: store.Succeeded}); !errors.Is(err, store.ErrNotClaimed) {
			t.Fatalf("attempt on a DEAD delivery: err = %v", err)
		}
		ds, err := s.ListDeliveries(ctx, "u_att", e.ID, uuid.Nil, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(ds) != 1 || ds[0].Status != "DEAD" || ds[0].Attempts != 2 || len(ds[0].AttemptLog) != 2 || ds[0].AttemptLog[1].Error == nil {
			t.Fatalf("deliveries = %+v", ds)
		}

		dead, _ := s.UnpublishedDead(ctx, 10)
		found := false
		for _, x := range dead {
			found = found || x.ID == d.ID
		}
		if !found {
			t.Fatal("DEAD delivery not waiting for the dead-letter queue")
		}
		_ = s.MarkDeadPublished(ctx, []uuid.UUID{d.ID})
		dead, _ = s.UnpublishedDead(ctx, 10)
		for _, x := range dead {
			if x.ID == d.ID {
				t.Fatal("still unpublished after MarkDeadPublished")
			}
		}
		if n, _ := s.DeadSince(ctx, time.Now().Add(-time.Hour)); n < 1 {
			t.Fatalf("DeadSince = %d", n)
		}
	})

	t.Run("a dead test ping stays out of the dead-letter queue and its gauge", func(t *testing.T) {
		before, _ := s.DeadSince(ctx, time.Now().Add(-time.Hour))
		e, _ := s.CreateEndpoint(ctx, endpoint("u_ping", "sale.completed"), time.Now())
		d := delivery(e, "ping_1")
		d.EventType = "ping"
		_, _ = s.InsertDeliveries(ctx, []store.Delivery{d})
		if err := s.RecordAttempt(ctx, d.ID, 1, store.AttemptResult{StatusCode: 500, Error: "HTTP 500", Outcome: store.Dead}); err != nil {
			t.Fatal(err)
		}
		dead, _ := s.UnpublishedDead(ctx, 100)
		for _, x := range dead {
			if x.ID == d.ID {
				t.Fatal("a dead ping is waiting for the dead-letter queue")
			}
		}
		if n, _ := s.DeadSince(ctx, time.Now().Add(-time.Hour)); n != before {
			t.Fatalf("DeadSince = %d, want %d (pings not counted)", n, before)
		}
	})

	t.Run("deleting an endpoint cancels what is pending", func(t *testing.T) {
		e, _ := s.CreateEndpoint(ctx, endpoint("u_del", "sale.completed"), time.Now())
		_, _ = s.InsertDeliveries(ctx, []store.Delivery{delivery(e, "evt-d")})
		if err := s.DeleteEndpoint(ctx, "u_del", e.ID); err != nil {
			t.Fatal(err)
		}
		var status string
		_ = s.Pool.QueryRow(ctx, "SELECT status FROM deliveries WHERE endpoint_id = $1", e.ID).Scan(&status)
		if status != "CANCELLED" {
			t.Fatalf("status = %s", status)
		}
	})

	t.Run("delivery log pages newest first", func(t *testing.T) {
		e, _ := s.CreateEndpoint(ctx, endpoint("u_page", "sale.completed"), time.Now())
		for i := range 5 {
			_, _ = s.InsertDeliveries(ctx, []store.Delivery{delivery(e, fmt.Sprintf("evt-p%d", i))})
		}
		page1, _ := s.ListDeliveries(ctx, "u_page", e.ID, uuid.Nil, 3)
		page2, _ := s.ListDeliveries(ctx, "u_page", e.ID, page1[2].ID, 3)
		if len(page1) != 3 || len(page2) != 2 || page1[0].SourceEventID != "evt-p4" || page2[1].SourceEventID != "evt-p0" {
			t.Fatalf("pages: %v / %v", ids(page1), ids(page2))
		}
		if _, err := s.ListDeliveries(context.Background(), "u_other", e.ID, uuid.Nil, 3); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("other owner's log: err = %v", err)
		}
	})
}

func ids(ds []store.Delivery) []string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = d.SourceEventID
	}
	return out
}

func ptr[T any](v T) *T { return &v }
