package leaderboard

import (
	"errors"
	"testing"
	"time"
)

func TestWeek(t *testing.T) {
	plus2 := time.FixedZone("UTC+2", 2*60*60)
	tests := []struct {
		name string
		t    time.Time
		want string
	}{
		{"friday", time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC), "2026-W42"},
		{"sunday 23:59:59 UTC is the same week", time.Date(2026, 10, 18, 23, 59, 59, 0, time.UTC), "2026-W42"},
		{"monday 00:00 UTC starts the next week", time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC), "2026-W43"},
		{"monday 01:00 at UTC+2 is still sunday in UTC", time.Date(2026, 10, 19, 1, 0, 0, 0, plus2), "2026-W42"},
		{"december 31 can be week 53", time.Date(2026, 12, 31, 12, 0, 0, 0, time.UTC), "2026-W53"},
		{"january 1 can belong to last year", time.Date(2027, 1, 1, 12, 0, 0, 0, time.UTC), "2026-W53"},
		{"the first monday of the ISO year", time.Date(2027, 1, 4, 0, 0, 0, 0, time.UTC), "2027-W01"},
		{"december 29 can belong to next year", time.Date(2025, 12, 29, 0, 0, 0, 0, time.UTC), "2026-W01"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Week(tt.t); got != tt.want {
				t.Errorf("Week(%v) = %s, want %s", tt.t, got, tt.want)
			}
		})
	}
}

func TestParseWeek(t *testing.T) {
	valid := []string{"2026-W42", "2026-W01", "2026-W53", "2020-W53"}
	for _, s := range valid {
		if got, err := ParseWeek(s); err != nil || got != s {
			t.Errorf("ParseWeek(%q) = %q, %v; want it back", s, got, err)
		}
	}
	invalid := []string{
		"", "2026-W00", "2026-W54", "2025-W53", "2026-W4", "2026-42", "2026-w42", "26-W42",
		" 2026-W42", "2026-W42 ", "2026-W42x", "abcd-W42",
	}
	for _, s := range invalid {
		if _, err := ParseWeek(s); !errors.Is(err, ErrBadWeek) {
			t.Errorf("ParseWeek(%q) err = %v, want ErrBadWeek", s, err)
		}
	}
}
