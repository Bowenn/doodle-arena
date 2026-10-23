// Package server holds the HTTP handlers of the service.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Bowenn/doodle-arena/libs/go/authn"
	"github.com/Bowenn/doodle-arena/services/lobby/internal/leaderboard"
	"github.com/Bowenn/doodle-arena/services/lobby/internal/registry"
	"github.com/Bowenn/doodle-arena/services/lobby/internal/rooms"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// Info identifies this process in health responses.
type Info struct {
	Service  string `json:"service"`
	Version  string `json:"version"`
	Instance string `json:"instance"`
}

// Verifier checks access tokens (ADR 0001).
type Verifier interface {
	Verify(ctx context.Context, token string) (authn.Claims, error)
}

// Rooms is the room logic behind the API.
type Rooms interface {
	CreateRoom(ctx context.Context, hostID string, o rooms.CreateOptions) (rooms.Room, error)
	List(ctx context.Context, after *rooms.Cursor, n int) ([]rooms.Listing, *rooms.Cursor, error)
	Join(ctx context.Context, user rooms.User, roomID string) (rooms.Join, error)
	QuickPlay(ctx context.Context, user rooms.User) (rooms.Join, error)
	Watch(ctx context.Context, user rooms.User, roomID string) (rooms.Join, error)
}

// Leaderboard is the weekly leaderboard behind the API.
type Leaderboard interface {
	Board(ctx context.Context, week string, limit int, userID string) (leaderboard.Board, error)
	RememberName(ctx context.Context, userID, name string) error
}

// Deps are what the API routes need. With a nil Rooms, only /healthz is served; with a nil
// Leaderboard, the leaderboard route is left out.
type Deps struct {
	Verifier    Verifier
	Rooms       Rooms
	Leaderboard Leaderboard
	Log         *slog.Logger
	// Now picks the current week; nil means time.Now.
	Now func() time.Time
}

// NewHandler returns the service's HTTP routes, traced with OpenTelemetry (a server span per
// request, continuing the caller's traceparent). Health checks are not traced.
func NewHandler(info Info, deps Deps, opts ...otelhttp.Option) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(struct {
			Status string `json:"status"`
			Info
		}{"ok", info})
	})
	if deps.Rooms != nil {
		now := deps.Now
		if now == nil {
			now = time.Now
		}
		a := &api{rooms: deps.Rooms, board: deps.Leaderboard, log: deps.Log, now: now}
		auth := func(h http.HandlerFunc) http.Handler { return requireUser(deps.Verifier, deps.Log, h) }
		mux.Handle("POST /api/lobby/rooms", auth(a.createRoom))
		mux.Handle("GET /api/lobby/rooms", auth(a.listRooms))
		mux.Handle("POST /api/lobby/rooms/{room_id}/join", auth(a.joinRoom))
		mux.Handle("POST /api/lobby/quickplay", auth(a.quickPlay))
		mux.Handle("POST /api/lobby/rooms/{room_id}/watch", auth(a.watchRoom))
		if deps.Leaderboard != nil {
			mux.Handle("GET /api/lobby/leaderboard", auth(a.leaderboard))
		}
	}

	opts = append([]otelhttp.Option{
		otelhttp.WithFilter(func(r *http.Request) bool { return r.URL.Path != "/healthz" }),
	}, opts...)
	return otelhttp.NewHandler(mux, "http.server", opts...)
}

// requireUser verifies the bearer token and puts the caller's claims in the context.
func requireUser(v Verifier, log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || token == "" {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "a bearer access token is required")
			return
		}
		claims, err := v.Verify(r.Context(), token)
		if err != nil {
			// The reason goes to the log (never the token itself); the client only learns 401.
			log.InfoContext(r.Context(), "access token rejected", "err", err)
			w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
			writeError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "the access token is invalid or expired")
			return
		}
		next.ServeHTTP(w, r.WithContext(authn.WithClaims(r.Context(), claims)))
	})
}

type api struct {
	rooms Rooms
	board Leaderboard
	log   *slog.Logger
	now   func() time.Time
}

func userID(r *http.Request) string {
	return caller(r).ID
}

// caller is the verified user, with the display name from the access token.
func caller(r *http.Request) rooms.User {
	c, _ := authn.FromContext(r.Context())
	return rooms.User{ID: c.UserID, Name: c.Name}
}

type createRoomRequest struct {
	// Public rooms appear in the room list and in quick play. Defaults to true.
	Public *bool `json:"public"`
	// Live rooms can be watched through relays (ADR 0017). Defaults to false.
	Live     bool           `json:"live"`
	Mode     string         `json:"mode"`
	Settings rooms.Settings `json:"settings"`
}

type roomResponse struct {
	RoomID    string         `json:"room_id"`
	HostID    string         `json:"host_id"`
	Mode      string         `json:"mode"`
	Public    bool           `json:"public"`
	Live      bool           `json:"live"`
	Settings  rooms.Settings `json:"settings"`
	CreatedAt time.Time      `json:"created_at"`
}

func (a *api) createRoom(w http.ResponseWriter, r *http.Request) {
	var req createRoomRequest
	if !decode(w, r, &req) {
		return
	}
	if req.Mode != "" && req.Mode != rooms.ModeClassic {
		writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "mode must be classic")
		return
	}
	public := req.Public == nil || *req.Public
	room, err := a.rooms.CreateRoom(r.Context(), userID(r), rooms.CreateOptions{Public: public, Live: req.Live, Settings: req.Settings})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, roomResponse{
		RoomID: room.ID, HostID: room.HostID, Mode: room.Mode, Public: room.Public, Live: room.Live,
		Settings: room.Settings, CreatedAt: room.CreatedAt,
	})
}

type listing struct {
	RoomID     string         `json:"room_id"`
	HostID     string         `json:"host_id"`
	Mode       string         `json:"mode"`
	Players    uint32         `json:"players"`
	MaxPlayers uint32         `json:"max_players"`
	Phase      registry.Phase `json:"phase"`
	Live       bool           `json:"live"`
	Viewers    uint32         `json:"viewers"`
	CreatedAt  time.Time      `json:"created_at"`
}

func (a *api) listRooms(w http.ResponseWriter, r *http.Request) {
	limit := 20
	if s := r.URL.Query().Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > 50 {
			writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "limit must be between 1 and 50")
			return
		}
		limit = n
	}
	var after *rooms.Cursor
	if s := r.URL.Query().Get("cursor"); s != "" {
		c, err := rooms.ParseCursor(s)
		if err != nil {
			writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "bad cursor")
			return
		}
		after = &c
	}
	page, next, err := a.rooms.List(r.Context(), after, limit)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	out := struct {
		Rooms      []listing `json:"rooms"`
		NextCursor *string   `json:"next_cursor"`
	}{Rooms: make([]listing, 0, len(page))}
	for _, l := range page {
		out.Rooms = append(out.Rooms, listing{
			RoomID: l.ID, HostID: l.HostID, Mode: l.Mode, Players: l.Players,
			MaxPlayers: l.Settings.MaxPlayers, Phase: l.Phase, Live: l.Live, Viewers: l.Viewers, CreatedAt: l.CreatedAt,
		})
	}
	if next != nil {
		s := next.String()
		out.NextCursor = &s
	}
	writeJSON(w, http.StatusOK, out)
}

var roomIDPattern = regexp.MustCompile(`^[a-z0-9]{1,64}$`)

func (a *api) joinRoom(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("room_id")
	if !roomIDPattern.MatchString(id) {
		writeError(w, r, http.StatusNotFound, "ROOM_NOT_FOUND", "no such room")
		return
	}
	j, err := a.rooms.Join(r.Context(), caller(r), id)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	a.rememberName(r)
	writeTicket(w, j)
}

// watchRoom hands out a viewer's ticket for a live room (ADR 0017). A viewer has no seat, so it
// doesn't count towards the leaderboard's names either.
func (a *api) watchRoom(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("room_id")
	if !roomIDPattern.MatchString(id) {
		writeError(w, r, http.StatusNotFound, "ROOM_NOT_FOUND", "no such room")
		return
	}
	j, err := a.rooms.Watch(r.Context(), caller(r), id)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeTicket(w, j)
}

func (a *api) quickPlay(w http.ResponseWriter, r *http.Request) {
	j, err := a.rooms.QuickPlay(r.Context(), caller(r))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	a.rememberName(r)
	writeTicket(w, j)
}

// rememberName keeps the player's display name for the leaderboard: MatchFinished carries
// only user ids, and the name is in the access token. A failure costs only a name on the
// board, so the join goes ahead.
func (a *api) rememberName(r *http.Request) {
	u := caller(r)
	if a.board == nil || u.Name == "" {
		return
	}
	if err := a.board.RememberName(r.Context(), u.ID, u.Name); err != nil {
		a.log.WarnContext(r.Context(), "could not remember the display name", "user_id", u.ID, "err", err)
	}
}

// leaderboard returns the top of a week's leaderboard (default: the current ISO week, UTC)
// and the caller's own rank.
func (a *api) leaderboard(w http.ResponseWriter, r *http.Request) {
	week := leaderboard.Week(a.now())
	if s := r.URL.Query().Get("week"); s != "" {
		var err error
		if week, err = leaderboard.ParseWeek(s); err != nil {
			writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "week must be an ISO week such as 2026-W42")
			return
		}
	}
	limit := 20
	if s := r.URL.Query().Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > leaderboard.MaxLimit {
			writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "limit must be between 1 and 100")
			return
		}
		limit = n
	}
	b, err := a.board.Board(r.Context(), week, limit, userID(r))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, b)
}

// writeTicket sends a join response; it holds a credential, so nothing may cache it.
func writeTicket(w http.ResponseWriter, j rooms.Join) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, j)
}

// fail maps service errors to the API's error responses.
func (a *api) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, rooms.ErrInvalidSettings):
		writeError(w, r, http.StatusBadRequest, "INVALID_SETTINGS", err.Error())
	case errors.Is(err, rooms.ErrRoomNotFound):
		writeError(w, r, http.StatusNotFound, "ROOM_NOT_FOUND", "no such room")
	case errors.Is(err, rooms.ErrNoServers):
		w.Header().Set("Retry-After", "2")
		writeError(w, r, http.StatusServiceUnavailable, "NO_GAME_SERVER", "no game server can take a room right now")
	case errors.Is(err, rooms.ErrNotLive):
		writeError(w, r, http.StatusConflict, "ROOM_NOT_LIVE", "the room can't be watched")
	case errors.Is(err, rooms.ErrNoRelays):
		w.Header().Set("Retry-After", "2")
		writeError(w, r, http.StatusServiceUnavailable, "NO_RELAY", "no relay can take a viewer right now")
	case errors.Is(err, rooms.ErrRoomUnavailable):
		w.Header().Set("Retry-After", "2")
		writeError(w, r, http.StatusServiceUnavailable, "ROOM_UNAVAILABLE", "the room's game server is not reachable")
	default:
		a.log.ErrorContext(r.Context(), "request failed", "path", r.URL.Path, "err", err)
		writeError(w, r, http.StatusInternalServerError, "INTERNAL", "something went wrong")
	}
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	dec.DisallowUnknownFields()
	// An empty body means "all defaults".
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "the body must be a JSON object with known fields")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes the shared error shape (design doc, section 6.3).
func writeError(w http.ResponseWriter, r *http.Request, code int, errCode, msg string) {
	type body struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		RequestID string `json:"request_id,omitempty"`
	}
	writeJSON(w, code, struct {
		Error body `json:"error"`
	}{body{errCode, msg, r.Header.Get("X-Request-Id")}})
}
