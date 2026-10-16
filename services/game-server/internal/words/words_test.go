package words

import (
	"math/rand/v2"
	"slices"
	"testing"
)

func TestListIsValid(t *testing.T) {
	if len(List) < 200 {
		t.Fatalf("only %d words", len(List))
	}
	seen := map[string]bool{}
	for _, w := range List {
		if !wordPattern.MatchString(w) {
			t.Errorf("bad word %q", w)
		}
		if seen[w] {
			t.Errorf("duplicate %q", w)
		}
		seen[w] = true
	}
}

func TestOptionsAreDistinctAndAvoidUsedWords(t *testing.T) {
	p := NewPicker(rand.New(rand.NewPCG(1, 2)))
	used := map[string]bool{}
	for range 50 {
		opts := p.Options(3, used)
		if len(opts) != 3 || opts[0] == opts[1] || opts[1] == opts[2] || opts[0] == opts[2] {
			t.Fatalf("options = %v", opts)
		}
		for _, w := range opts {
			if used[w] {
				t.Fatalf("%q was already used", w)
			}
			used[w] = true
		}
	}
}

func TestOptionsAreDeterministicWithASeed(t *testing.T) {
	a := NewPicker(rand.New(rand.NewPCG(7, 7))).Options(5, nil)
	b := NewPicker(rand.New(rand.NewPCG(7, 7))).Options(5, nil)
	if !slices.Equal(a, b) {
		t.Fatalf("%v != %v", a, b)
	}
}
