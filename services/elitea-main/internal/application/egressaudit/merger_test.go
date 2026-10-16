package egressaudit

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/audit"
)

type recorder struct {
	mu     sync.Mutex
	events []audit.Event
}

func (r *recorder) Record(_ context.Context, event audit.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

func (r *recorder) all() []audit.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]audit.Event(nil), r.events...)
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newMerger(t *testing.T) (*Merger, *recorder, *clock) {
	t.Helper()
	rec := &recorder{}
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	m := New(rec, nil, Config{Settle: 30 * time.Second, MaxHold: 10 * time.Minute}).WithClock(c.now)
	return m, rec, c
}

func report(replica string, final bool, reached ...HostReached) Report {
	return Report{ReplicaID: replica, ExecutionID: "exec-1", ProjectID: 7, Final: final,
		TokenExpiresAt: time.Unix(1_800_000_600, 0), Connections: int64(len(reached)), BytesUp: 10, BytesDown: 20,
		HostsReached: reached}
}

// Two replicas, periodic snapshots then finals: one event, merged, with the
// LATEST snapshot of each replica (a snapshot replaces, it never adds).
func TestMergerWritesOneMergedEventPerTurn(t *testing.T) {
	m, rec, c := newMerger(t)
	m.Report(report("a", false, HostReached{Host: "pypi.org", Port: 443, Connections: 1, BytesUp: 1, BytesDown: 2}))
	m.Report(report("a", false, HostReached{Host: "pypi.org", Port: 443, Connections: 2, BytesUp: 5, BytesDown: 9}))
	b := report("b", false, HostReached{Host: "pypi.org", Port: 443, Connections: 1, BytesUp: 1, BytesDown: 1},
		HostReached{Host: "github.com", Port: 443, Connections: 3, BytesUp: 3, BytesDown: 3})
	b.HostsRefused = []HostRefused{{Host: "evil.example", Port: 443, Reason: "host-not-allowed", Count: 2}}
	m.Report(b)
	c.t = c.t.Add(time.Minute)
	m.Sweep()
	if len(rec.all()) != 0 {
		t.Fatal("no replica is done: nothing is written")
	}

	final := report("a", true, HostReached{Host: "pypi.org", Port: 443, Connections: 2, BytesUp: 5, BytesDown: 9})
	m.Report(final)
	m.Report(final) // a retried report changes nothing
	c.t = c.t.Add(time.Minute)
	m.Sweep()
	if len(rec.all()) != 0 {
		t.Fatal("replica b is not done yet")
	}
	b.ReplicaShutdown = true
	m.Report(b)
	c.t = c.t.Add(10 * time.Second)
	m.Sweep()
	if len(rec.all()) != 0 {
		t.Fatal("the settle window has not passed")
	}
	c.t = c.t.Add(30 * time.Second)
	m.Sweep()
	events := rec.all()
	if len(events) != 1 || m.Pending() != 0 {
		t.Fatalf("events %d pending %d", len(events), m.Pending())
	}
	e := events[0]
	if e.EventType != "tool" || e.EntityType != "execution" || e.EntityName != "exec-1" || *e.ProjectID != 7 || e.DurationMS == nil {
		t.Fatalf("event %+v", e)
	}
	want := "egress proxy: reached github.com:443, pypi.org:443; refused evil.example:443 (host-not-allowed x2)"
	if e.Action != want {
		t.Fatalf("action %q", e.Action)
	}
	turn := merge("exec-1", &pending{replicas: map[string]Report{"a": final, "b": b}, done: map[string]bool{"a": true, "b": true}})
	if turn.Replicas != 2 || !turn.Complete || turn.HostsReached[1].Connections != 3 || turn.HostsReached[1].BytesDown != 10 {
		t.Fatalf("merged %+v", turn)
	}
}

// A replica that never sends its last report does not hold the turn forever.
func TestMergerWritesAPartialTurnAfterMaxHold(t *testing.T) {
	m, rec, c := newMerger(t)
	m.Report(report("a", false, HostReached{Host: "pypi.org", Port: 443, Connections: 1}))
	c.t = time.Unix(1_800_000_600, 0).Add(10 * time.Minute)
	m.Sweep()
	events := rec.all()
	if len(events) != 1 || !strings.HasSuffix(events[0].Action, "; partial") {
		t.Fatalf("%+v", events)
	}
}

func TestMergerCloseFlushesAndLaterReportsStillLand(t *testing.T) {
	m, rec, _ := newMerger(t)
	m.Start()
	m.Report(report("a", false))
	m.Close()
	if len(rec.all()) != 1 {
		t.Fatal("close writes what it holds")
	}
	m.Report(report("b", true))
	if len(rec.all()) != 2 {
		t.Fatal("a report after close is written at once")
	}
}

func TestMergerBoundsWhatItHolds(t *testing.T) {
	rec := &recorder{}
	m := New(rec, nil, Config{MaxPending: 2})
	for _, id := range []string{"e1", "e2", "e3"} {
		r := report("a", false)
		r.ExecutionID = id
		m.Report(r)
	}
	if m.Pending() != 2 || len(rec.all()) != 1 {
		t.Fatalf("pending %d written %d", m.Pending(), len(rec.all()))
	}
}

func TestActionIsBounded(t *testing.T) {
	turn := Turn{Complete: true}
	for range 200 {
		turn.HostsReached = append(turn.HostsReached, HostReached{Host: "a-long-host-name.example.com", Port: 443})
	}
	if got := Action(turn); len(got) > maxAction || !strings.HasSuffix(got, "...") {
		t.Fatalf("%d %q", len(got), got[len(got)-10:])
	}
}
