package runtimecomposition

// The reconciler's own behaviour over a fake journal. The claim itself (grace,
// backoff, lease, SKIP LOCKED) and a full run against Postgres are tested in
// projectprovisioning (TestClaimStaleDeletions,
// TestCleanupFailureLeavesAJournalRowTheReconcilerFinishes).

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/projectprovisioning"
)

type fakeDeletionJournal struct {
	mu        sync.Mutex
	claimable []int64
	claimErr  error
	failures  map[int64]error
	graces    []time.Duration
	resumed   []int64
	// claimedWhileWorking records, for each claim, how many rows were claimed
	// and not yet resumed: one lease per row means it is never above zero.
	claimedWhileWorking []int
	outstanding         int
}

func (f *fakeDeletionJournal) ClaimNextDeletion(_ context.Context, grace time.Duration) (int64, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.graces = append(f.graces, grace)
	if f.claimErr != nil {
		return 0, false, f.claimErr
	}
	if len(f.claimable) == 0 {
		return 0, false, nil
	}
	f.claimedWhileWorking = append(f.claimedWhileWorking, f.outstanding)
	f.outstanding++
	id := f.claimable[0]
	f.claimable = f.claimable[1:]
	return id, true, nil
}

func (f *fakeDeletionJournal) ResumeDeletion(_ context.Context, projectID int64) (projectprovisioning.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resumed = append(f.resumed, projectID)
	f.outstanding--
	return projectprovisioning.Result{ProjectID: projectID}, f.failures[projectID]
}

func newTestDeletionReconciler(t *testing.T, journal ProjectDeletionJournal, config ProjectDeletionReconcilerConfig) *ProjectDeletionReconciler {
	t.Helper()
	reconciler, err := NewProjectDeletionReconciler(journal, config, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return reconciler
}

// One pass claims one batch with the configured grace and resumes each row.
// Finished and failed cleanups are counted apart; a failure does not stop the
// batch.
func TestProjectDeletionReconcilerResumesTheClaimedBatch(t *testing.T) {
	journal := &fakeDeletionJournal{
		claimable: []int64{3, 4, 5, 6},
		failures: map[int64]error{
			4: errors.Join(projectprovisioning.ErrVectorStoreNotDropped),
			5: projectprovisioning.ErrProjectNotFound, // the row is gone: nothing left to do
		},
	}
	reconciler := newTestDeletionReconciler(t, journal,
		ProjectDeletionReconcilerConfig{GracePeriod: 7 * time.Minute, BatchSize: 3})

	worked, err := reconciler.RunOnce(context.Background())
	if err != nil || worked != 3 {
		t.Fatalf("RunOnce = %d, %v; want 3 rows worked", worked, err)
	}
	if !slices.Equal(journal.resumed, []int64{3, 4, 5}) {
		t.Fatalf("resumed %v, want the claimed batch [3 4 5]", journal.resumed)
	}
	if got := journal.graces[0]; got != 7*time.Minute {
		t.Fatalf("claim grace = %v, want the configured one", got)
	}
	// One lease per row: a row is claimed only after the previous one was
	// worked, so no claimed row ever waits in a local queue.
	for i, outstanding := range journal.claimedWhileWorking {
		if outstanding != 0 {
			t.Fatalf("claim %d was made while %d claimed row(s) were still unworked", i, outstanding)
		}
	}
	if reconciler.Finished() != 2 || reconciler.Failed() != 1 {
		t.Fatalf("finished=%d failed=%d, want 2 and 1", reconciler.Finished(), reconciler.Failed())
	}

	// The next pass takes the rest.
	if worked, err := reconciler.RunOnce(context.Background()); err != nil || worked != 1 {
		t.Fatalf("second pass = %d, %v", worked, err)
	}
	if reconciler.Finished() != 3 {
		t.Fatalf("finished=%d, want 3", reconciler.Finished())
	}
}

// A claim that fails is the pass's error; Run counts it and keeps going.
func TestProjectDeletionReconcilerCountsAFailedClaimAndKeepsRunning(t *testing.T) {
	journal := &fakeDeletionJournal{claimErr: errors.New("database is down")}
	reconciler := newTestDeletionReconciler(t, journal, ProjectDeletionReconcilerConfig{})
	if _, err := reconciler.RunOnce(context.Background()); err == nil {
		t.Fatal("RunOnce hid a failed claim")
	}

	ctx, cancel := context.WithCancel(context.Background())
	passes := 0
	reconciler.wait = func(ctx context.Context, _ time.Duration) error {
		passes++
		if passes == 2 {
			cancel()
			return ctx.Err()
		}
		return nil
	}
	if err := reconciler.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want context.Canceled", err)
	}
	if reconciler.Errors() != 2 {
		t.Fatalf("errors = %d, want both failed passes counted", reconciler.Errors())
	}
}

func TestProjectDeletionReconcilerDefaultsAndValidation(t *testing.T) {
	if _, err := NewProjectDeletionReconciler(nil, ProjectDeletionReconcilerConfig{}, nil); err == nil {
		t.Fatal("a reconciler without a journal was built")
	}
	if _, err := NewProjectDeletionReconciler(&fakeDeletionJournal{},
		ProjectDeletionReconcilerConfig{GracePeriod: -time.Second}, nil); err == nil {
		t.Fatal("a negative grace was accepted")
	}
	reconciler := newTestDeletionReconciler(t, &fakeDeletionJournal{}, ProjectDeletionReconcilerConfig{})
	if reconciler.config.GracePeriod != DefaultProjectDeletionGracePeriod ||
		reconciler.config.Interval != DefaultProjectDeletionInterval ||
		reconciler.config.BatchSize != DefaultProjectDeletionBatch {
		t.Fatalf("config = %+v, want the defaults", reconciler.config)
	}
}

func TestProjectDeletionReconcilerRunStopsWithTheContext(t *testing.T) {
	reconciler := newTestDeletionReconciler(t, &fakeDeletionJournal{}, ProjectDeletionReconcilerConfig{})
	ctx, cancel := context.WithCancel(context.Background())
	reconciler.wait = func(ctx context.Context, _ time.Duration) error {
		cancel()
		return ctx.Err()
	}
	if err := reconciler.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want context.Canceled", err)
	}
}
