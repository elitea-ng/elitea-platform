// Package routines is the registry of the Go platform's recurring routines
// (issue #1369): one row per routine in `elitea_runtime.platform_routines`,
// shared by the two processes that run them.
//
//   - elitea-main's platform scheduler (owner "main") reads each job's
//     enabled flag and schedule from the registry on every tick.
//   - elitea-scheduler's workers (owner "scheduler") run through [Loop], which
//     replaces their fixed tickers with the registry's schedule.
//   - elitea-main's admin API lists the rows and edits them through [Store].
//
// The registry holds the OPERATOR's wishes, not the code's defaults: each
// routine registers its default schedule at startup, an operator's override
// wins over it, and a fresh install keeps the env/values defaults because no
// override exists.
package routines

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

// scheduleParser accepts the five-field cron contract elitea-main's platform
// scheduler already uses, including descriptors such as `@hourly`.
var scheduleParser = cron.NewParser(
	cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor,
)

// MaxScheduleBytes bounds a stored schedule expression.
const MaxScheduleBytes = 64

// Schedule is a parsed schedule: a fixed interval (`30m`, `24h`) or a cron
// expression (`*/15 * * * *`).
type Schedule struct {
	text     string
	interval time.Duration
	cron     cron.Schedule
}

var (
	// ErrInvalidSchedule reports an expression that is neither a Go duration
	// nor a five-field cron expression.
	ErrInvalidSchedule = errors.New("schedule is not a duration or a five-field cron expression")
	// ErrBelowMinimum reports a schedule that fires more often than the
	// routine allows.
	ErrBelowMinimum = errors.New("schedule fires more often than this routine allows")
	// ErrNeverFires reports a cron expression that names no real instant
	// (`0 0 31 2 *`): it parses, but nothing would ever run.
	ErrNeverFires = errors.New("schedule never fires")
)

// ParseSchedule parses a Go duration (a fixed interval) or a cron expression.
func ParseSchedule(text string) (Schedule, error) {
	text = strings.TrimSpace(text)
	if text == "" || len(text) > MaxScheduleBytes {
		return Schedule{}, ErrInvalidSchedule
	}
	if interval, err := time.ParseDuration(text); err == nil {
		if interval < time.Second {
			return Schedule{}, fmt.Errorf("%w: an interval is at least one second", ErrInvalidSchedule)
		}
		return Schedule{text: text, interval: interval}, nil
	}
	parsed, err := scheduleParser.Parse(text)
	if err != nil {
		return Schedule{}, fmt.Errorf("%w: %v", ErrInvalidSchedule, err)
	}
	return Schedule{text: text, cron: parsed}, nil
}

// String returns the expression as written.
func (s Schedule) String() string { return s.text }

// Interval is the fixed gap of an interval schedule, zero for a cron
// expression. A planner that advances its cursor on every tick needs it to
// measure an interval from the previous occurrence instead.
func (s Schedule) Interval() time.Duration { return s.interval }

// IsZero reports an unparsed Schedule.
func (s Schedule) IsZero() bool { return s.interval == 0 && s.cron == nil }

// Next returns the first instant strictly after t at which the schedule fires.
// An interval fires one interval after t: callers pass the end of the previous
// pass (or the previous occurrence), so an interval is a gap between runs. A
// cron expression that never fires returns the zero time; callers skip it.
func (s Schedule) Next(t time.Time) time.Time {
	if s.cron != nil {
		return s.cron.Next(t)
	}
	if s.interval > 0 {
		return t.Add(s.interval)
	}
	return time.Time{}
}

// MinGap is the shortest distance between two consecutive firings, measured
// over the next samples firings after from. A cron expression's gaps differ
// (`0 9,10 * * *` fires an hour apart, then 23 hours), so the minimum is what
// a per-routine bound has to compare.
func (s Schedule) MinGap(from time.Time) time.Duration {
	if s.interval > 0 {
		return s.interval
	}
	if s.cron == nil {
		return 0
	}
	const samples = 64
	previous := s.cron.Next(from)
	minimum := time.Duration(1<<63 - 1)
	for range samples {
		next := s.cron.Next(previous)
		if next.IsZero() {
			break
		}
		if gap := next.Sub(previous); gap < minimum {
			minimum = gap
		}
		previous = next
	}
	return minimum
}

// ValidateSchedule parses text and refuses one that never fires or fires more
// often than minInterval. A zero minInterval skips the frequency bound.
func ValidateSchedule(text string, minInterval time.Duration, now time.Time) (Schedule, error) {
	schedule, err := ParseSchedule(text)
	if err != nil {
		return Schedule{}, err
	}
	if schedule.Next(now).IsZero() {
		return Schedule{}, ErrNeverFires
	}
	if minInterval > 0 && schedule.MinGap(now) < minInterval {
		return Schedule{}, fmt.Errorf("%w: at least %s between runs", ErrBelowMinimum, minInterval)
	}
	return schedule, nil
}
