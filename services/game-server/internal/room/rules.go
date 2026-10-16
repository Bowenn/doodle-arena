package room

import (
	"math/rand/v2"
	"strings"
	"time"
)

// Settings are a room's rules (design doc, section 7.3). The lobby validates the same ranges.
type Settings struct {
	MaxPlayers  int
	Choose      time.Duration
	Draw        time.Duration
	WordOptions int
}

// DefaultSettings are used for any value the host left at zero.
var DefaultSettings = Settings{MaxPlayers: 8, Choose: 10 * time.Second, Draw: 80 * time.Second, WordOptions: 3}

// Fixed timings.
const (
	StartingFor = 3 * time.Second  // countdown before the first round
	RevealFor   = 5 * time.Second  // the word is shown between rounds
	SeatGrace   = 60 * time.Second // a disconnected player keeps seat and score this long
	EmptyTTL    = 60 * time.Second // a room with nobody connected closes after this
)

// The canvas has a fixed 4:3 aspect. Points are normalized to 0-10000 on each axis, so these
// sizes only tell renderers the aspect ratio (RoundFinished and the stroke log carry them).
const (
	CanvasWidth  = 800
	CanvasHeight = 600
)

// Points.
const (
	DrawerPointsPerGuess = 10
	maxCanvasPoints      = 40_000 // per round, about 500 KB of JSON
)

// hintAt are the points in the drawing time when a hint reveals more letters.
var hintAt = []float64{0.4, 0.7}

// Score is what the order-th correct guesser earns: 100, 85, 70, ... but never below 20.
func Score(order int) int {
	return max(20, 100-15*(order-1))
}

// Normalize makes a guess comparable with a word: lowercase, letters a-z only. Words are
// single lowercase ASCII words, so " Ze-Bra! " matches "zebra".
func Normalize(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if r >= 'a' && r <= 'z' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// revealMore adds hint letters: max(1, len/4) more, but never more than len/2 in total, so a
// hint never shows the whole word.
func revealMore(word string, revealed map[int]bool, rng *rand.Rand) {
	limit := len(word) / 2
	for range max(1, len(word)/4) {
		if len(revealed) >= limit {
			return
		}
		var hidden []int
		for i := range len(word) {
			if !revealed[i] {
				hidden = append(hidden, i)
			}
		}
		revealed[hidden[rng.IntN(len(hidden))]] = true
	}
}

// mask shows revealed letters and "_" for the rest, for example "a___e".
func mask(word string, revealed map[int]bool) string {
	b := []byte(strings.Repeat("_", len(word)))
	for i := range revealed {
		b[i] = word[i]
	}
	return string(b)
}
