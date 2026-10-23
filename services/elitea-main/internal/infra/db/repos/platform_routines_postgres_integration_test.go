package repos

import (
	"context"
	"errors"
	"testing"
	"time"

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
	first, err := store.Claim(ctx, id, expected, next)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Claim(ctx, id, expected, next)
	if err != nil {
		t.Fatal(err)
	}
	if !first || second {
		t.Fatalf("claims: first=%v second=%v, want exactly one winner", first, second)
	}
}
