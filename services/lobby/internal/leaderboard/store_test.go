package leaderboard

import (
	"sync"
	"testing"
	"time"

	"github.com/Bowenn/gap-project_private/services/lobby/internal/redistest"
	"github.com/redis/go-redis/v9"
)

func TestMain(m *testing.M) { redistest.Main(m) }

// friday is in 2026-W42.
var friday = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

func scores(t *testing.T, rdb *redis.Client, week string) map[string]float64 {
	t.Helper()
	zs, err := rdb.ZRangeWithScores(t.Context(), weekKey(week), 0, -1).Result()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]float64{}
	for _, z := range zs {
		out[z.Member.(string)] = z.Score
	}
	return out
}

func TestApplyCountsAMatchOnce(t *testing.T) {
	rdb := redistest.New(t)
	s := NewStore(rdb)
	m := Match{ID: "m1", EndedAt: friday, Results: []Result{{"alice", 300}, {"bob", 120}, {"carol", 0}}}

	for i, want := range []bool{true, false, false} {
		applied, err := s.Apply(t.Context(), m)
		if err != nil {
			t.Fatal(err)
		}
		if applied != want {
			t.Errorf("delivery %d: applied = %v, want %v", i+1, applied, want)
		}
	}
	// A second match adds to the same week.
	if _, err := s.Apply(t.Context(), Match{ID: "m2", EndedAt: friday, Results: []Result{{"bob", 50}}}); err != nil {
		t.Fatal(err)
	}
	got := scores(t, rdb, "2026-W42")
	want := map[string]float64{"alice": 300, "bob": 170, "carol": 0}
	if len(got) != len(want) {
		t.Fatalf("scores = %v, want %v", got, want)
	}
	for id, v := range want {
		if got[id] != v {
			t.Errorf("%s = %v, want %v", id, got[id], v)
		}
	}
}

func TestConcurrentDuplicateDeliveriesCountOnce(t *testing.T) {
	rdb := redistest.New(t)
	s := NewStore(rdb)
	m := Match{ID: "m1", EndedAt: friday, Results: []Result{{"alice", 300}, {"bob", 120}}}

	const deliveries = 20
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		applied int
		start   = make(chan struct{})
	)
	for range deliveries {
		wg.Go(func() {
			<-start
			ok, err := s.Apply(t.Context(), m)
			if err != nil {
				t.Error(err)
				return
			}
			if ok {
				mu.Lock()
				applied++
				mu.Unlock()
			}
		})
	}
	close(start)
	wg.Wait()

	if applied != 1 {
		t.Errorf("%d deliveries applied, want exactly 1", applied)
	}
	got := scores(t, rdb, "2026-W42")
	if got["alice"] != 300 || got["bob"] != 120 {
		t.Errorf("scores = %v, want alice 300 and bob 120", got)
	}
}

func TestApplyUsesTheUTCWeekTheMatchEnded(t *testing.T) {
	rdb := redistest.New(t)
	s := NewStore(rdb)
	sunday := time.Date(2026, 10, 18, 23, 59, 59, 0, time.UTC)
	monday := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	for _, m := range []Match{
		{ID: "sun", EndedAt: sunday, Results: []Result{{"alice", 10}}},
		{ID: "mon", EndedAt: monday, Results: []Result{{"alice", 20}}},
		// 01:00 on Monday at UTC+2 is 23:00 on Sunday in UTC.
		{ID: "late", EndedAt: time.Date(2026, 10, 19, 1, 0, 0, 0, time.FixedZone("UTC+2", 7200)), Results: []Result{{"alice", 1}}},
	} {
		if _, err := s.Apply(t.Context(), m); err != nil {
			t.Fatal(err)
		}
	}
	if got := scores(t, rdb, "2026-W42")["alice"]; got != 11 {
		t.Errorf("2026-W42 alice = %v, want 11", got)
	}
	if got := scores(t, rdb, "2026-W43")["alice"]; got != 20 {
		t.Errorf("2026-W43 alice = %v, want 20", got)
	}
}

func TestApplySetsExpiries(t *testing.T) {
	rdb := redistest.New(t)
	s := NewStore(rdb)
	if _, err := s.Apply(t.Context(), Match{ID: "m1", EndedAt: friday, Results: []Result{{"alice", 1}}}); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]time.Duration{seenKey("m1"): SeenTTL, weekKey("2026-W42"): WeekTTL} {
		ttl, err := rdb.TTL(t.Context(), key).Result()
		if err != nil {
			t.Fatal(err)
		}
		if ttl <= want-time.Minute || ttl > want {
			t.Errorf("TTL of %s = %v, want about %v", key, ttl, want)
		}
	}
}

func TestBoard(t *testing.T) {
	rdb := redistest.New(t)
	s := NewStore(rdb)
	if _, err := s.Apply(t.Context(), Match{ID: "m1", EndedAt: friday, Results: []Result{
		{"alice", 300}, {"bob", 200}, {"carol", 200}, {"dave", 100}, {"erin", 50},
	}}); err != nil {
		t.Fatal(err)
	}
	for id, name := range map[string]string{"alice": "Alice", "bob": "Bob", "dave": "Dave"} {
		if err := s.RememberName(t.Context(), id, name); err != nil {
			t.Fatal(err)
		}
	}

	type row struct {
		rank  int64
		user  string
		name  string
		score int64
	}
	tests := []struct {
		name  string
		limit int
		user  string
		want  []row
		me    *Standing
	}{
		{
			// Equal scores share a rank, and the next rank is skipped. Ties are ordered by
			// user id, descending (Redis's order), so the list is stable.
			name: "competition ranking", limit: 5, user: "carol",
			want: []row{{1, "alice", "Alice", 300}, {2, "carol", "", 200}, {2, "bob", "Bob", 200}, {4, "dave", "Dave", 100}, {5, "erin", "", 50}},
			me:   &Standing{Rank: 2, Score: 200},
		},
		{
			name: "me below the limit", limit: 2, user: "erin",
			want: []row{{1, "alice", "Alice", 300}, {2, "carol", "", 200}},
			me:   &Standing{Rank: 5, Score: 50},
		},
		{
			name: "me tied with the last row shown", limit: 2, user: "bob",
			want: []row{{1, "alice", "Alice", 300}, {2, "carol", "", 200}},
			me:   &Standing{Rank: 2, Score: 200},
		},
		{
			name: "me with no points this week", limit: 1, user: "zoe",
			want: []row{{1, "alice", "Alice", 300}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := s.Board(t.Context(), "2026-W42", tt.limit, tt.user)
			if err != nil {
				t.Fatal(err)
			}
			if b.Week != "2026-W42" {
				t.Errorf("week = %s", b.Week)
			}
			if len(b.Entries) != len(tt.want) {
				t.Fatalf("entries = %+v, want %+v", b.Entries, tt.want)
			}
			for i, w := range tt.want {
				if got := b.Entries[i]; got != (Entry{Rank: w.rank, UserID: w.user, Name: w.name, Score: w.score}) {
					t.Errorf("entry %d = %+v, want %+v", i, got, w)
				}
			}
			switch {
			case tt.me == nil && b.Me != nil:
				t.Errorf("me = %+v, want nil", *b.Me)
			case tt.me != nil && (b.Me == nil || *b.Me != *tt.me):
				t.Errorf("me = %v, want %+v", b.Me, *tt.me)
			}
		})
	}
}

func TestBoardOfAnEmptyWeek(t *testing.T) {
	s := NewStore(redistest.New(t))
	b, err := s.Board(t.Context(), "2026-W01", 20, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if b.Entries == nil || len(b.Entries) != 0 || b.Me != nil {
		t.Errorf("board = %+v, want no entries (an empty list, not nil) and no me", b)
	}
}
