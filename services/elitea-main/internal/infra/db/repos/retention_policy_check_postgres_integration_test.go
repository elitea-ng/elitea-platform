package repos

// The write check of the retention writes against a real PostgreSQL
// (ADR-0059 R1.2b, issue 2993): each write calls it with the bounds it moves
// from and to, read under the write's locks, and a refused check writes
// nothing. Requires ELITEA_TEST_DATABASE_URL.

import (
	"context"
	"errors"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/retention"
)

var errCheckRefused = errors.New("refused by the check")

// recordingCheck records the move it was called with and refuses when
// refuse is set.
type recordingCheck struct {
	before, after retention.Bounds
	called        bool
	refuse        bool
}

func (c *recordingCheck) check(before, after retention.Bounds) error {
	c.before, c.after, c.called = before, after, true
	if c.refuse {
		return errCheckRefused
	}
	return nil
}

func maxOf(b retention.Bounds) int {
	if b.MaxAgeDays == nil {
		return -1
	}
	return *b.MaxAgeDays
}

func TestRetentionWritesCallTheCheckWithTheirMove(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	store := NewRetentionPolicyRepo(pool)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO centry.project (id) VALUES (1) ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	class := retention.ClassConversation
	if _, err := store.SetPlatformPolicy(ctx, retention.Policy{
		Class: class, MinAgeDays: 30, MaxAgeDays: ptrInt(365), AllowProjectOverride: true,
	}, 1, nil); err != nil {
		t.Fatal(err)
	}

	// A platform save: the stored row to the new one. A refused check
	// leaves the stored row as it was.
	platform := &recordingCheck{refuse: true}
	_, err := store.SetPlatformPolicy(ctx, retention.Policy{
		Class: class, MinAgeDays: 30, MaxAgeDays: ptrInt(90), AllowProjectOverride: true,
	}, 1, platform.check)
	if !errors.Is(err, errCheckRefused) {
		t.Fatalf("refused platform save = %v, want the check's error", err)
	}
	if !platform.called || maxOf(platform.before) != 365 || maxOf(platform.after) != 90 || platform.before.MinAgeDays != 30 {
		t.Fatalf("platform move = %+v -> %+v", platform.before, platform.after)
	}
	if stored, err := store.PlatformPolicy(ctx, class); err != nil || maxOf(retention.Bounds{MaxAgeDays: stored.MaxAgeDays}) != 365 {
		t.Fatalf("after a refused save the platform row = %+v, %v; want it unchanged", stored, err)
	}

	// An override save: the maximum that applies to the project (the
	// platform's, with no override yet) to the new one.
	override := &recordingCheck{}
	if _, err := store.SetProjectOverride(ctx, retention.Policy{
		Class: class, ProjectID: ptrInt64(1), MaxAgeDays: ptrInt(90),
	}, 1, override.check); err != nil {
		t.Fatal(err)
	}
	if maxOf(override.before) != 365 || maxOf(override.after) != 90 {
		t.Fatalf("override move = %+v -> %+v, want 365 -> 90", override.before, override.after)
	}
	refusedOverride := &recordingCheck{refuse: true}
	if _, err := store.SetProjectOverride(ctx, retention.Policy{
		Class: class, ProjectID: ptrInt64(1), MaxAgeDays: ptrInt(60),
	}, 1, refusedOverride.check); !errors.Is(err, errCheckRefused) {
		t.Fatalf("refused override save = %v", err)
	}
	if maxOf(refusedOverride.before) != 90 || maxOf(refusedOverride.after) != 60 {
		t.Fatalf("second override move = %+v -> %+v, want 90 -> 60", refusedOverride.before, refusedOverride.after)
	}

	// An override delete: the override's maximum to the platform maximum it
	// falls back to. A refused check keeps the override.
	deletion := &recordingCheck{refuse: true}
	if err := store.DeleteProjectOverride(ctx, class, 1, deletion.check); !errors.Is(err, errCheckRefused) {
		t.Fatalf("refused delete = %v", err)
	}
	if maxOf(deletion.before) != 90 || maxOf(deletion.after) != 365 {
		t.Fatalf("delete move = %+v -> %+v, want 90 -> 365", deletion.before, deletion.after)
	}
	overrides, err := store.ProjectOverrides(ctx, class)
	if err != nil || len(overrides) != 1 || maxOf(retention.Bounds{MaxAgeDays: overrides[1].MaxAgeDays}) != 90 {
		t.Fatalf("after a refused delete the overrides = %+v, %v; want the 90-day override", overrides, err)
	}
	if err := store.DeleteProjectOverride(ctx, class, 1, nil); err != nil {
		t.Fatalf("delete with no check: %v", err)
	}
}
