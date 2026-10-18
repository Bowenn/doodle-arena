package api_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Bowenn/gap-project_private/services/notifier/internal/api"
	"github.com/Bowenn/gap-project_private/services/notifier/internal/apikeys"
	"github.com/Bowenn/gap-project_private/services/notifier/internal/pgtest"
	"github.com/Bowenn/gap-project_private/services/notifier/internal/secretbox"
	"github.com/Bowenn/gap-project_private/services/notifier/internal/ssrf"
)

// keys accepts "da_<user>_ok" (webhooks scope), "da_<user>_noscope", refuses the rest, and
// "da_x_down" means auth is unreachable.
type keys struct{}

func (keys) Verify(_ context.Context, key string) (apikeys.Identity, error) {
	parts := strings.Split(key, "_")
	if len(parts) != 3 {
		return apikeys.Identity{}, apikeys.ErrInvalid
	}
	switch parts[2] {
	case "ok":
		return apikeys.Identity{KeyID: uuid.New(), UserID: parts[1], Scopes: []string{"webhooks"}}, nil
	case "noscope":
		return apikeys.Identity{KeyID: uuid.New(), UserID: parts[1], Scopes: []string{"other"}}, nil
	case "down":
		return apikeys.Identity{}, apikeys.ErrUnavailable
	}
	return apikeys.Identity{}, apikeys.ErrInvalid
}

func newServer(t *testing.T) (*httptest.Server, *api.API) {
	t.Helper()
	k := make([]byte, 16)
	_, _ = rand.Read(k)
	box, _ := secretbox.New(hex.EncodeToString(k))
	a := &api.API{
		Store: pgtest.Store(t), Keys: keys{}, Box: box, Policy: ssrf.NewPolicy("webhook-receiver"),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: time.Now,
	}
	mux := http.NewServeMux()
	a.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, a
}

func call(t *testing.T, srv *httptest.Server, method, path, key string, body any) (int, map[string]any) {
	t.Helper()
	var buf io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		buf = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, srv.URL+path, buf)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func errCode(out map[string]any) string {
	e, _ := out["error"].(map[string]any)
	s, _ := e["code"].(string)
	return s
}

func TestEndpoints(t *testing.T) {
	srv, a := newServer(t)

	t.Run("register returns the secret once and stores it encrypted", func(t *testing.T) {
		status, out := call(t, srv, "POST", "/api/notifier/endpoints", "da_ada_ok",
			map[string]any{"url": "https://example.com/hooks", "event_types": []string{"sale.completed", "coins.credited", "sale.completed"}})
		if status != 201 {
			t.Fatalf("status %d %v", status, out)
		}
		secret, _ := out["secret"].(string)
		if !strings.HasPrefix(secret, "whsec_") {
			t.Fatalf("secret = %q", secret)
		}
		if types := out["event_types"].([]any); len(types) != 2 {
			t.Fatalf("event types not deduplicated: %v", types)
		}
		var ct []byte
		_ = a.Store.Pool.QueryRow(context.Background(), "SELECT secret_ciphertext FROM endpoints WHERE id = $1", out["id"]).Scan(&ct)
		if bytes.Contains(ct, []byte(secret)) {
			t.Fatal("secret stored in plaintext")
		}
		_, list := call(t, srv, "GET", "/api/notifier/endpoints", "da_ada_ok", nil)
		eps := list["endpoints"].([]any)
		if len(eps) != 1 || eps[0].(map[string]any)["secret"] != nil {
			t.Fatalf("list = %v", list)
		}
	})

	t.Run("bad urls and event types are refused", func(t *testing.T) {
		for _, u := range []string{"http://example.com/x", "https://169.254.169.254/", "https://10.0.0.1/", "https://localhost/x", "not a url"} {
			status, out := call(t, srv, "POST", "/api/notifier/endpoints", "da_ada_ok", map[string]any{"url": u, "event_types": []string{"sale.completed"}})
			if status != 400 || errCode(out) != "INVALID_URL" {
				t.Errorf("%s: %d %v", u, status, out)
			}
		}
		for _, types := range [][]string{{}, {"sale.created"}, {"ping"}} {
			status, out := call(t, srv, "POST", "/api/notifier/endpoints", "da_ada_ok", map[string]any{"url": "https://example.com", "event_types": types})
			if status != 400 || errCode(out) != "INVALID_EVENT_TYPES" {
				t.Errorf("%v: %d %v", types, status, out)
			}
		}
		status, _ := call(t, srv, "POST", "/api/notifier/endpoints", "da_ada_ok", map[string]any{"url": "https://example.com", "event_types": []string{"sale.completed"}, "extra": 1})
		if status != 400 {
			t.Errorf("unknown field: %d", status)
		}
	})

	t.Run("an allow-listed test receiver may use http", func(t *testing.T) {
		status, out := call(t, srv, "POST", "/api/notifier/endpoints", "da_bob_ok", map[string]any{"url": "http://webhook-receiver:9000/hooks/ok", "event_types": []string{"sale.completed"}})
		if status != 201 {
			t.Fatalf("status %d %v", status, out)
		}
	})

	t.Run("keys are checked", func(t *testing.T) {
		cases := map[string]struct {
			key    string
			status int
			code   string
		}{
			"no key":       {"", 401, "UNAUTHENTICATED"},
			"a JWT":        {"eyJhbGciOi.x.y", 401, "UNAUTHENTICATED"},
			"unknown key":  {"da_ada_nope", 401, "UNAUTHENTICATED"},
			"wrong scope":  {"da_ada_noscope", 403, "FORBIDDEN"},
			"auth is down": {"da_x_down", 503, "AUTH_UNAVAILABLE"},
		}
		for name, c := range cases {
			status, out := call(t, srv, "GET", "/api/notifier/endpoints", c.key, nil)
			if status != c.status || errCode(out) != c.code {
				t.Errorf("%s: %d %v", name, status, out)
			}
		}
	})

	t.Run("endpoints are private to their owner", func(t *testing.T) {
		_, out := call(t, srv, "POST", "/api/notifier/endpoints", "da_cyd_ok", map[string]any{"url": "https://example.com/c", "event_types": []string{"sale.completed"}})
		id := out["id"].(string)
		for _, p := range []string{"/deliveries", "/test"} {
			method := "GET"
			if p == "/test" {
				method = "POST"
			}
			if status, out := call(t, srv, method, "/api/notifier/endpoints/"+id+p, "da_eve_ok", nil); status != 404 || errCode(out) != "ENDPOINT_NOT_FOUND" {
				t.Errorf("%s by another user: %d %v", p, status, out)
			}
		}
		if status, _ := call(t, srv, "DELETE", "/api/notifier/endpoints/"+id, "da_eve_ok", nil); status != 404 {
			t.Errorf("delete by another user: %d", status)
		}
		if status, _ := call(t, srv, "DELETE", "/api/notifier/endpoints/not-a-uuid", "da_cyd_ok", nil); status != 404 {
			t.Errorf("malformed id: %d", status)
		}
		if status, _ := call(t, srv, "DELETE", "/api/notifier/endpoints/"+id, "da_cyd_ok", nil); status != 204 {
			t.Errorf("delete by the owner: %d", status)
		}
	})

	t.Run("test queues a ping that shows in the log", func(t *testing.T) {
		_, out := call(t, srv, "POST", "/api/notifier/endpoints", "da_dee_ok", map[string]any{"url": "https://example.com/d", "event_types": []string{"coins.credited"}})
		id := out["id"].(string)
		status, ping := call(t, srv, "POST", "/api/notifier/endpoints/"+id+"/test", "da_dee_ok", nil)
		if status != 202 || !strings.HasPrefix(ping["event_id"].(string), "ping_") {
			t.Fatalf("test: %d %v", status, ping)
		}
		_, log := call(t, srv, "GET", "/api/notifier/endpoints/"+id+"/deliveries", "da_dee_ok", nil)
		ds := log["deliveries"].([]any)
		if len(ds) != 1 || ds[0].(map[string]any)["event_type"] != "ping" || ds[0].(map[string]any)["status"] != "PENDING" {
			t.Fatalf("log = %v", log)
		}
		if status, _ := call(t, srv, "GET", "/api/notifier/endpoints/"+id+"/deliveries?limit=0", "da_dee_ok", nil); status != 400 {
			t.Errorf("limit=0: %d", status)
		}
	})
}
