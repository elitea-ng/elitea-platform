// Package intervals manipulates sets of closed integer intervals.
package intervals

import "errors"

// Interval is the closed integer range [Start, End].
type Interval struct {
	Start, End int
}

var errNotImplemented = errors.New("not implemented")

// Merge sorts and combines overlapping or endpoint-sharing intervals.
func Merge(in []Interval) ([]Interval, error) {
	return nil, errNotImplemented
}

// Insert adds x to a sorted, non-overlapping set.
func Insert(set []Interval, x Interval) ([]Interval, error) {
	return nil, errNotImplemented
}

// Remove subtracts x from a sorted, non-overlapping set.
func Remove(set []Interval, x Interval) ([]Interval, error) {
	return nil, errNotImplemented
}
