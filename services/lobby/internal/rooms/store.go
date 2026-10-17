// Package rooms creates rooms on game servers, lists public rooms and hands out join tickets
// (design doc, sections 4.3 and 7.2).
//
// Redis keys (the lobby owns lobby:*):
//
//	lobby:room:{room_id}  string, the room as JSON. Expires RoomTTL after the owning game server
//	                      last reported it, so finished rooms disappear on their own.
//	lobby:rooms:public    sorted set of public room ids, scored by creation time (ms)
//	lobby:server_rooms:{server_id}  set of the room ids placed on that game server, so the
//	                      reassigner finds a dead server's rooms (its heartbeat is gone too)
//	lobby:room_hosts      set of server ids that have rooms
package rooms

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// RoomTTL is how long a room record outlives the last heartbeat that reported it.
const RoomTTL = 10 * time.Minute

const (
	publicKey = "lobby:rooms:public"
	hostsKey  = "lobby:room_hosts"
)

func serverRoomsKey(serverID string) string { return "lobby:server_rooms:" + serverID }

func roomKey(id string) string { return "lobby:room:" + id }

// Settings are the host's choices for a room (design doc, section 7.3).
type Settings struct {
	MaxPlayers    uint32 `json:"max_players"`
	ChooseSeconds uint32 `json:"choose_seconds"`
	DrawSeconds   uint32 `json:"draw_seconds"`
	WordOptions   uint32 `json:"word_options"`
}

// Room is the lobby's record of a room.
type Room struct {
	ID        string    `json:"id"`
	HostID    string    `json:"host_id"`
	Mode      string    `json:"mode"`
	Public    bool      `json:"public"`
	Settings  Settings  `json:"settings"`
	ServerID  string    `json:"server_id"`
	CreatedAt time.Time `json:"created_at"`
	// PlacedAt is when the room was put on ServerID (created or moved): the server has a few
	// seconds from then before its heartbeats must report the room.
	PlacedAt time.Time `json:"placed_at"`
}

// placedAt is when the room was put on its current server (older records: created).
func (r Room) placedAt() time.Time {
	if r.PlacedAt.IsZero() {
		return r.CreatedAt
	}
	return r.PlacedAt
}

// ErrRoomNotFound means there is no such room, or it has ended.
var ErrRoomNotFound = errors.New("room not found")

// Store keeps rooms in Redis.
type Store struct {
	rdb redis.UniversalClient
}

// NewStore returns a room store.
func NewStore(rdb redis.UniversalClient) *Store { return &Store{rdb: rdb} }

// Save writes a room and, if it is public, adds it to the public list.
func (s *Store) Save(ctx context.Context, r Room) error {
	data, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("encode room: %w", err)
	}
	_, err = s.rdb.TxPipelined(ctx, func(p redis.Pipeliner) error {
		p.Set(ctx, roomKey(r.ID), data, RoomTTL)
		if r.Public {
			p.ZAdd(ctx, publicKey, redis.Z{Score: float64(r.CreatedAt.UnixMilli()), Member: r.ID})
		}
		if r.ServerID != "" {
			p.SAdd(ctx, serverRoomsKey(r.ServerID), r.ID)
			p.SAdd(ctx, hostsKey, r.ServerID)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("save room %s: %w", r.ID, err)
	}
	return nil
}

// moveScript writes the moved room only if it still lives on the server the move started
// from: a stale reassignment pass can't overwrite a newer placement.
var moveScript = redis.NewScript(`
local raw = redis.call('GET', KEYS[1])
if not raw then return 0 end
local ok, room = pcall(cjson.decode, raw)
if not ok or room.server_id ~= ARGV[1] then return 0 end
redis.call('SET', KEYS[1], ARGV[3], 'PX', ARGV[4])
if ARGV[1] ~= ARGV[2] then redis.call('SREM', KEYS[2], ARGV[5]) end
redis.call('SADD', KEYS[3], ARGV[5])
redis.call('SADD', KEYS[4], ARGV[2])
return 1
`)

// ErrMovedMeanwhile means another reassignment moved the room first.
var ErrMovedMeanwhile = errors.New("room was moved meanwhile")

// Move records that a room now lives on r.ServerID instead of from, if it still lives on from.
func (s *Store) Move(ctx context.Context, r Room, from string) error {
	data, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("encode room: %w", err)
	}
	n, err := moveScript.Run(ctx, s.rdb,
		[]string{roomKey(r.ID), serverRoomsKey(from), serverRoomsKey(r.ServerID), hostsKey},
		from, r.ServerID, data, RoomTTL.Milliseconds(), r.ID).Int()
	if err != nil {
		return fmt.Errorf("move room %s: %w", r.ID, err)
	}
	if n == 0 {
		return ErrMovedMeanwhile
	}
	return nil
}

// Unindex drops a room id from a server's index (the room moved, ended or expired).
func (s *Store) Unindex(ctx context.Context, serverID, roomID string) error {
	if err := s.rdb.SRem(ctx, serverRoomsKey(serverID), roomID).Err(); err != nil {
		return fmt.Errorf("unindex room %s: %w", roomID, err)
	}
	return nil
}

// Hosts returns the servers that have rooms in the index, forgetting those with none left.
func (s *Store) Hosts(ctx context.Context) ([]string, error) {
	hosts, err := s.rdb.SMembers(ctx, hostsKey).Result()
	if err != nil {
		return nil, fmt.Errorf("list room hosts: %w", err)
	}
	var out []string
	for _, h := range hosts {
		n, err := s.rdb.SCard(ctx, serverRoomsKey(h)).Result()
		if err != nil {
			return nil, fmt.Errorf("count rooms of %s: %w", h, err)
		}
		if n == 0 {
			_ = s.rdb.SRem(ctx, hostsKey, h).Err()
			continue
		}
		out = append(out, h)
	}
	return out, nil
}

// RoomsOf returns the ids of the rooms placed on a server.
func (s *Store) RoomsOf(ctx context.Context, serverID string) ([]string, error) {
	ids, err := s.rdb.SMembers(ctx, serverRoomsKey(serverID)).Result()
	if err != nil {
		return nil, fmt.Errorf("list rooms of %s: %w", serverID, err)
	}
	return ids, nil
}

// Get returns one room.
func (s *Store) Get(ctx context.Context, id string) (Room, error) {
	raw, err := s.rdb.Get(ctx, roomKey(id)).Bytes()
	if errors.Is(err, redis.Nil) {
		return Room{}, ErrRoomNotFound
	}
	if err != nil {
		return Room{}, fmt.Errorf("read room %s: %w", id, err)
	}
	var r Room
	if err := json.Unmarshal(raw, &r); err != nil {
		return Room{}, fmt.Errorf("decode room %s: %w", id, err)
	}
	return r, nil
}

// touchScript restarts a room's expiry only if the reporting server is the room's owner, so a
// confused (or hostile) game server can't keep other servers' rooms alive. One key per call
// keeps it valid on Redis Cluster.
var touchScript = redis.NewScript(`
local raw = redis.call('GET', KEYS[1])
if not raw then return 0 end
local ok, room = pcall(cjson.decode, raw)
if ok and room.server_id == ARGV[1] then
  return redis.call('EXPIRE', KEYS[1], ARGV[2])
end
return 0
`)

// Touch restarts the expiry of the rooms that serverID still reports and owns. Ids the lobby
// doesn't know, or that another server owns, are ignored.
func (s *Store) Touch(ctx context.Context, serverID string, ids []string) error {
	ttl := int(RoomTTL / time.Second)
	// Load the script once, so the pipeline can use EVALSHA.
	if err := touchScript.Load(ctx, s.rdb).Err(); err != nil {
		return fmt.Errorf("load touch script: %w", err)
	}
	_, err := s.rdb.Pipelined(ctx, func(p redis.Pipeliner) error {
		for _, id := range ids {
			touchScript.EvalSha(ctx, p, []string{roomKey(id)}, serverID, ttl)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("touch rooms: %w", err)
	}
	return nil
}

// Remove deletes a room that has ended.
func (s *Store) Remove(ctx context.Context, r Room) error {
	_, err := s.rdb.TxPipelined(ctx, func(p redis.Pipeliner) error {
		p.Del(ctx, roomKey(r.ID))
		p.ZRem(ctx, publicKey, r.ID)
		if r.ServerID != "" {
			p.SRem(ctx, serverRoomsKey(r.ServerID), r.ID)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("remove room %s: %w", r.ID, err)
	}
	return nil
}

// Cursor marks where a page of public rooms ended: the last room's score and id.
type Cursor struct {
	Score int64
	ID    string
}

// String encodes the cursor for clients; it is opaque to them.
func (c Cursor) String() string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(c.Score, 10) + "." + c.ID))
}

// ParseCursor decodes a cursor from String.
func ParseCursor(s string) (Cursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return Cursor{}, fmt.Errorf("bad cursor: %w", err)
	}
	score, id, ok := strings.Cut(string(raw), ".")
	if !ok || id == "" {
		return Cursor{}, errors.New("bad cursor")
	}
	n, err := strconv.ParseInt(score, 10, 64)
	if err != nil {
		return Cursor{}, fmt.Errorf("bad cursor: %w", err)
	}
	return Cursor{Score: n, ID: id}, nil
}

// tieSlack is how many extra entries a page reads, to skip rooms created in the same
// millisecond as the cursor's room that the previous page already returned.
const tieSlack = 16

// PublicPage returns up to n public rooms, newest first, after the cursor (nil for the first
// page). next is nil when there are no more rooms. Ids whose record has expired are removed
// from the list on the way.
func (s *Store) PublicPage(ctx context.Context, after *Cursor, n int) (rooms []Room, next *Cursor, err error) {
	maxScore := "+inf"
	if after != nil {
		maxScore = strconv.FormatInt(after.Score, 10)
	}
	entries, err := s.rdb.ZRangeArgsWithScores(ctx, redis.ZRangeArgs{
		Key: publicKey, Start: maxScore, Stop: "-inf", ByScore: true, Rev: true, Count: int64(n + tieSlack),
	}).Result()
	if err != nil {
		return nil, nil, fmt.Errorf("list public rooms: %w", err)
	}
	// With equal scores, Redis orders a reverse range by member, descending. Skip everything
	// up to and including the cursor's room.
	var ids []string
	var scores []int64
	for _, e := range entries {
		id, _ := e.Member.(string)
		score := int64(e.Score)
		if after != nil && score == after.Score && id >= after.ID {
			continue
		}
		if len(ids) == n {
			break
		}
		ids = append(ids, id)
		scores = append(scores, score)
	}
	if len(ids) == 0 {
		return nil, nil, nil
	}
	if len(ids) == n {
		next = &Cursor{Score: scores[n-1], ID: ids[n-1]}
	}

	keys := make([]string, len(ids))
	for i, id := range ids {
		keys[i] = roomKey(id)
	}
	vals, err := s.rdb.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, nil, fmt.Errorf("read public rooms: %w", err)
	}
	var gone []any
	for i, v := range vals {
		raw, ok := v.(string)
		if !ok {
			gone = append(gone, ids[i])
			continue
		}
		var r Room
		if err := json.Unmarshal([]byte(raw), &r); err != nil {
			return nil, nil, fmt.Errorf("decode room %s: %w", ids[i], err)
		}
		rooms = append(rooms, r)
	}
	if len(gone) > 0 {
		if err := s.rdb.ZRem(ctx, publicKey, gone...).Err(); err != nil {
			return nil, nil, fmt.Errorf("forget expired rooms: %w", err)
		}
	}
	return rooms, next, nil
}
