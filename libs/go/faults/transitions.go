package faults

import (
	"sync"
	"time"
)

// Transition is what one observation changed.
type Transition int

const (
	// Unchanged: still healthy, or still lost.
	Unchanged Transition = iota
	// Lost: the dependency was healthy and is not any more.
	Lost
	// Back: the dependency was lost and answers again.
	Back
)

// Transitions tracks, per dependency, whether it is lost and since when, so
// an outage is logged once when it starts and once when it ends, however
// many requests or runner cycles fail in between (the FU-275 convention).
// It is safe for concurrent use.
type Transitions struct {
	mu      sync.Mutex
	since   map[string]time.Time
	now     func() time.Time
	confirm func(name string)
}

// SetConfirm installs the owner's way to check a dependency now (the
// readiness monitor's bounded, rate-limited evaluation).
func (t *Transitions) SetConfirm(confirm func(name string)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.confirm = confirm
}

// Confirm checks name now, if the owner installed a way to, and reports
// whether its outage is being reported. A runner's first failure in an
// outage thus finds the outage reported instead of racing the monitor's
// interval.
func (t *Transitions) Confirm(name string) bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	confirm := t.confirm
	t.mu.Unlock()
	if confirm != nil {
		confirm(name)
	}
	return t.IsLost(name)
}

// NewTransitions returns an empty tracker.
func NewTransitions() *Transitions {
	return &Transitions{since: map[string]time.Time{}, now: time.Now}
}

// Observe records whether name is failing now and returns the transition
// and, for Back, how long the outage lasted.
func (t *Transitions) Observe(name string, failing bool) (Transition, time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	since, lost := t.since[name]
	switch {
	case failing && !lost:
		t.since[name] = t.now()
		return Lost, 0
	case !failing && lost:
		delete(t.since, name)
		return Back, t.now().Sub(since)
	}
	return Unchanged, 0
}

// IsLost reports whether an outage of name is being reported now.
func (t *Transitions) IsLost(name string) bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	_, lost := t.since[name]
	return lost
}
