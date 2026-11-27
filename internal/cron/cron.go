// Package cron liest Zeitpläne im Cron-Format (5 Felder) und berechnet Termine.
//
//	┌ Minute (0–59)
//	│ ┌ Stunde (0–23)
//	│ │ ┌ Tag des Monats (1–31)
//	│ │ │ ┌ Monat (1–12 oder jan–dec)
//	│ │ │ │ ┌ Wochentag (0–7 oder sun–sat, 0 und 7 = Sonntag)
//	* * * * *
//
// Felder: *, Zahlen, Listen (1,15), Bereiche (1-5), Schritte (*/15, 0-30/5). Sind Tag des
// Monats und Wochentag beide eingeschränkt, genügt einer von beiden (wie bei cron üblich).
// Kurzformen: @hourly, @daily (@midnight), @weekly, @monthly, @yearly (@annually).
package cron

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule ist ein übersetzter Zeitplan.
type Schedule struct {
	src                       string
	minute, hour, dom, month  uint64 // Bitmasken
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

// Parse übersetzt einen Zeitplan.
func Parse(src string) (*Schedule, error) {
	expr := strings.TrimSpace(src)
	if m, ok := macros[strings.ToLower(expr)]; ok {
		expr = m
	} else if strings.HasPrefix(expr, "@") {
		return nil, fmt.Errorf("%q ist unbekannt (erlaubt: %s)", expr, "@hourly, @daily, @weekly, @monthly, @yearly")
	}
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return nil, fmt.Errorf("%q: erwartet 5 Felder (Minute Stunde Tag Monat Wochentag), gefunden %d", src, len(fields))
	}
	s := &Schedule{src: src}
	var err error
	if s.minute, err = field(fields[0], 0, 59, nil, "Minute"); err != nil {
		return nil, err
	}
	if s.hour, err = field(fields[1], 0, 23, nil, "Stunde"); err != nil {
		return nil, err
	}
	if s.dom, err = field(fields[2], 1, 31, nil, "Tag"); err != nil {
		return nil, err
	}
	if s.month, err = field(fields[3], 1, 12, monthNames, "Monat"); err != nil {
		return nil, err
	}
	if s.dow, err = field(fields[4], 0, 7, dayNames, "Wochentag"); err != nil {
		return nil, err
	}
	if s.dow&(1<<7) != 0 { // 7 = Sonntag
		s.dow |= 1
	}
	s.domRestricted, s.dowRestric = fields[2] != "*", fields[4] != "*"
	return s, nil
}

// String liefert den Quelltext.
func (s *Schedule) String() string { return s.src }

func field(f string, lo, hi int, names []string, what string) (uint64, error) {
	var mask uint64
	for _, part := range strings.Split(f, ",") {
		rng, stepStr, hasStep := strings.Cut(part, "/")
		step := 1
		if hasStep {
			n, err := strconv.Atoi(stepStr)
			if err != nil || n < 1 || n > hi {
				return 0, fmt.Errorf("%s: ungültige Schrittweite %q", what, stepStr)
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
					return 0, fmt.Errorf("%s: Bereich %q ist rückwärts", what, rng)
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
		return 0, fmt.Errorf("%s: %q liegt nicht zwischen %d und %d", what, s, lo, hi)
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

// Next liefert den ersten Termin nach t (in der Zeitzone von t). Gibt es in den nächsten
// fünf Jahren keinen (z. B. 31. Februar), ist das Ergebnis die Nullzeit.
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
