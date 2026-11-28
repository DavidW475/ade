package cron

import (
	"strings"
	"testing"
	"time"
)

func TestNext(t *testing.T) {
	base := time.Date(2026, 11, 27, 14, 7, 30, 0, time.UTC) // Friday
	for expr, want := range map[string]string{
		"* * * * *":        "2026-11-27 14:08",
		"*/15 * * * *":     "2026-11-27 14:15",
		"0 3 * * *":        "2026-11-28 03:00",
		"0 3 * * 1-5":      "2026-11-30 03:00",
		"30 9 * * mon,WED": "2026-11-30 09:30",
		"0 0 1 * *":        "2026-12-01 00:00",
		"0 12 * jan *":     "2027-01-01 12:00",
		"0 0 * * 7":        "2026-11-29 00:00",
		"0 0 13 * 5":       "2026-12-04 00:00", // day OR weekday
		"5-10/5 14 * * *":  "2026-11-27 14:10",
		"@daily":           "2026-11-28 00:00",
		"@hourly":          "2026-11-27 15:00",
		"0 0 29 2 *":       "2028-02-29 00:00",
	} {
		s, err := Parse(expr)
		if err != nil {
			t.Errorf("%s: %v", expr, err)
			continue
		}
		if got := s.Next(base).Format("2006-01-02 15:04"); got != want {
			t.Errorf("%s: %s, want %s", expr, got, want)
		}
	}
	if s, _ := Parse("0 0 31 2 *"); !s.Next(base).IsZero() {
		t.Error("February 31 has a date")
	}
}

func TestParseErrors(t *testing.T) {
	for expr, want := range map[string]string{
		"* * * *":      "5 fields",
		"60 * * * *":   "minute",
		"* 24 * * *":   "hour",
		"* * 0 * *":    "day",
		"* * * foo *":  "month",
		"* * * * 8":    "weekday",
		"*/0 * * * *":  "step",
		"10-5 * * * *": "backwards",
		"@sometimes":   "unknown",
	} {
		if _, err := Parse(expr); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", expr, err, want)
		}
	}
}
