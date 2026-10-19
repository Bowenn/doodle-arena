package api_test

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// Task 4.R2 (ADR 0015): after a credential revocation, tokens issued before it manage nothing,
// the user's endpoints are disabled, and enabling one rotates its secret.
func TestCredentialRevocation(t *testing.T) {
	srv, a := newServer(t)
	ctx := t.Context()
	user := "u_rev_" + uuid.NewString()[:8]
	issuedAt := func(at time.Time) func(jwt.MapClaims) {
		return func(c jwt.MapClaims) { c["iat"] = at.Unix() }
	}
	stolen := session(user, issuedAt(time.Now().Add(-time.Minute)))

	status, created := call(t, srv, "POST", "/api/notifier/endpoints", stolen,
		map[string]any{"url": "https://example.com/hook", "event_types": []string{"sale.completed"}})
	if status != 201 {
		t.Fatalf("register: %d %v", status, created)
	}
	id, oldSecret := created["id"].(string), created["secret"].(string)
	if created["status"] != "active" {
		t.Fatalf("a new endpoint is %v", created["status"])
	}

	// Auth revoked the credentials 30 s ago; the consumer applied it.
	if _, _, err := a.Store.RevokeCredentials(ctx, user, time.Now().Add(-30*time.Second)); err != nil {
		t.Fatal(err)
	}

	for _, r := range []struct{ method, path string }{
		{"POST", "/api/notifier/endpoints"},
		{"GET", "/api/notifier/endpoints"},
		{"POST", "/api/notifier/endpoints/" + id + "/enable"},
		{"POST", "/api/notifier/endpoints/" + id + "/test"},
		{"DELETE", "/api/notifier/endpoints/" + id},
	} {
		if status, out := call(t, srv, r.method, r.path, stolen, map[string]any{}); status != 401 || errCode(out) != "UNAUTHENTICATED" {
			t.Fatalf("%s %s with a token issued before the revocation: %d %v", r.method, r.path, status, out)
		}
	}

	fresh := session(user, nil) // the user signed in again
	status, list := call(t, srv, "GET", "/api/notifier/endpoints", fresh, nil)
	eps, _ := list["endpoints"].([]any)
	if status != 200 || len(eps) != 1 {
		t.Fatalf("list: %d %v", status, list)
	}
	if ep := eps[0].(map[string]any); ep["status"] != "disabled" || ep["disabled_reason"] != "credentials_revoked" {
		t.Fatalf("endpoint after the revocation = %v", ep)
	}

	status, enabled := call(t, srv, "POST", "/api/notifier/endpoints/"+id+"/enable", fresh, nil)
	newSecret, _ := enabled["secret"].(string)
	if status != 200 || enabled["status"] != "active" || newSecret == "" || newSecret == oldSecret {
		t.Fatalf("enable: %d %v", status, enabled)
	}
	e, _ := a.Store.GetEndpoint(ctx, user, uuid.MustParse(id))
	stored, err := a.Box.Open(e.SecretCiphertext, e.SecretNonce, []byte(id))
	if err != nil || string(stored) != newSecret {
		t.Fatalf("stored secret isn't the new one (%v)", err)
	}

	if status, out := call(t, srv, "POST", "/api/notifier/endpoints/"+id+"/enable", fresh, nil); status != 409 || errCode(out) != "ENDPOINT_ACTIVE" {
		t.Fatalf("enable an active endpoint: %d %v", status, out)
	}
	if status, out := call(t, srv, "POST", "/api/notifier/endpoints/"+id+"/enable", session("u_someone_else", nil), nil); status != 404 || errCode(out) != "ENDPOINT_NOT_FOUND" {
		t.Fatalf("enable someone else's endpoint: %d %v", status, out)
	}
}
