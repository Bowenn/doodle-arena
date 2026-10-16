// Package registry keeps the list of live game servers (design doc, section 4.3). Each game
// server calls RegistryService.Heartbeat every 2 seconds; its entry in Redis expires 6 seconds
// after the last one, which is how the lobby notices a dead server.
//
// Redis keys (the lobby owns lobby:*):
//
//	lobby:server:{server_id}  string, the last heartbeat as JSON, with a 6 s TTL
//	lobby:servers             set of server ids that have sent a heartbeat; ids whose entry has
//	                          expired are removed when the set is read
package registry

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/redis/go-redis/v9"
)

// DefaultTTL is how long a server stays registered after its last heartbeat (3 missed beats).
const DefaultTTL = 6 * time.Second

const serversKey = "lobby:servers"

func serverKey(id string) string { return "lobby:server:" + id }

// Phase is the coarse state of a room, as its game server reports it.
type Phase string

// Room phases.
const (
	PhaseWaiting Phase = "waiting"
	PhasePlaying Phase = "playing"
)

// Room is one room as its game server last reported it.
type Room struct {
	ID      string `json:"id"`
	Players uint32 `json:"players"`
	Phase   Phase  `json:"phase"`
}

// Server is a game server's last heartbeat.
type Server struct {
	ID             string    `json:"id"`
	WSAddress      string    `json:"ws_address"`
	ControlAddress string    `json:"control_address"`
	Sockets        uint32    `json:"sockets"`
	Draining       bool      `json:"draining"`
	Rooms          []Room    `json:"rooms"`
	SeenAt         time.Time `json:"seen_at"`
}

// Room returns the server's report for a room, if it hosts it.
func (s Server) Room(id string) (Room, bool) {
	for _, r := range s.Rooms {
		if r.ID == id {
			return r, true
		}
	}
	return Room{}, false
}

// Registry stores heartbeats in Redis.
type Registry struct {
	rdb redis.UniversalClient
	ttl time.Duration
	log *slog.Logger
}

// New returns a registry whose entries expire ttl after the last heartbeat.
func New(rdb redis.UniversalClient, ttl time.Duration, log *slog.Logger) *Registry {
	return &Registry{rdb: rdb, ttl: ttl, log: log}
}

// Record stores a heartbeat and (re)starts the server's expiry. It reports whether the server
// was unknown before: new, or back after its entry had expired.
func (r *Registry) Record(ctx context.Context, s Server) (registered bool, err error) {
	data, err := json.Marshal(s)
	if err != nil {
		return false, fmt.Errorf("encode heartbeat: %w", err)
	}
	var prev *redis.StatusCmd
	_, err = r.rdb.TxPipelined(ctx, func(p redis.Pipeliner) error {
		prev = p.SetArgs(ctx, serverKey(s.ID), data, redis.SetArgs{TTL: r.ttl, Get: true})
		p.SAdd(ctx, serversKey, s.ID)
		return nil
	})
	if err != nil && !errors.Is(err, redis.Nil) {
		return false, fmt.Errorf("record heartbeat: %w", err)
	}
	return errors.Is(prev.Err(), redis.Nil), nil
}

// Servers returns every live server (draining ones included), sorted by id, and forgets
// servers whose entry has expired.
func (r *Registry) Servers(ctx context.Context) ([]Server, error) {
	live, _, err := r.servers(ctx)
	return live, err
}

// servers also returns the ids it found expired, for tests and logs.
func (r *Registry) servers(ctx context.Context) (live []Server, expired []string, err error) {
	ids, err := r.rdb.SMembers(ctx, serversKey).Result()
	if err != nil {
		return nil, nil, fmt.Errorf("list servers: %w", err)
	}
	if len(ids) == 0 {
		return nil, nil, nil
	}
	slices.Sort(ids)
	keys := make([]string, len(ids))
	for i, id := range ids {
		keys[i] = serverKey(id)
	}
	vals, err := r.rdb.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, nil, fmt.Errorf("read servers: %w", err)
	}
	for i, v := range vals {
		raw, ok := v.(string)
		if !ok {
			expired = append(expired, ids[i])
			continue
		}
		var s Server
		if err := json.Unmarshal([]byte(raw), &s); err != nil {
			return nil, nil, fmt.Errorf("decode server %s: %w", ids[i], err)
		}
		live = append(live, s)
	}
	if len(expired) > 0 {
		members := make([]any, len(expired))
		for i, id := range expired {
			members[i] = id
		}
		// A heartbeat that lands between MGET and SREM loses its set member until its next
		// beat 2 seconds later, so the server is missing from at most one listing.
		if err := r.rdb.SRem(ctx, serversKey, members...).Err(); err != nil {
			return nil, nil, fmt.Errorf("forget expired servers: %w", err)
		}
		r.log.WarnContext(ctx, "game servers expired: no heartbeat", "server_ids", expired)
	}
	return live, expired, nil
}

// ErrNotFound means the server's heartbeats have stopped (or never started).
var ErrNotFound = errors.New("game server not registered")

// Server returns one live server.
func (r *Registry) Server(ctx context.Context, id string) (Server, error) {
	raw, err := r.rdb.Get(ctx, serverKey(id)).Bytes()
	if errors.Is(err, redis.Nil) {
		return Server{}, ErrNotFound
	}
	if err != nil {
		return Server{}, fmt.Errorf("read server %s: %w", id, err)
	}
	var s Server
	if err := json.Unmarshal(raw, &s); err != nil {
		return Server{}, fmt.Errorf("decode server %s: %w", id, err)
	}
	return s, nil
}

// Candidates returns the servers that may receive a new room, least loaded first: not
// draining, ordered by open sockets, then by room count, then by id (so ties are stable).
func Candidates(servers []Server) []Server {
	out := make([]Server, 0, len(servers))
	for _, s := range servers {
		if !s.Draining {
			out = append(out, s)
		}
	}
	slices.SortFunc(out, func(a, b Server) int {
		return cmp.Or(
			cmp.Compare(a.Sockets, b.Sockets),
			cmp.Compare(len(a.Rooms), len(b.Rooms)),
			cmp.Compare(a.ID, b.ID),
		)
	})
	return out
}
