// Package cron parses classic 5-field cron expressions.
package cron

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule is a parsed cron expression. Each field is a bitset of the
// selected values.
type Schedule struct {
	minute, hour, dom, month, dow uint64
	domStar, dowStar              bool
}

var monthNames = map[string]int{
	"JAN": 1, "FEB": 2, "MAR": 3, "APR": 4, "MAY": 5, "JUN": 6,
	"JUL": 7, "AUG": 8, "SEP": 9, "OCT": 10, "NOV": 11, "DEC": 12,
}

var dowNames = map[string]int{"SUN": 0, "MON": 1, "TUE": 2, "WED": 3, "THU": 4, "FRI": 5, "SAT": 6}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func value(s string, min, max int, names map[string]int) (int, error) {
	if v, ok := names[strings.ToUpper(s)]; ok {
		return v, nil
	}
	if !isDigits(s) {
		return 0, fmt.Errorf("cron: invalid value %q", s)
	}
	v, err := strconv.Atoi(s)
	if err != nil || v < min || v > max {
		return 0, fmt.Errorf("cron: value %q out of range %d-%d", s, min, max)
	}
	return v, nil
}

func parseBits(expr string, min, max int, names map[string]int) (uint64, error) {
	if expr == "" {
		return 0, fmt.Errorf("cron: empty field")
	}
	var bits uint64
	for _, item := range strings.Split(expr, ",") {
		rangePart, stepPart, hasStep := strings.Cut(item, "/")
		step := 1
		if hasStep {
			if !isDigits(stepPart) {
				return 0, fmt.Errorf("cron: invalid step in %q", item)
			}
			s, err := strconv.Atoi(stepPart)
			if err != nil || s < 1 {
				return 0, fmt.Errorf("cron: invalid step in %q", item)
			}
			step = s
		}
		var lo, hi int
		switch {
		case rangePart == "*":
			lo, hi = min, max
		case strings.Contains(rangePart, "-"):
			a, b, _ := strings.Cut(rangePart, "-")
			var err error
			if lo, err = value(a, min, max, names); err != nil {
				return 0, err
			}
			if hi, err = value(b, min, max, names); err != nil {
				return 0, err
			}
			if lo > hi {
				return 0, fmt.Errorf("cron: reversed range %q", rangePart)
			}
		default:
			var err error
			if lo, err = value(rangePart, min, max, names); err != nil {
				return 0, err
			}
			hi = lo
			if hasStep {
				hi = max
			}
		}
		for v := lo; v <= hi; v += step {
			bits |= 1 << uint(v)
		}
	}
	return bits, nil
}

func bitsToSlice(bits uint64) []int {
	out := []int{}
	for v := 0; v < 64; v++ {
		if bits&(1<<uint(v)) != 0 {
			out = append(out, v)
		}
	}
	return out
}

// ParseField returns the sorted, de-duplicated values selected by expr
// within [min, max].
func ParseField(expr string, min, max int) ([]int, error) {
	bits, err := parseBits(expr, min, max, nil)
	if err != nil {
		return nil, err
	}
	return bitsToSlice(bits), nil
}

// Parse parses "minute hour day-of-month month day-of-week".
func Parse(spec string) (*Schedule, error) {
	f := strings.Fields(spec)
	if len(f) != 5 {
		return nil, fmt.Errorf("cron: expected 5 fields, got %d", len(f))
	}
	var s Schedule
	var err error
	if s.minute, err = parseBits(f[0], 0, 59, nil); err != nil {
		return nil, err
	}
	if s.hour, err = parseBits(f[1], 0, 23, nil); err != nil {
		return nil, err
	}
	if s.dom, err = parseBits(f[2], 1, 31, nil); err != nil {
		return nil, err
	}
	if s.month, err = parseBits(f[3], 1, 12, monthNames); err != nil {
		return nil, err
	}
	if s.dow, err = parseBits(f[4], 0, 7, dowNames); err != nil {
		return nil, err
	}
	if s.dow&(1<<7) != 0 { // 7 is Sunday
		s.dow = (s.dow &^ (1 << 7)) | 1
	}
	s.domStar = strings.HasPrefix(f[2], "*")
	s.dowStar = strings.HasPrefix(f[4], "*")
	return &s, nil
}

func has(bits uint64, v int) bool { return bits&(1<<uint(v)) != 0 }

func (s *Schedule) dayMatches(t time.Time) bool {
	domOK := has(s.dom, t.Day())
	dowOK := has(s.dow, int(t.Weekday()))
	if !s.domStar && !s.dowStar {
		return domOK || dowOK
	}
	return domOK && dowOK
}

// Matches reports whether t (to the minute) is selected by the schedule.
func (s *Schedule) Matches(t time.Time) bool {
	return has(s.minute, t.Minute()) && has(s.hour, t.Hour()) &&
		has(s.month, int(t.Month())) && s.dayMatches(t)
}

// Next returns the first matching time strictly after `after`.
func (s *Schedule) Next(after time.Time) (time.Time, bool) {
	loc := after.Location()
	t := time.Date(after.Year(), after.Month(), after.Day(), after.Hour(), after.Minute()+1, 0, 0, loc)
	limit := after.AddDate(5, 0, 0)
	for !t.After(limit) {
		switch {
		case !has(s.month, int(t.Month())):
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, loc)
		case !s.dayMatches(t):
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, loc)
		case !has(s.hour, t.Hour()):
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, loc)
		case !has(s.minute, t.Minute()):
			t = t.Add(time.Minute)
		default:
			return t, true
		}
	}
	return time.Time{}, false
}
