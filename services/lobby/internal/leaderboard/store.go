// Package leaderboard keeps the weekly leaderboard: the sum of each player's match scores per
// ISO week, built from MatchFinished events (design doc, section 4.3).
//
// Redis keys (the lobby owns lobby:*):
//
//	lobby:leaderboard:{week}  sorted set of user ids scored by their points that week, such
//	                          as lobby:leaderboard:2026-W42. Expires WeekTTL after its last write.
//	lobby:lb:seen:{match_id}  string, set once a match was counted. Expires SeenTTL later.
//	lobby:names               hash of user id to display name, written on every join
package leaderboard

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	// SeenTTL is how long a counted match_id is remembered. It must outlive the week the
	// match counts towards: a redelivery after the marker expired would count again.
	SeenTTL = 8 * 24 * time.Hour
	// WeekTTL keeps a few past weeks readable.
	WeekTTL = 35 * 24 * time.Hour
	// MaxLimit is the most entries one read returns.
	MaxLimit = 100

	namesKey = "lobby:names"
	// opTimeout bounds every Redis call.
	opTimeout = 2 * time.Second
)

func weekKey(week string) string { return "lobby:leaderboard:" + week }

func seenKey(matchID string) string { return "lobby:lb:seen:" + matchID }

// Result is one player's score in a finished match.
type Result struct {
	UserID string
	Score  int64
}

// Match is what the leaderboard needs from a MatchFinished.
type Match struct {
	ID      string
	EndedAt time.Time
	Results []Result
}

// Entry is one row of a week's board.
type Entry struct {
	Rank   int64  `json:"rank"`
	UserID string `json:"user_id"`
	Name   string `json:"name"`
	Score  int64  `json:"score"`
}

// Standing is the caller's own place on a week's board.
type Standing struct {
	Rank  int64 `json:"rank"`
	Score int64 `json:"score"`
}

// Board is the top of one week's leaderboard, plus the caller's own place (nil if the caller
// scored nothing that week).
type Board struct {
	Week    string    `json:"week"`
	Entries []Entry   `json:"entries"`
	Me      *Standing `json:"me"`
}

// Store reads and writes the leaderboard in Redis.
type Store struct {
	rdb redis.UniversalClient
}

// NewStore returns a store on rdb.
func NewStore(rdb redis.UniversalClient) *Store {
	return &Store{rdb: rdb}
}

// applyScript counts a match once. The marker and the increments run in one script, so a
// crash can't leave a match marked but not counted (lost) or counted but not marked (counted
// again on redelivery), and two deliveries at the same moment can't both pass the check.
//
// KEYS[1] the seen marker, KEYS[2] the week's sorted set.
// ARGV[1] the marker TTL (s), ARGV[2] the week TTL (s), then pairs of user id and score.
// Returns 1 if the match was counted now, 0 if it had been counted before.
var applyScript = redis.NewScript(`
if not redis.call('SET', KEYS[1], '1', 'NX', 'EX', ARGV[1]) then
  return 0
end
for i = 3, #ARGV, 2 do
  redis.call('ZINCRBY', KEYS[2], ARGV[i + 1], ARGV[i])
end
redis.call('EXPIRE', KEYS[2], ARGV[2])
return 1
`)

// Apply adds a match's scores to the week it ended in (UTC). It returns false if the match
// was already counted, so a redelivered event changes nothing.
func (s *Store) Apply(ctx context.Context, m Match) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	args := make([]any, 0, 2+2*len(m.Results))
	args = append(args, int64(SeenTTL/time.Second), int64(WeekTTL/time.Second))
	for _, r := range m.Results {
		args = append(args, r.UserID, r.Score)
	}
	n, err := applyScript.Run(ctx, s.rdb, []string{seenKey(m.ID), weekKey(Week(m.EndedAt))}, args...).Int()
	if err != nil {
		return false, fmt.Errorf("apply match %s: %w", m.ID, err)
	}
	return n == 1, nil
}

// RememberName records a player's display name for the board.
func (s *Store) RememberName(ctx context.Context, userID, name string) error {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	if err := s.rdb.HSet(ctx, namesKey, userID, name).Err(); err != nil {
		return fmt.Errorf("remember name: %w", err)
	}
	return nil
}

// Board returns the top limit players of week, with competition ranking (equal scores share
// a rank and the next rank is skipped: 1, 2, 2, 4), and userID's own place.
func (s *Store) Board(ctx context.Context, week string, limit int, userID string) (Board, error) {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	key := weekKey(week)
	top, err := s.rdb.ZRevRangeWithScores(ctx, key, 0, int64(limit-1)).Result()
	if err != nil {
		return Board{}, fmt.Errorf("read board: %w", err)
	}
	b := Board{Week: week, Entries: make([]Entry, 0, len(top))}
	if len(top) > 0 {
		ids := make([]string, len(top))
		for i, z := range top {
			ids[i] = z.Member.(string)
		}
		names, err := s.rdb.HMGet(ctx, namesKey, ids...).Result()
		if err != nil {
			return Board{}, fmt.Errorf("read names: %w", err)
		}
		for i, z := range top {
			e := Entry{Rank: int64(i) + 1, UserID: ids[i], Score: score(z.Score)}
			if i > 0 && e.Score == b.Entries[i-1].Score {
				e.Rank = b.Entries[i-1].Rank
			}
			e.Name, _ = names[i].(string) // empty if the player never joined through this lobby
			b.Entries = append(b.Entries, e)
			if e.UserID == userID {
				b.Me = &Standing{Rank: e.Rank, Score: e.Score}
			}
		}
	}
	if b.Me != nil || userID == "" {
		return b, nil
	}

	// The caller is below the top: their rank is one more than the players with more points.
	mine, err := s.rdb.ZScore(ctx, key, userID).Result()
	if errors.Is(err, redis.Nil) {
		return b, nil
	}
	if err != nil {
		return Board{}, fmt.Errorf("read own score: %w", err)
	}
	above, err := s.rdb.ZCount(ctx, key, "("+strconv.FormatFloat(mine, 'f', -1, 64), "+inf").Result()
	if err != nil {
		return Board{}, fmt.Errorf("read own rank: %w", err)
	}
	b.Me = &Standing{Rank: above + 1, Score: score(mine)}
	return b, nil
}

// score converts a sorted-set score back to points. Points are whole numbers, and a float64
// holds every integer up to 2^53 exactly.
func score(f float64) int64 { return int64(math.Round(f)) }
