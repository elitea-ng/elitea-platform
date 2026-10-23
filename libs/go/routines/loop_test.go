package routines

import (
	"context"
	"errors"
	"testing"
	"time"
)

// memory is an in-memory Backend with the Claim semantics of the SQL.
type memory struct {
	row         *Row
	registerErr error
	getErr      error
	recorded    []string
}

func (m *memory) Register(_ context.Context, defs ...Definition) error {
	if m.registerErr != nil {
		return m.registerErr
	}
	d := defs[0]
	if m.row == nil {
		m.row = &Row{Definition: d, Enabled: true}
	}
	m.row.Definition = d
	return nil
}

func (m *memory) Get(context.Context, string) (Row, error) {
	if m.getErr != nil {
		return Row{}, m.getErr
	}
	return *m.row, nil
}

func (m *memory) Claim(_ context.Context, _ string, expected *time.Time, next time.Time) (bool, error) {
	same := (expected == nil && m.row.NextRunAt == nil) ||
		(expected != nil && m.row.NextRunAt != nil && expected.Equal(*m.row.NextRunAt))
	if !m.row.Enabled || !same {
		return false, nil
	}
	m.row.NextRunAt = &next
	return true, nil
}

func (m *memory) RecordRun(_ context.Context, _ string, at time.Time, outcome string) error {
	m.row.LastRunAt = &at
	m.row.LastOutcome = &outcome
	m.recorded = append(m.recorded, outcome)
	return nil
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newLoop(m *memory, c *clock, def string) (*Loop, *int) {
	runs := 0
	return &Loop{
		Backend: m,
		Def:     Definition{ID: "x", Owner: OwnerScheduler, DefaultSchedule: def},
		Poll:    time.Second,
		Now:     c.now,
	}, &runs
}

func counting(runs *int) Pass {
	return func(context.Context) (string, error) { *runs++; return OutcomeOK, nil }
}

func TestLoopRunsOnTheDefaultScheduleOnAFreshInstall(t *testing.T) {
	m, c := &memory{}, &clock{t: noon}
	l, runs := newLoop(m, c, "1h")
	pass := counting(runs)

	l.Step(t.Context(), pass) // first pass is immediate
	c.t = noon.Add(30 * time.Minute)
	l.Step(t.Context(), pass) // not due
	c.t = noon.Add(time.Hour)
	l.Step(t.Context(), pass) // due

	if *runs != 2 {
		t.Fatalf("runs = %d, want 2", *runs)
	}
	if got := m.recorded; len(got) != 2 || got[0] != OutcomeOK {
		t.Fatalf("recorded = %v", got)
	}
}

func TestLoopHonoursAnOperatorOverride(t *testing.T) {
	m, c := &memory{}, &clock{t: noon}
	l, runs := newLoop(m, c, "24h")
	pass := counting(runs)
	l.Step(t.Context(), pass)
	if *runs != 1 {
		t.Fatalf("first pass runs = %d", *runs)
	}

	// The operator retimes the routine to every 10 minutes; the admin API
	// re-plans next_run_at from the edit.
	override := "10m"
	m.row.ScheduleOverride = &override
	planned := noon.Add(10 * time.Minute)
	m.row.NextRunAt = &planned

	c.t = noon.Add(10 * time.Minute)
	l.Step(t.Context(), pass)
	c.t = noon.Add(20 * time.Minute)
	l.Step(t.Context(), pass)
	if *runs != 3 {
		t.Fatalf("runs = %d, want 3: the 10m override must beat the 24h default", *runs)
	}
}

func TestLoopDoesNotRunADisabledRoutine(t *testing.T) {
	m, c := &memory{}, &clock{t: noon}
	l, runs := newLoop(m, c, "1m")
	pass := counting(runs)
	l.Step(t.Context(), pass)
	m.row.Enabled = false

	for i := 1; i <= 5; i++ {
		c.t = noon.Add(time.Duration(i) * time.Hour)
		l.Step(t.Context(), pass)
	}
	if *runs != 1 {
		t.Fatalf("runs = %d, want 1: a disabled routine must not run", *runs)
	}
}

func TestLoopLetsOnlyOneReplicaClaimASlot(t *testing.T) {
	m, c := &memory{}, &clock{t: noon}
	a, runsA := newLoop(m, c, "1h")
	b, runsB := newLoop(m, c, "1h")
	a.Step(t.Context(), counting(runsA))
	b.Step(t.Context(), counting(runsB))
	if *runsA+*runsB != 1 {
		t.Fatalf("runs = %d + %d, want exactly one", *runsA, *runsB)
	}
}

func TestLoopRecordsAFailedPassAsAnError(t *testing.T) {
	m, c := &memory{}, &clock{t: noon}
	l, _ := newLoop(m, c, "1h")
	l.Step(t.Context(), func(context.Context) (string, error) { return "", errors.New("boom") })
	if len(m.recorded) != 1 || m.recorded[0] != OutcomeError {
		t.Fatalf("recorded = %v, want [error]", m.recorded)
	}
}

func TestLoopFallsBackToTheDefaultWhenTheRegistryIsUnreachable(t *testing.T) {
	m, c := &memory{registerErr: errors.New("relation does not exist")}, &clock{t: noon}
	l, runs := newLoop(m, c, "1h")
	pass := counting(runs)

	l.Step(t.Context(), pass)
	c.t = noon.Add(10 * time.Minute)
	l.Step(t.Context(), pass)
	c.t = noon.Add(time.Hour)
	l.Step(t.Context(), pass)
	if *runs != 2 {
		t.Fatalf("runs = %d, want 2 (immediate, then one interval later)", *runs)
	}
}

func TestLoopFallsBackToTheDefaultWhenAStoredScheduleIsInvalid(t *testing.T) {
	bad := "whenever"
	m := &memory{row: &Row{Enabled: true, ScheduleOverride: &bad}}
	c := &clock{t: noon}
	l, runs := newLoop(m, c, "1h")
	l.Step(t.Context(), counting(runs))
	if *runs != 1 {
		t.Fatalf("runs = %d, want 1", *runs)
	}
}

func TestLoopOutageAfterAReadKeepsTheRegistryClock(t *testing.T) {
	m, c := &memory{}, &clock{t: noon}
	l, runs := newLoop(m, c, "1h")
	pass := counting(runs)
	l.Step(t.Context(), pass) // runs and records noon

	m.getErr = errors.New("connection refused")
	c.t = noon.Add(10 * time.Minute)
	l.Step(t.Context(), pass) // degraded: the last run was ten minutes ago
	if *runs != 1 {
		t.Fatalf("runs = %d, want 1: an outage must not trigger an immediate pass", *runs)
	}
	c.t = noon.Add(time.Hour)
	l.Step(t.Context(), pass)
	if *runs != 2 {
		t.Fatalf("runs = %d, want 2: one interval after the recorded run", *runs)
	}
}

func TestLoopOutageKeepsADisabledRoutineOff(t *testing.T) {
	m, c := &memory{}, &clock{t: noon}
	l, runs := newLoop(m, c, "1m")
	pass := counting(runs)
	l.Step(t.Context(), pass)
	m.row.Enabled = false
	c.t = noon.Add(time.Minute)
	l.Step(t.Context(), pass) // reads the disable

	m.getErr = errors.New("connection refused")
	for i := 2; i <= 5; i++ {
		c.t = noon.Add(time.Duration(i) * time.Hour)
		l.Step(t.Context(), pass)
	}
	if *runs != 1 {
		t.Fatalf("runs = %d, want 1: the last read said disabled", *runs)
	}
}

func TestLoopPlansTheNextRunFromTheEndOfALongPass(t *testing.T) {
	m, c := &memory{}, &clock{t: noon}
	l, runs := newLoop(m, c, "10m")
	slow := func(context.Context) (string, error) {
		*runs++
		c.t = c.t.Add(25 * time.Minute) // the pass outlasts the interval
		return OutcomeOK, nil
	}
	l.Step(t.Context(), slow)
	if m.row.NextRunAt == nil || !m.row.NextRunAt.Equal(noon.Add(35*time.Minute)) {
		t.Fatalf("next_run_at = %v, want ten minutes after the pass ended", m.row.NextRunAt)
	}
	l.Step(t.Context(), slow)
	if *runs != 1 {
		t.Fatalf("runs = %d, want 1: no back-to-back pass", *runs)
	}
}

func TestLoopSkipsAScheduleThatNeverFires(t *testing.T) {
	never := "0 0 31 2 *"
	m := &memory{row: &Row{Enabled: true, ScheduleOverride: &never}}
	c := &clock{t: noon}
	l, runs := newLoop(m, c, "1h")
	for i := range 3 {
		c.t = noon.Add(time.Duration(i) * time.Minute)
		l.Step(t.Context(), counting(runs))
	}
	if *runs != 0 || m.row.NextRunAt != nil {
		t.Fatalf("runs = %d, next_run_at = %v: a never-firing schedule must not run", *runs, m.row.NextRunAt)
	}
}
