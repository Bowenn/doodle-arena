package apikeys

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// fakeAuth answers like auth's /internal/api-keys/verify for one good key.
func fakeAuth(t *testing.T, calls *atomic.Int32, status *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/internal/api-keys/verify" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		if s := status.Load(); s != 0 {
			w.WriteHeader(int(s))
			return
		}
		var body struct{ Key string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Key != "da_goodprefix12_secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"key_id": "0193a4f0-0000-7000-8000-000000000001", "user_id": "u_ada", "scopes": []string{"webhooks"},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestVerifyAndCache(t *testing.T) {
	var calls, status atomic.Int32
	srv := fakeAuth(t, &calls, &status)
	v := NewVerifier(srv.URL)
	clock := time.Now()
	v.now = func() time.Time { return clock }
	ctx := context.Background()

	id, err := v.Verify(ctx, "da_goodprefix12_secret")
	if err != nil || id.UserID != "u_ada" || !id.HasScope("webhooks") || id.HasScope("admin") {
		t.Fatalf("Verify = %+v, %v", id, err)
	}
	_, _ = v.Verify(ctx, "da_goodprefix12_secret")
	if calls.Load() != 1 {
		t.Fatalf("auth called %d times, want 1 (cached)", calls.Load())
	}
	if _, err := v.Verify(ctx, "da_badprefix123_secret"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad key: err = %v", err)
	}
	_, _ = v.Verify(ctx, "da_badprefix123_secret")
	if calls.Load() != 2 {
		t.Fatalf("auth called %d times, want 2 (negative answers cached too)", calls.Load())
	}

	clock = clock.Add(31 * time.Second) // past both TTLs
	_, _ = v.Verify(ctx, "da_goodprefix12_secret")
	if calls.Load() != 3 {
		t.Fatalf("auth called %d times after expiry, want 3", calls.Load())
	}
}

func TestAuthOutageIsNotAnAnswer(t *testing.T) {
	var calls, status atomic.Int32
	srv := fakeAuth(t, &calls, &status)
	v := NewVerifier(srv.URL)
	status.Store(http.StatusServiceUnavailable)

	if _, err := v.Verify(context.Background(), "da_goodprefix12_secret"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	status.Store(0)
	if _, err := v.Verify(context.Background(), "da_goodprefix12_secret"); err != nil {
		t.Fatalf("after the outage: %v (an outage must not be cached)", err)
	}

	srv.Close()
	v2 := NewVerifier(srv.URL)
	if _, err := v2.Verify(context.Background(), "x"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("auth down: err = %v", err)
	}
}
