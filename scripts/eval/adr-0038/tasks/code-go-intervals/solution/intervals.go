// Package intervals manipulates sets of closed integer intervals.
package intervals

import (
	"fmt"
	"sort"
)

// Interval is the closed integer range [Start, End].
type Interval struct {
	Start, End int
}

func validate(iv Interval) error {
	if iv.Start > iv.End {
		return fmt.Errorf("intervals: invalid interval [%d,%d]", iv.Start, iv.End)
	}
	return nil
}

// Merge sorts and combines overlapping or endpoint-sharing intervals.
func Merge(in []Interval) ([]Interval, error) {
	sorted := make([]Interval, len(in))
	copy(sorted, in)
	for _, iv := range sorted {
		if err := validate(iv); err != nil {
			return nil, err
		}
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Start < sorted[j].Start })

	out := make([]Interval, 0, len(sorted))
	for _, iv := range sorted {
		if n := len(out); n > 0 && iv.Start <= out[n-1].End {
			if iv.End > out[n-1].End {
				out[n-1].End = iv.End
			}
			continue
		}
		out = append(out, iv)
	}
	return out, nil
}

// Insert adds x to a sorted, non-overlapping set.
func Insert(set []Interval, x Interval) ([]Interval, error) {
	if err := validate(x); err != nil {
		return nil, err
	}
	out := make([]Interval, 0, len(set)+1)
	i := 0
	for ; i < len(set) && set[i].End < x.Start; i++ {
		out = append(out, set[i])
	}
	for ; i < len(set) && set[i].Start <= x.End; i++ {
		if set[i].Start < x.Start {
			x.Start = set[i].Start
		}
		if set[i].End > x.End {
			x.End = set[i].End
		}
	}
	out = append(out, x)
	out = append(out, set[i:]...)
	return out, nil
}

// Remove subtracts x from a sorted, non-overlapping set.
func Remove(set []Interval, x Interval) ([]Interval, error) {
	if err := validate(x); err != nil {
		return nil, err
	}
	out := make([]Interval, 0, len(set)+1)
	for _, iv := range set {
		if iv.End < x.Start || iv.Start > x.End {
			out = append(out, iv)
			continue
		}
		if iv.Start < x.Start {
			out = append(out, Interval{iv.Start, x.Start - 1})
		}
		if iv.End > x.End {
			out = append(out, Interval{x.End + 1, iv.End})
		}
	}
	return out, nil
}
