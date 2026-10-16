// Package words holds the classic-mode word list and picks word options for a drawer.
package words

import (
	_ "embed"
	"math/rand/v2"
	"regexp"
	"strings"
)

//go:embed words.txt
var raw string

// List is every playable word: lowercase, 3-12 letters a-z.
var List = parse(raw)

var wordPattern = regexp.MustCompile(`^[a-z]{3,12}$`)

func parse(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !wordPattern.MatchString(line) {
			panic("words.txt: bad word " + line)
		}
		out = append(out, line)
	}
	return out
}

// Picker chooses word options. It isn't safe for concurrent use; each room owns one.
type Picker struct {
	rng  *rand.Rand
	list []string
}

// NewPicker returns a picker over List. Seed it differently per room; tests use fixed seeds.
func NewPicker(rng *rand.Rand) *Picker {
	return &Picker{rng: rng, list: List}
}

// Options returns n distinct words, avoiding words in used when enough are left.
func (p *Picker) Options(n int, used map[string]bool) []string {
	pool := make([]string, 0, len(p.list))
	for _, w := range p.list {
		if !used[w] {
			pool = append(pool, w)
		}
	}
	if len(pool) < n {
		pool = append(pool[:0], p.list...)
	}
	p.rng.Shuffle(len(pool), func(i, j int) { pool[i], pool[j] = pool[j], pool[i] })
	return append([]string(nil), pool[:n]...)
}
