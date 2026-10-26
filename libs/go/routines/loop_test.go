package routines

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// memory is an in-memory Backend with the claim and lease semantics of the
// SQL, on the test's clock.
type memory struct {
	mu          sync.Mutex
	row         *Row
	registerErr error
	getErr      error
	renewErr    error
	// renewHang, when set, makes Renew block until its context ends, after
	// signalling renewStarted.
	renewHang    bool
	renewStarted chan struct{}
	recorded     []string
	clock        *clock

	token   int64
	holder  string
	expires time.Time
	// renewed, when set, receives a value after every successful renewal.
	renewed chan struct{}
}

func (m *memory) Register(_ context.Context, defs ...Definition) error {
	m.mu.Lock()
	defer m.mu.Unlock()
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
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.getErr != nil {
		return Row{}, m.getErr
	}
	return *m.row, nil
}

func (m *memory) Acquire(
	_ context.Context, _ string, expected *time.Time, held time.Time, holder string, ttl time.Duration,
) (Lease, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	same := (expected == nil && m.row.NextRunAt == nil) ||
		(expected != nil && m.row.NextRunAt != nil && expected.Equal(*m.row.NextRunAt))
	now := m.clock.now()
	live := !m.expires.IsZero() && now.Before(m.expires)
	if !m.row.Enabled || !same || live {
		return Lease{}, false, nil
	}
	m.row.NextRunAt = &held
	m.token++
	m.holder = holder
	m.expires = now.Add(ttl)
	return Lease{Token: m.token, Holder: holder}, true, nil
}

func (m *memory) Renew(ctx context.Context, _ string, lease Lease, ttl time.Duration) error {
	m.mu.Lock()
	if m.renewHang {
		m.mu.Unlock()
		select {
		case m.renewStarted <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return ctx.Err()
	}
	defer m.mu.Unlock()
	if m.renewErr != nil {
		return m.renewErr
	}
	if lease.Token != m.token || !m.clock.now().Before(m.expires) {
		return ErrLeaseLost
	}
	m.expires = m.clock.now().Add(ttl)
	if m.renewed != nil {
		select {
		case m.renewed <- struct{}{}:
		default:
		}
	}
	return nil
}

func (m *memory) Release(_ context.Context, _ string, lease Lease, done Completion) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if lease.Token != m.token {
		return ErrLeaseLost
	}
	m.row.LastRunAt = &done.RanAt
	m.row.LastOutcome = &done.Outcome
	m.recorded = append(m.recorded, done.Outcome)
	if !done.Next.IsZero() && m.row.NextRunAt != nil && m.row.NextRunAt.Equal(done.Held) && done.Next.After(done.Held) {
		next := done.Next
		m.row.NextRunAt = &next
	}
	m.expires = time.Time{}
	return nil
}

func (m *memory) runs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.recorded...)
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

func newLoop(m *memory, c *clock, def string) (*Loop, *int) {
	runs := 0
	m.clock = c
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

// #1414: a pass that outlasts its interval keeps its lease (it renews it), so
// the slot falling due again does not start the routine on another replica.
// The other replica runs only once the pass has ended and the slot planned
// from its end is due.
func TestLoopAPassThatOutlastsItsIntervalDoesNotStartOnAnotherReplica(t *testing.T) {
	m, c := &memory{renewed: make(chan struct{}, 1)}, &clock{t: noon}
	a, _ := newLoop(m, c, "10m")
	b, _ := newLoop(m, c, "10m")
	// A 30-minute lease on the test clock, renewed every 10 ms of real time.
	a.LeaseTTL, b.LeaseTTL = 30*time.Minute, 30*time.Minute
	a.renewEvery = 10 * time.Millisecond
	a.Holder, b.Holder = "a", "b"

	var runsA, runsB atomic.Int32
	started, finish := make(chan struct{}), make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.Step(t.Context(), func(context.Context) (string, error) {
			runsA.Add(1)
			close(started)
			<-finish
			return OutcomeOK, nil
		})
	}()
	<-started

	// The pass is still running 25 minutes later: its slot (noon+10m) is due.
	c.set(noon.Add(25 * time.Minute))
	waitLive(t, m, c, noon.Add(55*time.Minute)) // renewed on the new clock
	// 50 minutes in: the first grant (noon+30m) has run out; only the
	// renewals keep the lease live.
	c.set(noon.Add(50 * time.Minute))
	for range 3 {
		b.Step(t.Context(), func(context.Context) (string, error) { runsB.Add(1); return OutcomeOK, nil })
	}
	if runsB.Load() != 0 {
		t.Fatalf("replica b ran %d passes while a's pass was live", runsB.Load())
	}

	close(finish)
	<-done
	// The next slot is planned from the end of a's pass.
	b.Step(t.Context(), func(context.Context) (string, error) { runsB.Add(1); return OutcomeOK, nil })
	if runsB.Load() != 0 {
		t.Fatal("replica b ran before the slot planned from the end of a's pass")
	}
	c.set(noon.Add(60 * time.Minute))
	b.Step(t.Context(), func(context.Context) (string, error) { runsB.Add(1); return OutcomeOK, nil })
	if runsA.Load() != 1 || runsB.Load() != 1 {
		t.Fatalf("runs a=%d b=%d, want 1 and 1", runsA.Load(), runsB.Load())
	}
}

// A holder that crashed mid-pass stops renewing: while its lease is live the
// routine does not run anywhere, and once the lease runs out another replica
// takes it over. The crashed holder's stale token can no longer write.
func TestLoopTakesOverACrashedHoldersRoutineOnceItsLeaseExpires(t *testing.T) {
	m, c := &memory{}, &clock{t: noon}
	b, runs := newLoop(m, c, "10m")
	pass := counting(runs)
	if err := m.Register(t.Context(), b.Def); err != nil {
		t.Fatal(err)
	}
	crashed, ok, err := m.Acquire(t.Context(), "x", nil, noon.Add(10*time.Minute), "crashed", 20*time.Minute)
	if err != nil || !ok {
		t.Fatalf("crashed holder's acquire: ok=%v err=%v", ok, err)
	}

	c.set(noon.Add(10 * time.Minute)) // the slot is due, the lease is live until noon+20m
	b.Step(t.Context(), pass)
	if *runs != 0 {
		t.Fatal("a replica ran the routine while a crashed holder's lease was live")
	}

	c.set(noon.Add(20 * time.Minute)) // the lease has run out
	b.Step(t.Context(), pass)
	if *runs != 1 {
		t.Fatalf("runs = %d, want 1: the routine must be taken over once the lease expires", *runs)
	}
	if err := m.Renew(t.Context(), "x", crashed, time.Minute); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale renew: %v, want ErrLeaseLost", err)
	}
	if err := m.Release(t.Context(), "x", crashed, Completion{RanAt: noon, Outcome: OutcomeError}); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale release: %v, want ErrLeaseLost", err)
	}
	if got := m.runs(); len(got) != 1 || got[0] != OutcomeOK {
		t.Fatalf("recorded = %v, want only the take-over's ok", got)
	}
}

// A holder whose token moved on (another replica took the lease over) has its
// pass cancelled with ErrLeaseLost, and records nothing.
func TestLoopCancelsAPassWhoseLeaseWasTakenOver(t *testing.T) {
	m, c := &memory{}, &clock{t: noon}
	a, _ := newLoop(m, c, "10m")
	a.LeaseTTL = 30 * time.Millisecond
	var cause error
	a.Step(t.Context(), func(ctx context.Context) (string, error) {
		m.mu.Lock()
		m.token++ // another replica's acquisition
		m.mu.Unlock()
		select {
		case <-ctx.Done():
			cause = context.Cause(ctx)
		case <-time.After(5 * time.Second):
		}
		return "", cause
	})
	if !errors.Is(cause, ErrLeaseLost) {
		t.Fatalf("pass context cause = %v, want ErrLeaseLost", cause)
	}
	if got := m.runs(); len(got) != 0 {
		t.Fatalf("recorded = %v: a holder that lost its lease must not write", got)
	}
	if a.seen.LastRunAt != nil {
		t.Fatalf("seen.LastRunAt = %v: a run the fence refused must not count as this replica's run", a.seen.LastRunAt)
	}
}

// Renewals that keep failing stop the pass strictly BEFORE the lease can run
// out (#1414 review): the holder gives up one renewal interval ahead of the
// earliest expiry, counted from when the last successful statement was
// issued, so another replica can never start while this pass still runs.
func TestLoopStopsAPassBeforeAnUnrenewedLeaseCanRunOut(t *testing.T) {
	m, c := &memory{renewErr: errors.New("connection refused")}, &clock{t: noon}
	a, _ := newLoop(m, c, "10m")
	const ttl = 300 * time.Millisecond
	a.LeaseTTL = ttl
	var cause error
	var elapsed time.Duration
	begun := time.Now()
	a.Step(t.Context(), func(ctx context.Context) (string, error) {
		select {
		case <-ctx.Done():
			elapsed = time.Since(begun)
			cause = context.Cause(ctx)
		case <-time.After(5 * time.Second):
		}
		return "", cause
	})
	if !errors.Is(cause, ErrLeaseLost) {
		t.Fatalf("pass context cause = %v, want ErrLeaseLost", cause)
	}
	if elapsed >= ttl {
		t.Fatalf("the pass was stopped %s after the claim, not before the %s lease could run out", elapsed, ttl)
	}
}

// A renewal that hangs does not hold up the end of the pass: the renewer is
// cancelled before it is waited for.
func TestLoopEndsAPassWithoutWaitingForAHungRenewal(t *testing.T) {
	m, c := &memory{renewHang: true, renewStarted: make(chan struct{}, 1)}, &clock{t: noon}
	a, _ := newLoop(m, c, "10m")
	a.LeaseTTL = time.Hour // a renewal's own deadline is nearly an hour away
	a.renewEvery = 10 * time.Millisecond
	stepped := make(chan struct{})
	go func() {
		defer close(stepped)
		a.Step(t.Context(), func(context.Context) (string, error) {
			<-m.renewStarted
			return OutcomeOK, nil
		})
	}()
	select {
	case <-stepped:
	case <-time.After(5 * time.Second):
		t.Fatal("Step waited for a hung renewal after the pass had ended")
	}
	if got := m.runs(); len(got) != 1 || got[0] != OutcomeOK {
		t.Fatalf("recorded = %v, want [ok]", got)
	}
}

// waitLive waits for the running pass to renew its lease to at least until.
func waitLive(t *testing.T, m *memory, c *clock, until time.Time) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		m.mu.Lock()
		live := !m.expires.Before(until) && m.expires.After(c.now())
		m.mu.Unlock()
		if live {
			return
		}
		select {
		case <-m.renewed:
		case <-time.After(5 * time.Millisecond):
		case <-deadline:
			t.Fatal("the running pass never renewed its lease")
		}
	}
}
