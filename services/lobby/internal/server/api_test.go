package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Bowenn/doodle-arena/libs/go/authn"
	"github.com/Bowenn/doodle-arena/services/lobby/internal/registry"
	"github.com/Bowenn/doodle-arena/services/lobby/internal/rooms"
)

type fakeVerifier struct{}

func (fakeVerifier) Verify(_ context.Context, token string) (authn.Claims, error) {
	if user, ok := strings.CutPrefix(token, "good-"); ok {
		return authn.Claims{UserID: user, Name: "Name " + user, Roles: []string{"player"}}, nil
	}
	return authn.Claims{}, authn.ErrInvalidToken
}

type fakeRooms struct {
	createErr error
	created   []string // "host public max_players"
	joinErr   error
	watchErr  error
	listed    *rooms.Cursor
}

func (f *fakeRooms) CreateRoom(_ context.Context, host string, o rooms.CreateOptions) (rooms.Room, error) {
	if f.createErr != nil {
		return rooms.Room{}, f.createErr
	}
	s, err := o.Settings.Resolve()
	if err != nil {
		return rooms.Room{}, err
	}
	created := fmt.Sprint(host, " ", o.Public, " ", s.MaxPlayers)
	if o.Live {
		created += " live"
	}
	f.created = append(f.created, created)
	return rooms.Room{ID: "r1", HostID: host, Mode: rooms.ModeClassic, Public: o.Public, Live: o.Live, Settings: s}, nil
}

func (f *fakeRooms) Watch(_ context.Context, user rooms.User, room string) (rooms.Join, error) {
	if f.watchErr != nil {
		return rooms.Join{}, f.watchErr
	}
	return rooms.Join{RoomID: room, WSURL: "/ws/relay-1/rooms/" + room, Ticket: "v-" + user.ID, ExpiresAt: time.Unix(60, 0)}, nil
}

func (f *fakeRooms) List(_ context.Context, after *rooms.Cursor, _ int) ([]rooms.Listing, *rooms.Cursor, error) {
	f.listed = after
	l := rooms.Listing{Room: rooms.Room{ID: "r1", HostID: "u-h", Mode: "classic", Settings: rooms.DefaultSettings},
		Players: 3, Phase: registry.PhaseWaiting}
	return []rooms.Listing{l}, &rooms.Cursor{Score: 5, ID: "r1"}, nil
}

func (f *fakeRooms) Join(_ context.Context, user rooms.User, room string) (rooms.Join, error) {
	if f.joinErr != nil {
		return rooms.Join{}, f.joinErr
	}
	// The ticket echoes who it was signed for, so tests can check the name got through.
	return rooms.Join{RoomID: room, WSURL: "/ws/gs-1/rooms/" + room, Ticket: "t-" + user.ID + "-" + user.Name, ExpiresAt: time.Unix(60, 0)}, nil
}

func (f *fakeRooms) QuickPlay(ctx context.Context, user rooms.User) (rooms.Join, error) {
	return f.Join(ctx, user, "rq")
}

func newAPI(f *fakeRooms) http.Handler {
	return NewHandler(Info{Service: "lobby"}, Deps{
		Verifier: fakeVerifier{}, Rooms: f, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

func call(t *testing.T, h http.Handler, method, path, token, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("X-Request-Id", "req-1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s %s: body is not JSON: %q", method, path, rec.Body.String())
	}
	return rec, out
}

func errorCode(body map[string]any) string {
	e, _ := body["error"].(map[string]any)
	code, _ := e["code"].(string)
	return code
}

func TestEveryRoomRouteNeedsAValidToken(t *testing.T) {
	h := newAPI(&fakeRooms{})
	routes := [][2]string{
		{http.MethodPost, "/api/lobby/rooms"},
		{http.MethodGet, "/api/lobby/rooms"},
		{http.MethodPost, "/api/lobby/rooms/r1/join"},
		{http.MethodPost, "/api/lobby/quickplay"},
		{http.MethodPost, "/api/lobby/rooms/r1/watch"},
	}
	for _, rt := range routes {
		for _, token := range []string{"", "forged"} {
			rec, body := call(t, h, rt[0], rt[1], token, "")
			if rec.Code != http.StatusUnauthorized || errorCode(body) != "UNAUTHENTICATED" {
				t.Errorf("%s %s with %q: %d %v", rt[0], rt[1], token, rec.Code, body)
			}
			if rec.Header().Get("WWW-Authenticate") == "" {
				t.Errorf("%s %s: no WWW-Authenticate header", rt[0], rt[1])
			}
			if e := body["error"].(map[string]any); e["request_id"] != "req-1" {
				t.Errorf("request_id = %v", e["request_id"])
			}
		}
	}
}

func TestCreateRoom(t *testing.T) {
	f := &fakeRooms{}
	h := newAPI(f)

	rec, body := call(t, h, http.MethodPost, "/api/lobby/rooms", "good-u1", "")
	if rec.Code != http.StatusCreated || body["room_id"] != "r1" || body["public"] != true {
		t.Fatalf("empty body: %d %v", rec.Code, body)
	}
	rec, _ = call(t, h, http.MethodPost, "/api/lobby/rooms", "good-u1", `{"public":false,"settings":{"max_players":4}}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("private room: %d", rec.Code)
	}
	rec, body = call(t, h, http.MethodPost, "/api/lobby/rooms", "good-u1", `{"live":true}`)
	if rec.Code != http.StatusCreated || body["live"] != true {
		t.Fatalf("live room: %d %v", rec.Code, body)
	}
	if want := []string{"u1 true 8", "u1 false 4", "u1 true 8 live"}; strings.Join(f.created, ",") != strings.Join(want, ",") {
		t.Fatalf("created = %v, want %v", f.created, want)
	}

	tests := []struct {
		body, code string
	}{
		{`{"settings":{"max_players":20}}`, "INVALID_SETTINGS"},
		{`{"mode":"battle-royale"}`, "INVALID_REQUEST"},
		{`{"unknown":1}`, "INVALID_REQUEST"},
		{`not json`, "INVALID_REQUEST"},
	}
	for _, tt := range tests {
		rec, body := call(t, h, http.MethodPost, "/api/lobby/rooms", "good-u1", tt.body)
		if rec.Code != http.StatusBadRequest || errorCode(body) != tt.code {
			t.Errorf("%s: %d %v", tt.body, rec.Code, body)
		}
	}

	f.createErr = rooms.ErrNoServers
	rec, body = call(t, h, http.MethodPost, "/api/lobby/rooms", "good-u1", "")
	if rec.Code != http.StatusServiceUnavailable || errorCode(body) != "NO_GAME_SERVER" || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("no servers: %d %v", rec.Code, body)
	}
}

func TestListRooms(t *testing.T) {
	f := &fakeRooms{}
	h := newAPI(f)
	rec, body := call(t, h, http.MethodGet, "/api/lobby/rooms", "good-u1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %v", rec.Code, body)
	}
	list := body["rooms"].([]any)
	first := list[0].(map[string]any)
	if first["room_id"] != "r1" || first["players"] != 3.0 || first["max_players"] != 8.0 || first["phase"] != "waiting" {
		t.Fatalf("rooms = %v", list)
	}
	next, _ := body["next_cursor"].(string)
	if next == "" {
		t.Fatal("no next_cursor")
	}
	// The cursor round-trips.
	rec, _ = call(t, h, http.MethodGet, "/api/lobby/rooms?limit=5&cursor="+next, "good-u1", "")
	if rec.Code != http.StatusOK || f.listed == nil || f.listed.ID != "r1" || f.listed.Score != 5 {
		t.Fatalf("%d, cursor = %+v", rec.Code, f.listed)
	}
	for _, q := range []string{"limit=0", "limit=51", "limit=x", "cursor=!!!", "cursor=bm9kb3Q"} {
		rec, body := call(t, h, http.MethodGet, "/api/lobby/rooms?"+q, "good-u1", "")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %v", q, rec.Code, body)
		}
	}
}

func TestJoinAndQuickPlay(t *testing.T) {
	f := &fakeRooms{}
	h := newAPI(f)
	rec, body := call(t, h, http.MethodPost, "/api/lobby/rooms/r1/join", "good-u2", "")
	if rec.Code != http.StatusOK || body["ticket"] != "t-u2-Name u2" || body["ws_url"] != "/ws/gs-1/rooms/r1" {
		t.Fatalf("%d %v", rec.Code, body)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("a ticket response must not be cached")
	}
	rec, body = call(t, h, http.MethodPost, "/api/lobby/quickplay", "good-u2", "")
	if rec.Code != http.StatusOK || body["room_id"] != "rq" {
		t.Fatalf("quick play: %d %v", rec.Code, body)
	}

	rec, body = call(t, h, http.MethodPost, "/api/lobby/rooms/BAD!/join", "good-u2", "")
	if rec.Code != http.StatusNotFound || errorCode(body) != "ROOM_NOT_FOUND" {
		t.Fatalf("bad id: %d %v", rec.Code, body)
	}
	for err, want := range map[error]int{
		rooms.ErrRoomNotFound:    http.StatusNotFound,
		rooms.ErrRoomUnavailable: http.StatusServiceUnavailable,
		errors.New("redis down"): http.StatusInternalServerError,
	} {
		f.joinErr = err
		rec, body := call(t, h, http.MethodPost, "/api/lobby/rooms/r1/join", "good-u2", "")
		if rec.Code != want {
			t.Errorf("%v: %d %v", err, rec.Code, body)
		}
		if strings.Contains(rec.Body.String(), "redis") {
			t.Errorf("internal error leaked to the client: %s", rec.Body.String())
		}
	}
}

func TestWatch(t *testing.T) {
	f := &fakeRooms{}
	h := newAPI(f)
	rec, body := call(t, h, http.MethodPost, "/api/lobby/rooms/r1/watch", "good-u3", "")
	if rec.Code != http.StatusOK || body["ticket"] != "v-u3" || body["ws_url"] != "/ws/relay-1/rooms/r1" {
		t.Fatalf("%d %v", rec.Code, body)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("a ticket response must not be cached")
	}
	for err, want := range map[error][2]any{
		rooms.ErrRoomNotFound: {http.StatusNotFound, "ROOM_NOT_FOUND"},
		rooms.ErrNotLive:      {http.StatusConflict, "ROOM_NOT_LIVE"},
		rooms.ErrNoRelays:     {http.StatusServiceUnavailable, "NO_RELAY"},
	} {
		f.watchErr = err
		rec, body := call(t, h, http.MethodPost, "/api/lobby/rooms/r1/watch", "good-u3", "")
		if rec.Code != want[0] || errorCode(body) != want[1] {
			t.Errorf("%v: %d %v", err, rec.Code, body)
		}
	}
}
