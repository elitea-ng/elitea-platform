// Package cron parses classic 5-field cron expressions.
package cron

import (
	"errors"
	"time"
)

var errNotImplemented = errors.New("not implemented")

// Schedule is a parsed cron expression.
type Schedule struct{}

// ParseField returns the sorted, de-duplicated values selected by expr
// within [min, max].
func ParseField(expr string, min, max int) ([]int, error) {
	return nil, errNotImplemented
}

// Parse parses "minute hour day-of-month month day-of-week".
func Parse(spec string) (*Schedule, error) {
	return nil, errNotImplemented
}

// Matches reports whether t (to the minute) is selected by the schedule.
func (s *Schedule) Matches(t time.Time) bool {
	return false
}

// Next returns the first matching time strictly after `after`.
func (s *Schedule) Next(after time.Time) (time.Time, bool) {
	return time.Time{}, false
}
