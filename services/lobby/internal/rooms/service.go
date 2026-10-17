package rooms

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	gamev1 "github.com/Bowenn/gap-project_private/proto/gen/go/doodle/game/v1"
	"github.com/Bowenn/gap-project_private/services/lobby/internal/registry"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	grpccodes "google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ModeClassic is the only game mode so far.
const ModeClassic = "classic"

// Defaults and limits for room settings (design doc, section 7.3). A zero means the default.
var (
	DefaultSettings = Settings{MaxPlayers: 8, ChooseSeconds: 10, DrawSeconds: 80, WordOptions: 3}
	minSettings     = Settings{MaxPlayers: 2, ChooseSeconds: 5, DrawSeconds: 30, WordOptions: 1}
	maxSettings     = Settings{MaxPlayers: 8, ChooseSeconds: 30, DrawSeconds: 180, WordOptions: 5}
)

const (
	// allocateTimeout is the deadline of one AllocateRoom call.
	allocateTimeout = 2 * time.Second
	// maxAttempts is how many game servers Create tries before giving up.
	maxAttempts = 3
	// reportGrace is how long a new room may be missing from its server's heartbeats (the
	// next heartbeat is at most 2 s away). After that, a room its live server doesn't report
	// is reassigned: the server restarted, or the room ended (the game server then answers
	// NOT_FOUND and the room is removed).
	reportGrace = 10 * time.Second
	// suspectFor is how long Create skips a server whose AllocateRoom failed in transport
	// (unreachable or timed out), unless no other server is left.
	suspectFor = 10 * time.Second
	// quickPlayScan is how many public rooms quick play looks at before creating a new one.
	quickPlayScan = 60
)

// Errors that the HTTP layer maps to status codes.
var (
	ErrInvalidSettings = errors.New("invalid room settings")
	ErrNoServers       = errors.New("no game server can take the room")
	ErrRoomUnavailable = errors.New("the room's game server is not reachable")
)

// Servers is the part of the registry the service reads.
type Servers interface {
	Servers(ctx context.Context) ([]registry.Server, error)
	Server(ctx context.Context, id string) (registry.Server, error)
}

// TicketSigner mints join tickets (ADR 0005).
type TicketSigner interface {
	Sign(userID, name, roomID string) (string, time.Time, error)
}

// User is the verified caller: id and display name from the access token.
type User struct {
	ID   string
	Name string
}

// Join is what a player needs to connect to a room.
type Join struct {
	RoomID    string    `json:"room_id"`
	WSURL     string    `json:"ws_url"`
	Ticket    string    `json:"ticket"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Listing is a public room with its live status.
type Listing struct {
	Room
	Players uint32
	Phase   registry.Phase
}

// Service is the room logic behind the REST API.
type Service struct {
	store       *Store
	servers     Servers
	alloc       Allocator
	tickets     TicketSigner
	now         func() time.Time
	log         *slog.Logger
	tracer      trace.Tracer
	allocations metric.Int64Counter
	reassigns   metric.Int64Counter
	// lockHolder re-checks the reassigner lock before each move (RunReassigner sets it).
	lockHolder func(context.Context) bool

	// suspect holds game servers that heartbeat but didn't answer AllocateRoom, until when
	// to skip them. Without it, such a server never gets rooms, so it stays the least loaded
	// and every create would wait out its deadline first. Per lobby process, on purpose: it
	// is a latency optimisation, not shared state.
	mu      sync.Mutex
	suspect map[string]time.Time
}

// NewService wires the room service. Metrics and spans go to the global OpenTelemetry
// providers.
func NewService(store *Store, servers Servers, alloc Allocator, tickets TicketSigner, now func() time.Time, log *slog.Logger) (*Service, error) {
	allocations, err := otel.Meter("lobby").Int64Counter("lobby.allocations",
		metric.WithDescription("Room allocations by result: success, no_servers, exhausted or error"),
		metric.WithUnit("{allocation}"))
	if err != nil {
		return nil, fmt.Errorf("lobby.allocations: %w", err)
	}
	reassigns, err := otel.Meter("lobby").Int64Counter("lobby.reassignments",
		metric.WithDescription("Rooms the reassigner handled, by result: moved, ended or failed"),
		metric.WithUnit("{room}"))
	if err != nil {
		return nil, fmt.Errorf("lobby.reassignments: %w", err)
	}
	return &Service{
		store: store, servers: servers, alloc: alloc, tickets: tickets, now: now, log: log,
		tracer: otel.Tracer("lobby"), allocations: allocations, reassigns: reassigns, suspect: make(map[string]time.Time),
	}, nil
}

// Resolve fills in defaults and checks the limits.
func (s Settings) Resolve() (Settings, error) {
	out := s
	fields := []struct {
		name     string
		v        *uint32
		def, min uint32
		max      uint32
	}{
		{"max_players", &out.MaxPlayers, DefaultSettings.MaxPlayers, minSettings.MaxPlayers, maxSettings.MaxPlayers},
		{"choose_seconds", &out.ChooseSeconds, DefaultSettings.ChooseSeconds, minSettings.ChooseSeconds, maxSettings.ChooseSeconds},
		{"draw_seconds", &out.DrawSeconds, DefaultSettings.DrawSeconds, minSettings.DrawSeconds, maxSettings.DrawSeconds},
		{"word_options", &out.WordOptions, DefaultSettings.WordOptions, minSettings.WordOptions, maxSettings.WordOptions},
	}
	for _, f := range fields {
		if *f.v == 0 {
			*f.v = f.def
		}
		if *f.v < f.min || *f.v > f.max {
			return Settings{}, fmt.Errorf("%w: %s must be between %d and %d", ErrInvalidSettings, f.name, f.min, f.max)
		}
	}
	return out, nil
}

var roomIDEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// newRoomID returns 80 random bits as 16 lowercase base32 characters, safe in URLs and keys.
func newRoomID() string {
	var b [10]byte
	_, _ = rand.Read(b[:]) // never fails (crypto/rand)
	return strings.ToLower(roomIDEncoding.EncodeToString(b[:]))
}

// Create allocates a new room on the least-loaded healthy game server. If a server is full,
// draining or unreachable, it tries the next one, up to maxAttempts servers.
func (s *Service) Create(ctx context.Context, hostID string, public bool, settings Settings) (room Room, err error) {
	ctx, span := s.tracer.Start(ctx, "lobby.create_room")
	defer func() { endSpan(span, err) }()

	settings, err = settings.Resolve()
	if err != nil {
		return Room{}, err
	}
	now := s.now().UTC()
	room = Room{
		ID: newRoomID(), HostID: hostID, Mode: ModeClassic, Public: public,
		Settings: settings, CreatedAt: now, PlacedAt: now,
	}
	span.SetAttributes(attribute.String("room_id", room.ID))

	all, err := s.servers.Servers(ctx)
	if err != nil {
		s.count(ctx, "error")
		return Room{}, err
	}
	candidates := s.withoutSuspects(registry.Candidates(all))
	if len(candidates) == 0 {
		s.count(ctx, "no_servers")
		s.log.WarnContext(ctx, "no game server for a new room", "room_id", room.ID, "user_id", hostID, "registered", len(all))
		return Room{}, ErrNoServers
	}

	req := &gamev1.AllocateRoomRequest{
		RoomId:     room.ID,
		HostUserId: hostID,
		Mode:       gamev1.GameMode_GAME_MODE_CLASSIC,
		Settings: &gamev1.RoomSettings{
			MaxPlayers: settings.MaxPlayers, ChooseSeconds: settings.ChooseSeconds,
			DrawSeconds: settings.DrawSeconds, WordOptions: settings.WordOptions,
		},
	}
	result := "exhausted"
	for _, srv := range candidates[:min(maxAttempts, len(candidates))] {
		err := s.tryAllocate(ctx, srv, req)
		if err == nil {
			room.ServerID = srv.ID
			if err := s.store.Save(ctx, room); err != nil {
				// The game server holds a room nobody can find; it closes it when nobody
				// joins (task 2.2).
				s.count(ctx, "error")
				return Room{}, err
			}
			s.count(ctx, "success")
			s.log.InfoContext(ctx, "room created", "room_id", room.ID, "user_id", hostID, "server_id", srv.ID, "public", public)
			return room, nil
		}
		switch status.Code(err) {
		case grpccodes.ResourceExhausted, grpccodes.FailedPrecondition:
			// Full or draining: an answer, not a fault.
		default:
			result = "error"
			s.markSuspect(srv.ID)
		}
		s.log.WarnContext(ctx, "allocation failed, trying the next server", "room_id", room.ID, "server_id", srv.ID, "err", err)
	}
	s.count(ctx, result)
	return Room{}, ErrNoServers
}

// tryAllocate asks one server, redialing once if the answer came from the wrong instance.
func (s *Service) tryAllocate(ctx context.Context, srv registry.Server, req *gamev1.AllocateRoomRequest) error {
	err := s.allocateOn(ctx, srv, req)
	if errors.Is(err, errMisrouted) || status.Code(err) == grpccodes.FailedPrecondition {
		// Maybe a stale connection reached another instance (it refuses: server_id). Redial
		// with a fresh name lookup and ask the same server once more; a draining server
		// simply refuses again.
		s.log.WarnContext(ctx, "redialing game server", "room_id", req.GetRoomId(), "server_id", srv.ID, "err", err)
		s.alloc.Forget(srv.ControlAddress)
		err = s.allocateOn(ctx, srv, req)
	}
	return err
}

// withoutSuspects moves suspect servers to the end, keeping the order otherwise, so they are
// tried only if every healthy-looking server fails first.
func (s *Service) withoutSuspects(candidates []registry.Server) []registry.Server {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	var ok, suspect []registry.Server
	for _, c := range candidates {
		if until, found := s.suspect[c.ID]; found && now.Before(until) {
			suspect = append(suspect, c)
			continue
		}
		delete(s.suspect, c.ID)
		ok = append(ok, c)
	}
	return append(ok, suspect...)
}

func (s *Service) markSuspect(serverID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.suspect[serverID] = s.now().Add(suspectFor)
}

func (s *Service) allocateOn(ctx context.Context, srv registry.Server, req *gamev1.AllocateRoomRequest) (err error) {
	ctx, span := s.tracer.Start(ctx, "lobby.allocate_room", trace.WithAttributes(
		attribute.String("room_id", req.GetRoomId()), attribute.String("server_id", srv.ID)))
	defer func() { endSpan(span, err) }()
	ctx, cancel := context.WithTimeout(ctx, allocateTimeout)
	defer cancel()
	req.ServerId = srv.ID // the server refuses if it isn't this one
	resp, err := s.alloc.AllocateRoom(ctx, srv.ControlAddress, req)
	if err != nil {
		return err
	}
	if resp.GetServerId() != srv.ID {
		// A game server from before the server_id check took a room meant for another one.
		// Never record it under the wrong server: joins would go to a server without it.
		return fmt.Errorf("%w: asked %s, %s answered", errMisrouted, srv.ID, resp.GetServerId())
	}
	return nil
}

// errMisrouted means the connection for one game server reached another.
var errMisrouted = errors.New("allocation reached another game server")

func (s *Service) count(ctx context.Context, result string) {
	s.allocations.Add(ctx, 1, metric.WithAttributes(attribute.String("result", result)))
}

// Join returns the room's WebSocket URL and a fresh single-use ticket. The lobby doesn't
// check capacity here: only the game server knows whether the player is already in the room
// (a reconnect), so it enforces max_players itself.
func (s *Service) Join(ctx context.Context, user User, roomID string) (j Join, err error) {
	ctx, span := s.tracer.Start(ctx, "lobby.join_room", trace.WithAttributes(attribute.String("room_id", roomID)))
	defer func() { endSpan(span, err) }()
	return s.join(ctx, user, roomID)
}

func (s *Service) join(ctx context.Context, user User, roomID string) (Join, error) {
	room, err := s.store.Get(ctx, roomID)
	if err != nil {
		return Join{}, err
	}
	srv, err := s.servers.Server(ctx, room.ServerID)
	if errors.Is(err, registry.ErrNotFound) {
		// The server died; failover (task 2.5) moves the room. Until then it can't be joined.
		return Join{}, ErrRoomUnavailable
	}
	if err != nil {
		return Join{}, err
	}
	if _, ok := srv.Room(room.ID); !ok && s.now().Sub(room.placedAt()) > reportGrace {
		// Its server no longer runs it: the reassigner moves it (or finds it ended).
		return Join{}, ErrRoomUnavailable
	}
	ticket, exp, err := s.tickets.Sign(user.ID, user.Name, room.ID)
	if err != nil {
		return Join{}, err
	}
	s.log.InfoContext(ctx, "join ticket issued", "room_id", room.ID, "user_id", user.ID, "server_id", srv.ID)
	return Join{
		RoomID:    room.ID,
		WSURL:     strings.TrimRight(srv.WSAddress, "/") + "/rooms/" + room.ID,
		Ticket:    ticket,
		ExpiresAt: exp,
	}, nil
}

// List returns a page of public rooms with their live player counts. Rooms whose server is
// gone, or doesn't report them, are hidden: the reassigner moves them, or removes them if the
// game server says they ended.
func (s *Service) List(ctx context.Context, after *Cursor, n int) ([]Listing, *Cursor, error) {
	page, next, err := s.store.PublicPage(ctx, after, n)
	if err != nil {
		return nil, nil, err
	}
	all, err := s.servers.Servers(ctx)
	if err != nil {
		return nil, nil, err
	}
	byID := make(map[string]registry.Server, len(all))
	for _, srv := range all {
		byID[srv.ID] = srv
	}
	out := make([]Listing, 0, len(page))
	for _, r := range page {
		srv, live := byID[r.ServerID]
		if !live {
			continue
		}
		st, reported := srv.Room(r.ID)
		if !reported {
			if s.now().Sub(r.placedAt()) > reportGrace {
				continue
			}
			st = registry.Room{ID: r.ID, Phase: registry.PhaseWaiting}
		}
		out = append(out, Listing{Room: r, Players: st.Players, Phase: st.Phase})
	}
	return out, next, nil
}

// QuickPlay puts the player in the fullest waiting public room that has space, or creates a
// new public room if there is none.
func (s *Service) QuickPlay(ctx context.Context, user User) (j Join, err error) {
	ctx, span := s.tracer.Start(ctx, "lobby.quickplay")
	defer func() { endSpan(span, err) }()

	var best *Listing
	var cursor *Cursor
	for seen := 0; seen < quickPlayScan; {
		page, next, err := s.List(ctx, cursor, 20)
		if err != nil {
			return Join{}, err
		}
		for i := range page {
			l := page[i]
			if l.Phase == registry.PhaseWaiting && l.Players < l.Settings.MaxPlayers &&
				(best == nil || l.Players > best.Players) {
				best = &l
			}
		}
		seen += 20
		if next == nil {
			break
		}
		cursor = next
	}
	if best != nil {
		span.SetAttributes(attribute.Bool("created", false))
		return s.join(ctx, user, best.ID)
	}
	room, err := s.Create(ctx, user.ID, true, Settings{})
	if err != nil {
		return Join{}, err
	}
	span.SetAttributes(attribute.Bool("created", true))
	return s.join(ctx, user, room.ID)
}

func endSpan(span trace.Span, err error) {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	span.End()
}
