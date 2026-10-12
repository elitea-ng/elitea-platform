package runtimecomposition

// The toolkit-deletion reconciler over a fake journal. The claim itself and a
// full run against Postgres are tested in toolkitcleanup.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitcleanup"
)

type fakeToolkitJournal struct {
	mu        sync.Mutex
	claimable []int64
	claimErr  error
	failures  map[int64]error
	// unfinished lists ids whose run ends with no error and no completion.
	unfinished map[int64]bool
	graces     []time.Duration
	resumed    []int64
	// outstanding counts rows claimed and not yet resumed: one lease per row
	// means it never exceeds one while a row is being worked.
	outstanding    int
	maxOutstanding int
}

func (f *fakeToolkitJournal) ClaimNext(_ context.Context, grace time.Duration) (int64, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.graces = append(f.graces, grace)
	if f.claimErr != nil {
		return 0, false, f.claimErr
	}
	if len(f.claimable) == 0 {
		return 0, false, nil
	}
	f.outstanding++
	f.maxOutstanding = max(f.maxOutstanding, f.outstanding)
	id := f.claimable[0]
	f.claimable = f.claimable[1:]
	return id, true, nil
}

func (f *fakeToolkitJournal) Resume(_ context.Context, id int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resumed = append(f.resumed, id)
	f.outstanding--
	err := f.failures[id]
	return err == nil && !f.unfinished[id], err
}

func newTestToolkitReconciler(t *testing.T, journal ToolkitDeletionJournal, config ToolkitDeletionReconcilerConfig) *ToolkitDeletionReconciler {
	t.Helper()
	reconciler, err := NewToolkitDeletionReconciler(journal, config, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return reconciler
}

// One pass claims one batch with the configured grace and resumes each row,
// one lease at a time. Closed and still-open rows are counted apart; a row
// that stays open does not stop the batch, and a row that vanished is neither.
func TestToolkitDeletionReconcilerResumesTheClaimedBatch(t *testing.T) {
	t.Parallel()
	journal := &fakeToolkitJournal{
		claimable:  []int64{11, 12, 13, 14, 15},
		failures:   map[int64]error{12: errors.New("engine unavailable"), 14: toolkitcleanup.ErrRowNotFound},
		unfinished: map[int64]bool{13: true},
	}
	reconciler := newTestToolkitReconciler(t, journal, ToolkitDeletionReconcilerConfig{BatchSize: 4})

	worked, err := reconciler.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if worked != 4 {
		t.Fatalf("worked = %d, want the batch of 4", worked)
	}
	if !slices.Equal(journal.resumed, []int64{11, 12, 13, 14}) {
		t.Fatalf("resumed = %v", journal.resumed)
	}
	if journal.maxOutstanding != 1 {
		t.Fatalf("%d rows leased at once, want one", journal.maxOutstanding)
	}
	for _, grace := range journal.graces {
		if grace != 0 {
			t.Fatalf("grace = %v, want none: no request works its own row", grace)
		}
	}
	if reconciler.Finished() != 1 || reconciler.Failed() != 2 {
		t.Fatalf("finished=%d failed=%d, want 1 and 2 (the vanished row counts as neither)",
			reconciler.Finished(), reconciler.Failed())
	}
}

func TestToolkitDeletionReconcilerCountsAFailedClaimAndKeepsRunning(t *testing.T) {
	t.Parallel()
	journal := &fakeToolkitJournal{claimErr: errors.New("connection refused")}
	reconciler := newTestToolkitReconciler(t, journal, ToolkitDeletionReconcilerConfig{Interval: time.Millisecond})
	waits := 0
	reconciler.wait = func(ctx context.Context, _ time.Duration) error {
		waits++
		if waits == 3 {
			return context.Canceled
		}
		return nil
	}
	if err := reconciler.Run(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v", err)
	}
	if reconciler.Errors() != 3 {
		t.Fatalf("errors = %d, want one per pass", reconciler.Errors())
	}
}

func TestToolkitDeletionReconcilerDefaultsAndValidation(t *testing.T) {
	t.Parallel()
	if _, err := NewToolkitDeletionReconciler(nil, ToolkitDeletionReconcilerConfig{}, nil); err == nil {
		t.Fatal("a nil journal was accepted")
	}
	if _, err := NewToolkitDeletionReconciler(&fakeToolkitJournal{}, ToolkitDeletionReconcilerConfig{BatchSize: -1}, nil); err == nil {
		t.Fatal("a negative batch was accepted")
	}
	reconciler := newTestToolkitReconciler(t, &fakeToolkitJournal{}, ToolkitDeletionReconcilerConfig{})
	if reconciler.config.Interval != DefaultToolkitDeletionInterval || reconciler.config.BatchSize != DefaultToolkitDeletionBatch {
		t.Fatalf("defaults not applied: %+v", reconciler.config)
	}
	if reconciler.config.GracePeriod != 0 {
		t.Fatalf("grace = %v, want zero by default", reconciler.config.GracePeriod)
	}
}

func TestToolkitDeletionReconcilerDoesNotCountAnInterruptedRun(t *testing.T) {
	t.Parallel()
	// The process stops while a row is being worked: the journal reports
	// (false, nil) under a done context, and the reconciler counts nothing.
	ctx, cancel := context.WithCancel(context.Background())
	journal := &cancellingToolkitJournal{fakeToolkitJournal: fakeToolkitJournal{claimable: []int64{21}, unfinished: map[int64]bool{21: true}}, cancel: cancel}
	reconciler := newTestToolkitReconciler(t, journal, ToolkitDeletionReconcilerConfig{BatchSize: 2})
	worked, err := reconciler.RunOnce(ctx)
	if worked != 1 || !errors.Is(err, context.Canceled) {
		t.Fatalf("worked=%d err=%v, want the one row and the cancellation", worked, err)
	}
	if reconciler.Failed() != 0 || reconciler.Finished() != 0 {
		t.Fatalf("an interrupted run was counted: finished=%d failed=%d", reconciler.Finished(), reconciler.Failed())
	}
}

// cancellingToolkitJournal stops the process (cancels the context) from inside
// the resume, as a shutdown arriving mid-row would.
type cancellingToolkitJournal struct {
	fakeToolkitJournal
	cancel context.CancelFunc
}

func (c *cancellingToolkitJournal) Resume(ctx context.Context, id int64) (bool, error) {
	c.cancel()
	return c.fakeToolkitJournal.Resume(ctx, id)
}
