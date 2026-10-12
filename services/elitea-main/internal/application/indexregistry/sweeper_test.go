package indexregistry

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	indexingapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexing"
)

type sweepStore struct {
	tombstones  []Tombstone
	claims      int
	purged      []string
	rescheduled []sweepReschedule
	purgeErr    error
}

type sweepReschedule struct {
	indexID string
	at      time.Time
	failed  bool
}

func (s *sweepStore) ClaimTombstones(_ context.Context, _ int, lease time.Duration) ([]Tombstone, error) {
	s.claims++
	out := s.tombstones
	s.tombstones = nil
	// The claim stamps the lease, as the repository does.
	for i := range out {
		if out[i].LeaseUntil.IsZero() {
			out[i].LeaseUntil = t0.Add(lease)
		}
	}
	return out, nil
}

func (s *sweepStore) PurgeClaimed(_ context.Context, tombstone Tombstone) error {
	if tombstone.LeaseUntil.IsZero() {
		return errors.New("purge without a claim")
	}
	if s.purgeErr != nil {
		return s.purgeErr
	}
	s.purged = append(s.purged, tombstone.IndexID)
	return nil
}

func (s *sweepStore) RescheduleClaimed(_ context.Context, tombstone Tombstone, at time.Time, failed bool) error {
	if tombstone.LeaseUntil.IsZero() {
		return errors.New("reschedule without a claim")
	}
	s.rescheduled = append(s.rescheduled, sweepReschedule{tombstone.IndexID, at, failed})
	return nil
}

type sweepVectors struct {
	errs  map[string]error
	calls []indexingapp.IndexVectorNamespace
	// deadlines records the bound each call was given.
	deadlines []time.Duration
}

func (v *sweepVectors) DeleteIndexVectors(ctx context.Context, ns indexingapp.IndexVectorNamespace) error {
	v.calls = append(v.calls, ns)
	if deadline, ok := ctx.Deadline(); ok {
		v.deadlines = append(v.deadlines, time.Until(deadline))
	} else {
		v.deadlines = append(v.deadlines, 0)
	}
	return v.errs[ns.IndexID]
}

func (*sweepVectors) Deferred() bool { return false }

// wrappedVectors decorates another deleter (as a metrics or logging wrapper
// would) and forwards its capability.
type wrappedVectors struct {
	inner indexingapp.IndexVectorDeleter
}

func (w wrappedVectors) DeleteIndexVectors(ctx context.Context, ns indexingapp.IndexVectorNamespace) error {
	return w.inner.DeleteIndexVectors(ctx, ns)
}

func (w wrappedVectors) Deferred() bool { return w.inner.Deferred() }

func newSweeper(t *testing.T, store *sweepStore, vectors indexingapp.IndexVectorDeleter, reported *[]error) *TombstoneSweeper {
	t.Helper()
	sweeper, err := NewTombstoneSweeper(store, vectors, func(err error) { *reported = append(*reported, err) }, 10)
	if err != nil {
		t.Fatal(err)
	}
	sweeper.now = func() time.Time { return t0 }
	sweeper.jitter = func() float64 { return 1 }
	return sweeper
}

func TestSweeperPurgesATombstoneWhoseVectorsAreDeleted(t *testing.T) {
	store := &sweepStore{tombstones: []Tombstone{{IndexID: "a", ProjectID: 7}}}
	vectors := &sweepVectors{}
	var reported []error
	worked, err := newSweeper(t, store, vectors, &reported).RunOnce(context.Background())
	if err != nil || worked != 1 {
		t.Fatalf("worked=%d err=%v", worked, err)
	}
	if len(vectors.calls) != 1 || vectors.calls[0] != (indexingapp.IndexVectorNamespace{ProjectID: 7, IndexID: "a"}) {
		t.Fatalf("deletions = %+v", vectors.calls)
	}
	if len(store.purged) != 1 || store.purged[0] != "a" || len(store.rescheduled) != 0 || len(reported) != 0 {
		t.Fatalf("purged=%v rescheduled=%v reported=%v", store.purged, store.rescheduled, reported)
	}
}

func TestSweeperBacksOffATransientFailureAndKeepsTheTombstone(t *testing.T) {
	store := &sweepStore{tombstones: []Tombstone{{IndexID: "a", ProjectID: 7, Attempts: 2}, {IndexID: "b", ProjectID: 7}}}
	vectors := &sweepVectors{errs: map[string]error{"a": errors.New("vector store unavailable")}}
	var reported []error
	worked, err := newSweeper(t, store, vectors, &reported).RunOnce(context.Background())
	if err != nil || worked != 2 {
		t.Fatalf("worked=%d err=%v", worked, err)
	}
	// One failure does not stop the batch: b was purged.
	if len(store.purged) != 1 || store.purged[0] != "b" {
		t.Fatalf("purged = %v", store.purged)
	}
	// a stays, counted as an attempt, and waits twice the base delay (the third
	// failure: 30s, 60s, 120s).
	if len(store.rescheduled) != 1 || store.rescheduled[0].indexID != "a" || !store.rescheduled[0].failed ||
		!store.rescheduled[0].at.Equal(t0.Add(4*TombstoneBackoffBase)) {
		t.Fatalf("rescheduled = %+v", store.rescheduled)
	}
	if len(reported) != 1 {
		t.Fatalf("reported = %v", reported)
	}
}

func TestTombstoneBackoffDoublesAndIsCapped(t *testing.T) {
	want := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute}
	for i, expected := range want {
		if got := TombstoneBackoff(int32(i + 1)); got != expected {
			t.Fatalf("backoff(%d) = %v, want %v", i+1, got, expected)
		}
	}
	if got := TombstoneBackoff(500); got != TombstoneBackoffMax {
		t.Fatalf("backoff(500) = %v, want the cap %v", got, TombstoneBackoffMax)
	}
}

func TestSweeperRetriesAPurgeFailureWithoutLosingTheTombstone(t *testing.T) {
	store := &sweepStore{tombstones: []Tombstone{{IndexID: "a"}}, purgeErr: errors.New("database down")}
	var reported []error
	if _, err := newSweeper(t, store, &sweepVectors{}, &reported).RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.purged) != 0 || len(store.rescheduled) != 1 || !store.rescheduled[0].failed || len(reported) != 1 {
		t.Fatalf("purged=%v rescheduled=%+v reported=%v", store.purged, store.rescheduled, reported)
	}
}

// While the hook is the deferred one nothing can succeed: the sweeper does not
// even claim, so no attempt is counted and no query runs.
func TestSweeperWithTheDeferredDeleterDoesNoWork(t *testing.T) {
	store := &sweepStore{tombstones: []Tombstone{{IndexID: "a"}}}
	var reported []error
	sweeper := newSweeper(t, store, indexingapp.DeferredIndexVectorDeleter{}, &reported)
	if !sweeper.Deferred() {
		t.Fatal("the deferred deleter must be recognised")
	}
	worked, err := sweeper.RunOnce(context.Background())
	if err != nil || worked != 0 || store.claims != 0 || len(store.tombstones) != 1 ||
		len(store.rescheduled) != 0 || len(store.purged) != 0 || len(reported) != 0 {
		t.Fatalf("worked=%d claims=%d tombstones=%d rescheduled=%v err=%v", worked, store.claims, len(store.tombstones), store.rescheduled, err)
	}
}

// Deferral is the deleter's own declared capability, not its concrete type: a
// pointer to the placeholder and a wrapper around it are deferred too, and a
// wrapper around a real deleter is not.
func TestSweeperAsksTheDeleterWhetherItIsDeferred(t *testing.T) {
	cases := map[string]struct {
		vectors  indexingapp.IndexVectorDeleter
		deferred bool
	}{
		"pointer to the placeholder":  {&indexingapp.DeferredIndexVectorDeleter{}, true},
		"wrapped placeholder":         {wrappedVectors{indexingapp.DeferredIndexVectorDeleter{}}, true},
		"wrapped pointer placeholder": {wrappedVectors{&indexingapp.DeferredIndexVectorDeleter{}}, true},
		"real deleter":                {&sweepVectors{}, false},
		"wrapped real deleter":        {wrappedVectors{&sweepVectors{}}, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			store := &sweepStore{tombstones: []Tombstone{{IndexID: "a"}}}
			var reported []error
			sweeper := newSweeper(t, store, tc.vectors, &reported)
			if sweeper.Deferred() != tc.deferred {
				t.Fatalf("Deferred() = %v, want %v", sweeper.Deferred(), tc.deferred)
			}
			if _, err := sweeper.RunOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			if claimed := store.claims != 0; claimed == tc.deferred {
				t.Fatalf("claims = %d with a deferred=%v deleter", store.claims, tc.deferred)
			}
		})
	}
}

// A deleter that defers per call (not the installed placeholder) is released
// without counting an attempt.
func TestSweeperDoesNotCountADeferralAsAnAttempt(t *testing.T) {
	store := &sweepStore{tombstones: []Tombstone{{IndexID: "a", Attempts: 1}}}
	vectors := &sweepVectors{errs: map[string]error{"a": indexingapp.ErrIndexVectorDeletionDeferred}}
	var reported []error
	if _, err := newSweeper(t, store, vectors, &reported).RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.rescheduled) != 1 || store.rescheduled[0].failed || len(reported) != 0 || len(store.purged) != 0 {
		t.Fatalf("rescheduled=%+v reported=%v purged=%v", store.rescheduled, reported, store.purged)
	}
}

// Every deletion of a run is bounded to its share of the lease, so a batch
// whose deletions all run to their bound still ends inside the lease of its
// first claim: no tombstone is ever worked by two runs at once.
func TestSweeperBoundsEachDeletionToItsShareOfTheLease(t *testing.T) {
	store := &sweepStore{tombstones: []Tombstone{{IndexID: "a"}, {IndexID: "b"}}}
	vectors := &sweepVectors{}
	var reported []error
	sweeper := newSweeper(t, store, vectors, &reported) // batch 10
	if _, err := sweeper.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := TombstoneLease / 11
	for i, got := range vectors.deadlines {
		// time.Until of a deadline set a moment ago: within a second of the share.
		if got <= 0 || got > want || want-got > time.Second {
			t.Fatalf("deletion %d was bounded to %v, want %v (lease %v / (batch 10 + 1))", i, got, want, TombstoneLease)
		}
	}
	if deleteBudget(10)*11 > TombstoneLease || deleteBudget(1)*2 > TombstoneLease {
		t.Fatal("a full batch of bounded deletions outlasts the lease")
	}
}

// A claimed tombstone whose lease has already ended when the run reaches it is
// not touched: another replica may hold it. It is reported, nothing else.
func TestSweeperLeavesATombstoneWhoseLeaseExpired(t *testing.T) {
	store := &sweepStore{tombstones: []Tombstone{
		{IndexID: "expired", LeaseUntil: t0.Add(-time.Second)},
		{IndexID: "held", LeaseUntil: t0.Add(time.Minute)},
	}}
	vectors := &sweepVectors{}
	var reported []error
	worked, err := newSweeper(t, store, vectors, &reported).RunOnce(context.Background())
	if err != nil || worked != 2 {
		t.Fatalf("worked=%d err=%v", worked, err)
	}
	if len(vectors.calls) != 1 || vectors.calls[0].IndexID != "held" {
		t.Fatalf("deletions = %+v, want only the held tombstone", vectors.calls)
	}
	if len(store.purged) != 1 || store.purged[0] != "held" || len(store.rescheduled) != 0 {
		t.Fatalf("purged=%v rescheduled=%+v", store.purged, store.rescheduled)
	}
	if len(reported) != 1 || !errors.Is(reported[0], ErrTombstoneLeaseExpired) {
		t.Fatalf("reported = %v, want the expired lease", reported)
	}
	// With little lease left the deletion's bound shrinks to what is left.
	store = &sweepStore{tombstones: []Tombstone{{IndexID: "late", LeaseUntil: t0.Add(2 * time.Second)}}}
	vectors = &sweepVectors{}
	if _, err := newSweeper(t, store, vectors, &reported).RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(vectors.deadlines) != 1 || vectors.deadlines[0] > 2*time.Second {
		t.Fatalf("deadline = %v, want at most the 2s of lease left", vectors.deadlines)
	}
}

// A refused identity is not an outage: the tombstone is counted and waits the
// longest pause at once, so the store is not polled every 30s with a request
// it will refuse until an operator acts.
func TestSweeperBacksARefusedIdentityOffToTheLongestPauseAtOnce(t *testing.T) {
	store := &sweepStore{tombstones: []Tombstone{{IndexID: "a", ProjectID: 7}}}
	vectors := &sweepVectors{errs: map[string]error{"a": fmt.Errorf("%w: as told", indexingapp.ErrIndexVectorDeletionRefused)}}
	var reported []error
	if _, err := newSweeper(t, store, vectors, &reported).RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.purged) != 0 || len(store.rescheduled) != 1 || !store.rescheduled[0].failed ||
		!store.rescheduled[0].at.Equal(t0.Add(TombstoneBackoffMax)) {
		t.Fatalf("purged=%v rescheduled=%+v", store.purged, store.rescheduled)
	}
	if len(reported) != 1 || !errors.Is(reported[0], indexingapp.ErrIndexVectorDeletionRefused) {
		t.Fatalf("reported = %v", reported)
	}
}
