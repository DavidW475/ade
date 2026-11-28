// Package cron parses schedules in cron format (5 fields) and computes dates.
//
//	┌ minute (0–59)
//	│ ┌ hour (0–23)
//	│ │ ┌ day of month (1–31)
//	│ │ │ ┌ month (1–12 or jan–dec)
//	│ │ │ │ ┌ day of week (0–7 or sun–sat, 0 and 7 = Sunday)
//	* * * * *
//
// Fields: *, numbers, lists (1,15), ranges (1-5), steps (*/15, 0-30/5). If both day of month
// and day of week are restricted, either one is enough (as usual for cron).
// Short forms: @hourly, @daily (@midnight), @weekly, @monthly, @yearly (@annually).
package cron

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule is a parsed schedule.
type Schedule struct {
	src                       string
	minute, hour, dom, month  uint64 // bit masks
	dow                       uint64
	domRestricted, dowRestric bool
}

var macros = map[string]string{
	"@hourly":   "0 * * * *",
	"@daily":    "0 0 * * *",
	"@midnight": "0 0 * * *",
	"@weekly":   "0 0 * * 0",
	"@monthly":  "0 0 1 * *",
	"@yearly":   "0 0 1 1 *",
	"@annually": "0 0 1 1 *",
}

var monthNames = []string{"jan", "feb", "mar", "apr", "may", "jun", "jul", "aug", "sep", "oct", "nov", "dec"}
var dayNames = []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}

// Parse parses a schedule.
func Parse(src string) (*Schedule, error) {
	expr := strings.TrimSpace(src)
	if m, ok := macros[strings.ToLower(expr)]; ok {
		expr = m
	} else if strings.HasPrefix(expr, "@") {
		return nil, fmt.Errorf("%q is unknown (allowed: %s)", expr, "@hourly, @daily, @weekly, @monthly, @yearly")
	}
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return nil, fmt.Errorf("%q: expected 5 fields (minute hour day month weekday), found %d", src, len(fields))
	}
	s := &Schedule{src: src}
	var err error
	if s.minute, err = field(fields[0], 0, 59, nil, "minute"); err != nil {
		return nil, err
	}
	if s.hour, err = field(fields[1], 0, 23, nil, "hour"); err != nil {
		return nil, err
	}
	if s.dom, err = field(fields[2], 1, 31, nil, "day"); err != nil {
		return nil, err
	}
	if s.month, err = field(fields[3], 1, 12, monthNames, "month"); err != nil {
		return nil, err
	}
	if s.dow, err = field(fields[4], 0, 7, dayNames, "weekday"); err != nil {
		return nil, err
	}
	if s.dow&(1<<7) != 0 { // 7 = Sunday
		s.dow |= 1
	}
	s.domRestricted, s.dowRestric = fields[2] != "*", fields[4] != "*"
	return s, nil
}

// String returns the source text.
func (s *Schedule) String() string { return s.src }

func field(f string, lo, hi int, names []string, what string) (uint64, error) {
	var mask uint64
	for _, part := range strings.Split(f, ",") {
		rng, stepStr, hasStep := strings.Cut(part, "/")
		step := 1
		if hasStep {
			n, err := strconv.Atoi(stepStr)
			if err != nil || n < 1 || n > hi {
				return 0, fmt.Errorf("%s: invalid step %q", what, stepStr)
			}
			step = n
		}
		from, to := lo, hi
		if rng != "*" {
			a, b, isRange := strings.Cut(rng, "-")
			x, err := value(a, lo, hi, names, what)
			if err != nil {
				return 0, err
			}
			from, to = x, x
			if isRange {
				if to, err = value(b, lo, hi, names, what); err != nil {
					return 0, err
				}
				if to < from {
					return 0, fmt.Errorf("%s: range %q is backwards", what, rng)
				}
			} else if hasStep {
				to = hi
			}
		}
		for v := from; v <= to; v += step {
			mask |= 1 << uint(v)
		}
	}
	return mask, nil
}

func value(s string, lo, hi int, names []string, what string) (int, error) {
	for i, n := range names {
		if strings.EqualFold(s, n) {
			return i + lo, nil
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < lo || n > hi {
		return 0, fmt.Errorf("%s: %q is not between %d and %d", what, s, lo, hi)
	}
	return n, nil
}

func (s *Schedule) dayMatches(t time.Time) bool {
	dom := s.dom&(1<<uint(t.Day())) != 0
	dow := s.dow&(1<<uint(t.Weekday())) != 0
	if s.domRestricted && s.dowRestric {
		return dom || dow
	}
	return dom && dow
}

// Next returns the first date after t (in t's time zone). If there is none within the next
// five years (e.g. February 31), the result is the zero time.
func (s *Schedule) Next(t time.Time) time.Time {
	t = t.Truncate(time.Minute).Add(time.Minute)
	limit := t.AddDate(5, 0, 0)
	for t.Before(limit) {
		switch {
		case s.month&(1<<uint(t.Month())) == 0:
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, t.Location())
		case !s.dayMatches(t):
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, t.Location())
		case s.hour&(1<<uint(t.Hour())) == 0:
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, t.Location())
		case s.minute&(1<<uint(t.Minute())) == 0:
			t = t.Add(time.Minute)
		default:
			return t
		}
	}
	return time.Time{}
}
