package repos

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/libs/go/routines"
	schedulingapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/scheduling"
)

// TestPlatformRoutinesStorePostgres is a real PostgreSQL gate for the routine
// registry (#1369): registration keeps what an operator set, an edit is
// validated against the routine's own minimum, and only one replica wins a
// slot.
func TestPlatformRoutinesStorePostgres(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	const id = "scheduler.postgres.routine.v1"
	cleanup := func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM elitea_runtime.platform_routines WHERE id = $1`, id)
	}
	cleanup()
	t.Cleanup(cleanup)

	store := routines.NewStore(pool)
	definition := routines.Definition{
		ID: id, Owner: routines.OwnerScheduler, Description: "probe",
		DefaultSchedule: "1h", MinInterval: time.Minute,
	}
	if err := store.Register(ctx, definition); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()

	// Too fast, and not a schedule at all: refused, nothing stored.
	disabled := false
	fast := "10s"
	if _, err := store.Update(ctx, id, routines.Patch{Schedule: &fast, Actor: "op"}, now); !errors.Is(err, routines.ErrBelowMinimum) {
		t.Fatalf("a schedule below the minimum: got %v", err)
	}
	junk := "whenever"
	if _, err := store.Update(ctx, id, routines.Patch{Schedule: &junk, Actor: "op"}, now); !errors.Is(err, routines.ErrInvalidSchedule) {
		t.Fatalf("an unparseable schedule: got %v", err)
	}
	if _, err := store.Update(ctx, "no.such.routine", routines.Patch{Enabled: &disabled}, now); !errors.Is(err, routines.ErrNotFound) {
		t.Fatalf("an unknown routine: got %v", err)
	}

	override := "30m"
	row, err := store.Update(ctx, id, routines.Patch{Schedule: &override, Enabled: &disabled, Actor: "op@example.test"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if row.Enabled || row.EffectiveSchedule() != "30m" || row.UpdatedBy == nil || *row.UpdatedBy != "op@example.test" {
		t.Fatalf("stored row = %+v", row)
	}

	// A restart re-registers: the code's default may change, the operator's word stays.
	definition.DefaultSchedule = "2h"
	if err := store.Register(ctx, definition); err != nil {
		t.Fatal(err)
	}
	row, err = store.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.Enabled || row.EffectiveSchedule() != "30m" || row.DefaultSchedule != "2h" {
		t.Fatalf("after re-register = %+v", row)
	}

	// The controls the platform scheduler reads see the same row.
	controls, err := NewPlatformRoutineControls(pool)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := controls.Resolve(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if control, ok := resolved[id]; !ok || control.Enabled || control.Schedule == nil {
		t.Fatalf("resolved control = %+v (present=%v)", resolved[id], ok)
	}
	var _ schedulingapp.Controls = controls

	// Back on, back to the default: one replica claims the slot, the other loses it.
	enabled := true
	reset := ""
	row, err = store.Update(ctx, id, routines.Patch{Enabled: &enabled, Schedule: &reset, Actor: "op"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if !row.Enabled || row.Overridden() || row.NextRunAt == nil {
		t.Fatalf("after reset = %+v", row)
	}
	expected := row.NextRunAt
	next := expected.Add(2 * time.Hour)
	_, first, err := store.Acquire(ctx, id, expected, next, "a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	_, second, err := store.Acquire(ctx, id, expected, next, "b", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !first || second {
		t.Fatalf("claims: first=%v second=%v, want exactly one winner", first, second)
	}
}

// newRoutineForLeaseTest registers a fresh one-second routine and removes it
// afterwards.
func newRoutineForLeaseTest(t *testing.T, pool *pgxpool.Pool, id string) routines.Definition {
	t.Helper()
	definition := routines.Definition{
		ID: id, Owner: routines.OwnerScheduler, Description: "lease probe", DefaultSchedule: "1s",
	}
	if err := routines.NewStore(pool).Register(context.Background(), definition); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(),
			`DELETE FROM elitea_runtime.platform_routines WHERE id = $1`, id); err != nil {
			t.Errorf("remove routine %s: %v", id, err)
		}
	})
	return definition
}

// #1414 against the real registry: two loops, each with its own pool (two
// replicas), on one routine whose pass outlasts its one-second interval and
// its lease's length. The pass renews its lease, so the second replica never
// starts the routine while the first one's pass runs, however often it
// polls; it runs only after the pass has ended and the slot planned from its
// end is due.
func TestPlatformRoutineLeaseHoldsAPassThatOutlastsItsIntervalPostgres(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	other, err := pgxpool.NewWithConfig(context.Background(), pool.Config().Copy())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(other.Close)
	const id = "scheduler.postgres.lease.outlast.v1"
	definition := newRoutineForLeaseTest(t, pool, id)

	const ttl = 300 * time.Millisecond
	a := &routines.Loop{Backend: routines.NewStore(pool), Def: definition, Poll: 50 * time.Millisecond,
		LeaseTTL: ttl, Holder: "replica-a"}
	b := &routines.Loop{Backend: routines.NewStore(other), Def: definition, Poll: 50 * time.Millisecond,
		LeaseTTL: ttl, Holder: "replica-b"}

	var runsA, runsB, concurrent, overlap atomic.Int32
	passOf := func(runs *atomic.Int32, hold time.Duration) routines.Pass {
		return func(ctx context.Context) (string, error) {
			runs.Add(1)
			if concurrent.Add(1) > 1 {
				overlap.Add(1)
			}
			defer concurrent.Add(-1)
			select {
			case <-time.After(hold):
			case <-ctx.Done():
				return "", context.Cause(ctx)
			}
			return routines.OutcomeOK, nil
		}
	}

	// a's pass lasts 2.5 s: two and a half intervals and over eight lease
	// lengths. b polls the whole time.
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.Step(context.Background(), passOf(&runsA, 2500*time.Millisecond))
	}()
	waitFor(t, func() bool { return runsA.Load() == 1 })
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		b.Step(context.Background(), passOf(&runsB, 0))
		time.Sleep(50 * time.Millisecond)
	}
	if runsB.Load() != 0 {
		t.Fatalf("replica b started %d passes while a's pass was live", runsB.Load())
	}
	<-done

	row, err := routines.NewStore(pool).Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if row.LastOutcome == nil || *row.LastOutcome != routines.OutcomeOK || row.NextRunAt == nil {
		t.Fatalf("after a's pass: %+v", row)
	}
	// The slot planned from the end of a's pass falls due and b takes it.
	deadline = time.Now().Add(5 * time.Second)
	for runsB.Load() == 0 && time.Now().Before(deadline) {
		b.Step(context.Background(), passOf(&runsB, 0))
		time.Sleep(50 * time.Millisecond)
	}
	if runsA.Load() != 1 || runsB.Load() != 1 || overlap.Load() != 0 {
		t.Fatalf("runs a=%d b=%d overlap=%d, want 1, 1 and 0", runsA.Load(), runsB.Load(), overlap.Load())
	}
}

// #1414: a holder that crashed mid-pass (it acquired and then neither renewed
// nor released) keeps the routine from running while its lease is live; once
// the lease runs out on the database's clock, another replica takes the
// routine over, and the crashed holder's stale token can no longer renew or
// release, so it cannot overwrite the take-over's outcome.
func TestPlatformRoutineLeaseExpiresForACrashedHolderPostgres(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	store := routines.NewStore(pool)
	const id = "scheduler.postgres.lease.crash.v1"
	definition := newRoutineForLeaseTest(t, pool, id)

	crashed, ok, err := store.Acquire(ctx, id, nil, time.Now().Add(-time.Second), "crashed", 1500*time.Millisecond)
	if err != nil || !ok {
		t.Fatalf("crashed holder's acquire: ok=%v err=%v", ok, err)
	}
	row, err := store.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}

	// The slot is already due, but the lease is live.
	if _, ok, err := store.Acquire(ctx, id, row.NextRunAt, time.Now().Add(time.Second), "b", time.Second); err != nil || ok {
		t.Fatalf("acquire under a live lease: ok=%v err=%v, want refused", ok, err)
	}
	b := &routines.Loop{Backend: store, Def: definition, Poll: 50 * time.Millisecond,
		LeaseTTL: time.Second, Holder: "replica-b"}
	var runs atomic.Int32
	pass := func(context.Context) (string, error) { runs.Add(1); return routines.OutcomeOK, nil }
	b.Step(ctx, pass)
	if runs.Load() != 0 {
		t.Fatal("replica b ran the routine while the crashed holder's lease was live")
	}

	deadline := time.Now().Add(5 * time.Second)
	for runs.Load() == 0 && time.Now().Before(deadline) {
		b.Step(ctx, pass)
		time.Sleep(50 * time.Millisecond)
	}
	if runs.Load() != 1 {
		t.Fatalf("runs = %d, want 1: the routine must be taken over once the lease expires", runs.Load())
	}
	if err := store.Renew(ctx, id, crashed, time.Minute); !errors.Is(err, routines.ErrLeaseLost) {
		t.Fatalf("stale renew: %v, want ErrLeaseLost", err)
	}
	if err := store.Release(ctx, id, crashed, routines.Completion{RanAt: time.Now(), Outcome: routines.OutcomeError}); !errors.Is(err, routines.ErrLeaseLost) {
		t.Fatalf("stale release: %v, want ErrLeaseLost", err)
	}
	row, err = store.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.LastOutcome == nil || *row.LastOutcome != routines.OutcomeOK {
		t.Fatalf("last_outcome = %v: the crashed holder overwrote the take-over's outcome", row.LastOutcome)
	}
	var expires *time.Time
	if err := pool.QueryRow(ctx, `SELECT lease_expires_at FROM elitea_runtime.platform_routines WHERE id = $1`,
		id).Scan(&expires); err != nil {
		t.Fatal(err)
	}
	if expires != nil {
		t.Fatalf("lease_expires_at = %v after the take-over's release, want NULL", expires)
	}
}

// Expiry is final: a lease that ran out cannot be renewed even though nobody
// has taken the routine over yet, because another replica may be acquiring it
// at that moment.
func TestPlatformRoutineLeaseCannotBeRenewedAfterItExpiresPostgres(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	store := routines.NewStore(pool)
	const id = "scheduler.postgres.lease.expired-renew.v1"
	newRoutineForLeaseTest(t, pool, id)

	lease, ok, err := store.Acquire(ctx, id, nil, time.Now().Add(time.Hour), "a", 200*time.Millisecond)
	if err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}
	if err := store.Renew(ctx, id, lease, 200*time.Millisecond); err != nil {
		t.Fatalf("renew of a live lease: %v", err)
	}
	time.Sleep(400 * time.Millisecond)
	if err := store.Renew(ctx, id, lease, time.Minute); !errors.Is(err, routines.ErrLeaseLost) {
		t.Fatalf("renew after expiry: %v, want ErrLeaseLost", err)
	}
}

// Rolling deploy: a replica of the previous release claims with the old,
// lease-blind UPDATE. Against a live lease the 0175 trigger skips it, so it
// changes nothing and reports no claim; once the lease is gone it claims as
// before. An operator's edit (it stamps updated_at) still re-plans the slot
// under a live lease.
func TestPlatformRoutineLeaseRefusesAnOldStyleClaimPostgres(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	store := routines.NewStore(pool)
	const id = "scheduler.postgres.lease.old-claim.v1"
	newRoutineForLeaseTest(t, pool, id)

	held := time.Now().Add(-time.Second).UTC().Truncate(time.Microsecond)
	lease, ok, err := store.Acquire(ctx, id, nil, held, "new-replica", time.Minute)
	if err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}
	oldClaim := func(expected, next time.Time) int64 {
		t.Helper()
		tag, err := pool.Exec(ctx, `
UPDATE elitea_runtime.platform_routines
SET next_run_at = $3
WHERE id = $1 AND enabled AND next_run_at IS NOT DISTINCT FROM $2::timestamptz`, id, expected, next)
		if err != nil {
			t.Fatalf("old-style claim: %v", err)
		}
		return tag.RowsAffected()
	}
	if got := oldClaim(held, held.Add(time.Hour)); got != 0 {
		t.Fatalf("an old-style claim under a live lease affected %d rows, want 0", got)
	}
	row, err := store.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.NextRunAt == nil || !row.NextRunAt.Equal(held) {
		t.Fatalf("next_run_at = %v, want it left at %v", row.NextRunAt, held)
	}

	// An operator's edit is not a claim: it re-plans under a live lease.
	override := "2h"
	if _, err := store.Update(ctx, id, routines.Patch{Schedule: &override, Actor: "op"}, time.Now()); err != nil {
		t.Fatalf("operator edit under a live lease: %v", err)
	}
	row, err = store.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.NextRunAt == nil || row.NextRunAt.Equal(held) {
		t.Fatalf("next_run_at = %v: the operator's edit did not re-plan the slot", row.NextRunAt)
	}

	// The lease's own release, then an old-style claim succeeds as before.
	if err := store.Release(ctx, id, lease, routines.Completion{RanAt: time.Now(), Outcome: routines.OutcomeOK, Held: held}); err != nil {
		t.Fatalf("release: %v", err)
	}
	if got := oldClaim(*row.NextRunAt, row.NextRunAt.Add(time.Hour)); got != 1 {
		t.Fatalf("an old-style claim with no live lease affected %d rows, want 1", got)
	}
}

// failingRenewals is the real store with every renewal failing, as during a
// database outage that the acquisition just missed.
type failingRenewals struct{ *routines.Store }

func (failingRenewals) Renew(context.Context, string, routines.Lease, time.Duration) error {
	return errors.New("connection refused")
}

// When renewals fail, the holder stops its pass while the lease the database
// granted is still live: no other replica can have started yet.
func TestPlatformRoutineLeaseHolderGivesUpBeforeTheLeaseExpiresPostgres(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	const id = "scheduler.postgres.lease.give-up.v1"
	definition := newRoutineForLeaseTest(t, pool, id)
	a := &routines.Loop{Backend: failingRenewals{routines.NewStore(pool)}, Def: definition,
		Poll: 50 * time.Millisecond, LeaseTTL: 600 * time.Millisecond, Holder: "replica-a"}

	var stillLive *bool
	a.Step(ctx, func(passCtx context.Context) (string, error) {
		select {
		case <-passCtx.Done():
		case <-time.After(5 * time.Second):
			return "", errors.New("the pass was never stopped")
		}
		var live bool
		if err := pool.QueryRow(ctx, `
SELECT lease_expires_at > clock_timestamp() FROM elitea_runtime.platform_routines WHERE id = $1`,
			id).Scan(&live); err != nil {
			return "", err
		}
		stillLive = &live
		return "", context.Cause(passCtx)
	})
	if stillLive == nil {
		t.Fatal("the pass was never stopped")
	}
	if !*stillLive {
		t.Fatal("the holder gave up only after its lease had expired in the database")
	}
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 5s")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
