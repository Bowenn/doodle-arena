package leaderboard

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"time"
)

// ErrBadWeek means a week is not an ISO week such as "2026-W42".
var ErrBadWeek = errors.New("week must look like 2026-W42")

// Week returns the ISO week of t in UTC, such as "2026-W42". ISO weeks start on Monday, and
// week 1 is the week with the year's first Thursday, so the last days of December can belong
// to week 1 of the next year and the first days of January to week 52 or 53 of the previous
// one. The year in the result is that ISO year, not the calendar year.
func Week(t time.Time) string {
	y, w := t.UTC().ISOWeek()
	return fmt.Sprintf("%04d-W%02d", y, w)
}

var weekPattern = regexp.MustCompile(`^(\d{4})-W(\d{2})$`)

// ParseWeek checks that s is an ISO week that exists, such as "2026-W42" (but not
// "2025-W53": 2025 has 52 weeks).
func ParseWeek(s string) (string, error) {
	m := weekPattern.FindStringSubmatch(s)
	if m == nil {
		return "", ErrBadWeek
	}
	year, _ := strconv.Atoi(m[1])
	week, _ := strconv.Atoi(m[2])
	if week < 1 || week > 53 {
		return "", ErrBadWeek
	}
	// January 4 is always in week 1, so week 1 starts on the Monday on or before it.
	jan4 := time.Date(year, time.January, 4, 0, 0, 0, 0, time.UTC)
	monday := jan4.AddDate(0, 0, -((int(jan4.Weekday())+6)%7)+7*(week-1))
	if Week(monday) != s {
		return "", ErrBadWeek // week 53 of a year that has only 52
	}
	return s, nil
}
