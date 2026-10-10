package runtimecomposition

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/projectprovisioning"
)

// fakeDeprovisioner records the projects it is asked to delete, answers a
// configured error per project, and (like the real delete) removes the row when
// it succeeds.
type fakeDeprovisioner struct {
	pool *pgxpool.Pool

	mu     sync.Mutex
	calls  []int64
	errors map[int64]error
}

func (f *fakeDeprovisioner) Deprovision(
	ctx context.Context, projectID int64, _ ...projectprovisioning.DeprovisionOption,
) (projectprovisioning.Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, projectID)
	err := f.errors[projectID]
	f.mu.Unlock()
	if err != nil {
		return projectprovisioning.Result{}, err
	}
	_, execErr := f.pool.Exec(ctx, `DELETE FROM centry.project WHERE id = $1`, projectID)
	return projectprovisioning.Result{}, execErr
}

func (f *fakeDeprovisioner) called() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func (f *fakeDeprovisioner) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
}

func newProjectDeletionFixture(t *testing.T, batch int) (*ProjectDeletionReconciler, *fakeDeprovisioner, *pgxpool.Pool, *time.Time) {
	t.Helper()
	pool := newArtifactRetentionPostgresPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
CREATE SCHEMA centry;
CREATE TABLE centry.project (id INTEGER PRIMARY KEY, deleting_at TIMESTAMPTZ);
-- 1: live; 2: tombstoned 1 minute ago; 3: tombstoned 10 minutes ago;
-- 4: tombstoned 20 minutes ago; 5: tombstoned 30 minutes ago.
INSERT INTO centry.project (id, deleting_at) VALUES
  (1, NULL),
  (2, now() - interval '1 minute'),
  (3, now() - interval '10 minutes'),
  (4, now() - interval '20 minutes'),
  (5, now() - interval '30 minutes')`); err != nil {
		t.Fatal(err)
	}
	deprovisioner := &fakeDeprovisioner{pool: pool, errors: map[int64]error{}}
	reconciler, err := NewProjectDeletionReconciler(pool, deprovisioner,
		ProjectDeletionReconcilerConfig{GracePeriod: 5 * time.Minute, Interval: time.Minute, BatchSize: batch},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Now()
	reconciler.now = func() time.Time { return clock }
	return reconciler, deprovisioner, pool, &clock
}

// Only tombstones older than the grace period are finished, oldest first.
func TestProjectDeletionReconcilerFinishesOnlyStaleTombstones(t *testing.T) {
	reconciler, deprovisioner, pool, _ := newProjectDeletionFixture(t, 10)
	ctx := context.Background()

	attempted, err := reconciler.RunOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := []int64{5, 4, 3}; !slices.Equal(deprovisioner.called(), want) || attempted != 3 {
		t.Fatalf("deleted %v (attempted %d), want %v: not the live project 1 or the fresh tombstone 2", deprovisioner.called(), attempted, want)
	}
	var left []int64
	rows, err := pool.Query(ctx, `SELECT id FROM centry.project ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		left = append(left, id)
	}
	if !slices.Equal(left, []int64{1, 2}) {
		t.Fatalf("rows left = %v, want [1 2]", left)
	}
	if reconciler.Finished() != 3 || reconciler.Failures() != 0 {
		t.Fatalf("finished=%d failures=%d, want 3 and 0", reconciler.Finished(), reconciler.Failures())
	}
}

func TestProjectDeletionReconcilerBoundsTheBatch(t *testing.T) {
	reconciler, deprovisioner, _, _ := newProjectDeletionFixture(t, 2)
	if _, err := reconciler.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if want := []int64{5, 4}; !slices.Equal(deprovisioner.called(), want) {
		t.Fatalf("one pass deleted %v, want the two oldest %v", deprovisioner.called(), want)
	}
}

// A failing delete is logged, counted and backed off; it does not starve the
// projects behind it, and it is retried once the backoff has passed.
func TestProjectDeletionReconcilerBacksOffAFailingProject(t *testing.T) {
	reconciler, deprovisioner, _, clock := newProjectDeletionFixture(t, 1)
	ctx := context.Background()
	deprovisioner.errors[5] = errors.New("vault is down")

	// Pass 1 picks the oldest (5), which fails.
	if _, err := reconciler.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if reconciler.Failures() != 1 || !slices.Equal(deprovisioner.called(), []int64{5}) {
		t.Fatalf("pass 1: failures=%d calls=%v", reconciler.Failures(), deprovisioner.called())
	}
	// Pass 2: 5 is backing off, so the batch of ONE goes to the next project
	// instead of being spent on the same failing one.
	deprovisioner.reset()
	if _, err := reconciler.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(deprovisioner.called(), []int64{4}) {
		t.Fatalf("pass 2 deleted %v, want [4] (5 is backing off)", deprovisioner.called())
	}
	// Past the backoff, 5 is tried again, and fails again with a longer delay.
	*clock = clock.Add(2 * time.Minute)
	deprovisioner.reset()
	if _, err := reconciler.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(deprovisioner.called(), []int64{5}) || reconciler.Failures() != 2 {
		t.Fatalf("pass 3: calls=%v failures=%d, want [5] and 2", deprovisioner.called(), reconciler.Failures())
	}
	if first, second := reconciler.backoff[5].retryAt.Sub(*clock), time.Minute; first <= second {
		t.Fatalf("second failure backs off %v, want more than the first (%v)", first, second)
	}
	// It heals: the failure clears and the project is finished.
	delete(deprovisioner.errors, 5)
	*clock = clock.Add(time.Hour)
	deprovisioner.reset()
	if _, err := reconciler.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if reconciler.Finished() < 2 || len(reconciler.backoff) != 0 {
		t.Fatalf("finished=%d backoff=%v, want 5 finished and no backoff left", reconciler.Finished(), reconciler.backoff)
	}
}

// Another replica (or the request) finishing the delete is not a failure.
func TestProjectDeletionReconcilerLeavesADeleteInProgressAlone(t *testing.T) {
	reconciler, deprovisioner, _, _ := newProjectDeletionFixture(t, 10)
	deprovisioner.errors[5] = projectprovisioning.ErrProjectDeletionInProgress
	if _, err := reconciler.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if reconciler.Failures() != 0 {
		t.Fatalf("a delete in progress elsewhere was counted as a failure: %d", reconciler.Failures())
	}
	if len(reconciler.backoff) != 0 {
		t.Fatalf("a delete in progress elsewhere was backed off: %v", reconciler.backoff)
	}
}

// SKIP LOCKED: a project row someone holds (a fence, a delete step) is not
// waited for.
func TestProjectDeletionReconcilerSkipsRowsThatAreLocked(t *testing.T) {
	reconciler, deprovisioner, pool, _ := newProjectDeletionFixture(t, 10)
	ctx := context.Background()

	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if _, err := holder.Exec(ctx, `SELECT id FROM centry.project WHERE id = 5 FOR UPDATE`); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := reconciler.RunOnce(ctx)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the reconciler waited for a row someone else holds")
	}
	if want := []int64{4, 3}; !slices.Equal(deprovisioner.called(), want) {
		t.Fatalf("deleted %v, want %v (5 is locked)", deprovisioner.called(), want)
	}
}

func TestProjectDeletionReconcilerRunStopsWithTheContext(t *testing.T) {
	reconciler, _, _, _ := newProjectDeletionFixture(t, 10)
	ctx, cancel := context.WithCancel(context.Background())
	reconciler.wait = func(ctx context.Context, _ time.Duration) error {
		cancel()
		return ctx.Err()
	}
	if err := reconciler.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want context.Canceled", err)
	}
}
