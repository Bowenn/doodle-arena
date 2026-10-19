package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Bowenn/gap-project_private/services/notifier/internal/pgtest"
	"github.com/Bowenn/gap-project_private/services/notifier/internal/store"
)

// Task 4.R2 (ADR 0015): a CredentialsRevoked disables the owner's endpoints once.
func TestRevokeCredentials(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	owner, other := "u_rev_"+uuid.NewString(), "u_rev_other_"+uuid.NewString()
	a, _ := s.CreateEndpoint(ctx, endpoint(owner, "sale.completed"), time.Now())
	b, _ := s.CreateEndpoint(ctx, endpoint(owner, "coins.credited"), time.Now())
	untouched, _ := s.CreateEndpoint(ctx, endpoint(other, "sale.completed"), time.Now())
	pending := delivery(a, "evt-pending-"+uuid.NewString())
	theirs := delivery(untouched, "evt-theirs-"+uuid.NewString())
	_, _ = s.InsertDeliveries(ctx, []store.Delivery{pending, theirs})
	revokedAt := time.Now().Add(-time.Second).UTC().Truncate(time.Microsecond)

	n, fresh, err := s.RevokeCredentials(ctx, owner, revokedAt)
	if err != nil || n != 2 || !fresh {
		t.Fatalf("RevokeCredentials = %d, %v, %v; want 2 endpoints, a new revocation", n, fresh, err)
	}
	for _, id := range []uuid.UUID{a.ID, b.ID} {
		e, _ := s.GetEndpoint(ctx, owner, id)
		if e.DisabledAt == nil || e.DisabledReason == nil || *e.DisabledReason != store.DisabledCredentialsRevoked {
			t.Fatalf("endpoint %s not disabled: %+v", id, e)
		}
	}
	if e, _ := s.GetEndpoint(ctx, other, untouched.ID); e.DisabledAt != nil {
		t.Fatal("another user's endpoint was disabled")
	}
	ds, _ := s.ListDeliveries(ctx, owner, a.ID, uuid.Nil, 10)
	if len(ds) != 1 || ds[0].Status != "CANCELLED" {
		t.Fatalf("pending delivery not cancelled: %+v", ds)
	}
	if at, ok, _ := s.RevokedAt(ctx, owner); !ok || !at.Equal(revokedAt) {
		t.Fatalf("watermark = %v, %v; want %v", at, ok, revokedAt)
	}

	t.Run("disabled endpoints get no new deliveries and are never claimed", func(t *testing.T) {
		subs, _ := s.Subscribers(ctx, []string{owner, other}, "sale.completed")
		if len(subs) != 1 || subs[0].ID != untouched.ID {
			t.Fatalf("subscribers = %+v, want only the other user's endpoint", subs)
		}
		// A delivery queued before the revocation but still pending would be cancelled; one that
		// somehow exists must not be claimed either.
		late := delivery(b, "evt-late-"+uuid.NewString())
		_, _ = s.InsertDeliveries(ctx, []store.Delivery{late})
		claimed, _ := s.Claim(ctx, 100, time.Minute)
		for _, c := range claimed {
			if c.EndpointID == a.ID || c.EndpointID == b.ID {
				t.Fatalf("claimed a delivery of a disabled endpoint: %+v", c)
			}
		}
	})

	t.Run("enabling rotates the secret; a replayed event doesn't disable it again", func(t *testing.T) {
		e, err := s.EnableEndpoint(ctx, owner, a.ID, []byte("new-ct"), []byte("new-nonce"), time.Now())
		if err != nil || e.DisabledAt != nil || string(e.SecretCiphertext) != "new-ct" {
			t.Fatalf("EnableEndpoint = %+v, %v", e, err)
		}
		n, fresh, err := s.RevokeCredentials(ctx, owner, revokedAt) // the same event again
		if err != nil || n != 0 || fresh {
			t.Fatalf("replay = %d, %v, %v; want nothing", n, fresh, err)
		}
		if e, _ := s.GetEndpoint(ctx, owner, a.ID); e.DisabledAt != nil {
			t.Fatal("a replayed revocation disabled an endpoint the owner enabled")
		}
		if _, _, err := s.RevokeCredentials(ctx, owner, revokedAt.Add(-time.Hour)); err != nil {
			t.Fatal(err)
		}
		if at, _, _ := s.RevokedAt(ctx, owner); !at.Equal(revokedAt) {
			t.Fatalf("an older revocation lowered the watermark to %v", at)
		}
	})

	t.Run("enable refuses active and unknown endpoints", func(t *testing.T) {
		if _, err := s.EnableEndpoint(ctx, owner, a.ID, []byte("x"), []byte("y"), time.Now()); !errors.Is(err, store.ErrEndpointActive) {
			t.Fatalf("enable an active endpoint: %v", err)
		}
		if _, err := s.EnableEndpoint(ctx, other, b.ID, []byte("x"), []byte("y"), time.Now()); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("enable someone else's endpoint: %v", err)
		}
	})

	t.Run("a newer revocation disables again", func(t *testing.T) {
		n, fresh, err := s.RevokeCredentials(ctx, owner, revokedAt.Add(time.Minute))
		if err != nil || !fresh || n != 1 { // a was enabled again; b is still disabled
			t.Fatalf("newer revocation = %d, %v, %v; want 1 endpoint", n, fresh, err)
		}
	})
}

// A registration with a credential issued before a revocation can't slip in while the revocation
// is being applied: both run under the owner's lock, and the registration re-checks the
// watermark once it holds it.
func TestRegistrationRacingARevocation(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	owner := "u_race_" + uuid.NewString()[:8]
	issued := time.Now().Add(-time.Minute) // the thief's token, checked before the revocation landed

	// Stand in for RevokeCredentials holding the owner's lock while it applies.
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1, 0))", owner); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := s.CreateEndpoint(ctx, endpoint(owner, "sale.completed"), issued)
		result <- err
	}()
	time.Sleep(200 * time.Millisecond) // the registration is now waiting for the lock
	if _, err := tx.Exec(ctx, "INSERT INTO revocations (owner_id, revoked_at) VALUES ($1, now())", owner); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, store.ErrCredentialsRevoked) {
		t.Fatalf("registration after the revocation committed: err = %v", err)
	}
	if es, _ := s.ListEndpoints(ctx, owner); len(es) != 0 {
		t.Fatalf("endpoints = %+v, want none", es)
	}
	// A credential issued after the revocation registers normally.
	if _, err := s.CreateEndpoint(ctx, endpoint(owner, "sale.completed"), time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
}
