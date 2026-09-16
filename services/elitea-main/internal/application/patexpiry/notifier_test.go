package patexpiry_test

// The pass's own arithmetic and its stopping rule, with a fake store — the
// half no database can show, because "the store said it already notified this
// one" and "the store failed" are indistinguishable in a produced-row count.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/patexpiry"
)

type fakeStore struct {
	candidates []patexpiry.Candidate
	listErr    error
	// produced answers NotifyPATExpiring in order; a short list repeats its
	// last value.
	produced  []bool
	notifyErr error
	// calls records what was asked for, so a window that never reached the
	// store is visible.
	listedWithin time.Duration
	notifyCalls  int
}

func (f *fakeStore) ListPATsNeedingExpiryNotice(
	_ context.Context, _ time.Time, within time.Duration, _ int32,
) ([]patexpiry.Candidate, error) {
	f.listedWithin = within
	return f.candidates, f.listErr
}

func (f *fakeStore) NotifyPATExpiring(
	_ context.Context, _ patexpiry.Candidate, _ time.Time,
) (bool, error) {
	index := f.notifyCalls
	f.notifyCalls++
	if f.notifyErr != nil {
		return false, f.notifyErr
	}
	if len(f.produced) == 0 {
		return true, nil
	}
	if index >= len(f.produced) {
		index = len(f.produced) - 1
	}
	return f.produced[index], nil
}

func candidates(n int) []patexpiry.Candidate {
	rows := make([]patexpiry.Candidate, 0, n)
	for i := 0; i < n; i++ {
		rows = append(rows, patexpiry.Candidate{TokenID: int64(i + 1), UserID: 1, Name: "key"})
	}
	return rows
}

func TestRunCountsProducedAndSkippedSeparately(t *testing.T) {
	store := &fakeStore{candidates: candidates(3), produced: []bool{true, false, true}}
	notifier, err := patexpiry.New(store)
	if err != nil {
		t.Fatal(err)
	}

	result, err := notifier.Run(context.Background(), time.Now(), time.Hour)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// A single "produced" number would report 2 here and 2 for a pass that
	// examined 2 and skipped none — which is the difference between the dedupe
	// holding and the candidate query having gone blind.
	if result.Examined != 3 || result.Produced != 2 || result.Skipped != 1 {
		t.Errorf("result = %+v, want examined 3, produced 2, skipped 1", result)
	}
}

func TestRunStopsAtTheFirstWriteFailure(t *testing.T) {
	store := &fakeStore{candidates: candidates(4), notifyErr: errors.New("mark could not be written")}
	notifier, err := patexpiry.New(store)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := notifier.Run(context.Background(), time.Now(), time.Hour); err == nil {
		t.Fatal("Run reported success after a write failure")
	}
	// Carrying on past a failed MARK is how a sweep produces the duplicate the
	// mark exists to prevent, so the pass must stop at the first one.
	if store.notifyCalls != 1 {
		t.Errorf("the pass made %d write attempts after the first failure, want 1", store.notifyCalls)
	}
}

func TestRunReportsAFailedRead(t *testing.T) {
	store := &fakeStore{listErr: errors.New("relation does not exist")}
	notifier, err := patexpiry.New(store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := notifier.Run(context.Background(), time.Now(), time.Hour); err == nil {
		t.Fatal("a failed candidate read was reported as an empty pass")
	}
}

func TestRunFallsBackToTheProductionWindow(t *testing.T) {
	for name, within := range map[string]time.Duration{"zero": 0, "negative": -time.Hour} {
		t.Run(name, func(t *testing.T) {
			store := &fakeStore{}
			notifier, err := patexpiry.New(store)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := notifier.Run(context.Background(), time.Now(), within); err != nil {
				t.Fatal(err)
			}
			// A window of nothing would silently announce nobody, for ever.
			if store.listedWithin != patexpiry.DefaultWindow {
				t.Errorf("listed with %v, want the production window %v", store.listedWithin, patexpiry.DefaultWindow)
			}
		})
	}
}

func TestNewRefusesANilStore(t *testing.T) {
	if _, err := patexpiry.New(nil); err == nil {
		t.Fatal("a notifier was built over no store")
	}
}
