package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/Bowenn/doodle-arena/libs/go/redistest"
	"github.com/Bowenn/doodle-arena/services/lobby/internal/leaderboard"
)

func TestMain(m *testing.M) { redistest.Main(m) }

// friday is in 2026-W42.
var friday = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

func newLeaderboardAPI(t *testing.T, board Leaderboard) http.Handler {
	t.Helper()
	return NewHandler(Info{Service: "lobby"}, Deps{
		Verifier: fakeVerifier{}, Rooms: &fakeRooms{}, Leaderboard: board,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return friday },
	})
}

type row struct {
	Rank   float64
	UserID string
	Name   string
	Score  float64
}

func rows(t *testing.T, body map[string]any) []row {
	t.Helper()
	entries, ok := body["entries"].([]any)
	if !ok {
		t.Fatalf("entries is not a list: %v", body)
	}
	out := make([]row, 0, len(entries))
	for _, e := range entries {
		m := e.(map[string]any)
		out = append(out, row{m["rank"].(float64), m["user_id"].(string), m["name"].(string), m["score"].(float64)})
	}
	return out
}

func TestLeaderboard(t *testing.T) {
	store := leaderboard.NewStore(redistest.New(t))
	if _, err := store.Apply(t.Context(), leaderboard.Match{ID: "m1", EndedAt: friday, Results: []leaderboard.Result{
		{UserID: "alice", Score: 300}, {UserID: "bob", Score: 200}, {UserID: "carol", Score: 200}, {UserID: "dave", Score: 100},
	}}); err != nil {
		t.Fatal(err)
	}
	h := newLeaderboardAPI(t, store)

	// Joining and quick play record the name from the access token.
	if rec, _ := call(t, h, http.MethodPost, "/api/lobby/rooms/r1/join", "good-alice", ""); rec.Code != http.StatusOK {
		t.Fatalf("join: %d", rec.Code)
	}
	if rec, _ := call(t, h, http.MethodPost, "/api/lobby/quickplay", "good-bob", ""); rec.Code != http.StatusOK {
		t.Fatalf("quick play: %d", rec.Code)
	}

	rec, body := call(t, h, http.MethodGet, "/api/lobby/leaderboard", "good-carol", "")
	if rec.Code != http.StatusOK || body["week"] != "2026-W42" {
		t.Fatalf("%d %v", rec.Code, body)
	}
	want := []row{{1, "alice", "Name alice", 300}, {2, "carol", "", 200}, {2, "bob", "Name bob", 200}, {4, "dave", "", 100}}
	got := rows(t, body)
	if len(got) != len(want) {
		t.Fatalf("entries = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %v, want %v", i, got[i], want[i])
		}
	}
	if me, _ := body["me"].(map[string]any); me["rank"] != 2.0 || me["score"] != 200.0 {
		t.Errorf("me = %v, want rank 2 and score 200", body["me"])
	}

	// Below the limit, the caller still learns their rank.
	_, body = call(t, h, http.MethodGet, "/api/lobby/leaderboard?limit=1", "good-dave", "")
	if got := rows(t, body); len(got) != 1 || got[0].UserID != "alice" {
		t.Errorf("limit=1: entries = %v", got)
	}
	if me, _ := body["me"].(map[string]any); me["rank"] != 4.0 || me["score"] != 100.0 {
		t.Errorf("limit=1: me = %v, want rank 4 and score 100", body["me"])
	}

	// No points this week: me is null, not missing.
	_, body = call(t, h, http.MethodGet, "/api/lobby/leaderboard", "good-zoe", "")
	if me, present := body["me"]; !present || me != nil {
		t.Errorf("me = %v (present %v), want null", me, present)
	}

	// Another week, nobody played: an empty list.
	rec, body = call(t, h, http.MethodGet, "/api/lobby/leaderboard?week=2026-W43", "good-alice", "")
	if rec.Code != http.StatusOK || body["week"] != "2026-W43" || len(rows(t, body)) != 0 || body["me"] != nil {
		t.Errorf("2026-W43: %d %v", rec.Code, body)
	}
}

func TestLeaderboardValidation(t *testing.T) {
	h := newLeaderboardAPI(t, leaderboard.NewStore(redistest.New(t)))
	for _, q := range []string{
		"week=2026-W54", "week=2025-W53", "week=2026-42", "week=last", "limit=0", "limit=101", "limit=x", "limit=-1",
	} {
		rec, body := call(t, h, http.MethodGet, "/api/lobby/leaderboard?"+q, "good-alice", "")
		if rec.Code != http.StatusBadRequest || errorCode(body) != "INVALID_REQUEST" {
			t.Errorf("%s: %d %v", q, rec.Code, body)
		}
	}
	for _, q := range []string{"limit=1", "limit=100", "week=2026-W53"} {
		if rec, body := call(t, h, http.MethodGet, "/api/lobby/leaderboard?"+q, "good-alice", ""); rec.Code != http.StatusOK {
			t.Errorf("%s: %d %v", q, rec.Code, body)
		}
	}
	rec, body := call(t, h, http.MethodGet, "/api/lobby/leaderboard", "", "")
	if rec.Code != http.StatusUnauthorized || errorCode(body) != "UNAUTHENTICATED" {
		t.Errorf("no token: %d %v", rec.Code, body)
	}
}

type brokenBoard struct{}

func (brokenBoard) Board(context.Context, string, int, string) (leaderboard.Board, error) {
	return leaderboard.Board{}, errors.New("redis down")
}

func (brokenBoard) RememberName(context.Context, string, string) error {
	return errors.New("redis down")
}

func TestLeaderboardFailuresDontLeakOrBlockJoins(t *testing.T) {
	h := newLeaderboardAPI(t, brokenBoard{})
	if rec, body := call(t, h, http.MethodPost, "/api/lobby/rooms/r1/join", "good-alice", ""); rec.Code != http.StatusOK {
		t.Errorf("join must not fail because the name couldn't be stored: %d %v", rec.Code, body)
	}
	rec, body := call(t, h, http.MethodGet, "/api/lobby/leaderboard", "good-alice", "")
	if rec.Code != http.StatusInternalServerError || errorCode(body) != "INTERNAL" {
		t.Errorf("%d %v", rec.Code, body)
	}
}
