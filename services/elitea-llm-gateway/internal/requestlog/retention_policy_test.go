package requestlog

// The prune window follows the administrator's `gateway_request_log` policy
// (ADR-0042 decision 4, issue #1495) and falls back to the 92-day default when
// there is no usable one. Nothing makes the table unbounded.

import (
	"context"
	"errors"
	"testing"
	"time"
)

type policySink struct {
	captureSink
	window time.Duration
	ok     bool
	err    error
}

func (s *policySink) RetentionPolicy(context.Context) (time.Duration, bool, error) {
	return s.window, s.ok, s.err
}

func pruneWindow(t *testing.T, sink Sink, pruned func() []time.Time) time.Duration {
	t.Helper()
	now := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
	r := &Recorder{sink: sink, logger: quietLogger(), now: func() time.Time { return now }}
	r.prune()
	got := pruned()
	if len(got) != 1 {
		t.Fatalf("pruned %d times, want 1", len(got))
	}
	return now.Sub(got[0])
}

func TestPruneUsesTheDefaultWithoutAPolicy(t *testing.T) {
	sink := &captureSink{}
	if got := pruneWindow(t, sink, func() []time.Time { return sink.pruned }); got != RetentionWindow {
		t.Fatalf("window %s, want the 92-day default", got)
	}
}

func TestPruneFollowsThePolicy(t *testing.T) {
	sink := &policySink{window: 30 * 24 * time.Hour, ok: true}
	if got := pruneWindow(t, sink, func() []time.Time { return sink.pruned }); got != 30*24*time.Hour {
		t.Fatalf("window %s, want the policy's 30 days", got)
	}
}

func TestPruneFallsBackOnAnUnusablePolicy(t *testing.T) {
	cases := map[string]*policySink{
		"no row":      {},
		"unreadable":  {err: errors.New("db down")},
		"below a day": {window: time.Hour, ok: true},
		"absurd":      {window: MaxRetentionWindow + time.Hour, ok: true},
	}
	for name, sink := range cases {
		if got := pruneWindow(t, sink, func() []time.Time { return sink.pruned }); got != RetentionWindow {
			t.Errorf("%s: window %s, want the default", name, got)
		}
	}
}
