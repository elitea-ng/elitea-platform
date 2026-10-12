package indexregistry

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	indexingapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexing"
)

const (
	// TombstoneBackoffBase is the pause after a tombstone's first failed
	// deletion; it doubles with every further failure up to TombstoneBackoffMax.
	TombstoneBackoffBase = 30 * time.Second
	TombstoneBackoffMax  = time.Hour
	// TombstoneLease is how long a claimed tombstone is hidden from other
	// replicas while its deletion runs. A replica that dies mid-deletion leaves
	// the tombstone due again after the lease. One run's deletions all fit in
	// it: each is bounded to TombstoneLease / (batch + 1) (deleteBudget), so
	// even a batch of deletions that each run to their bound ends before the
	// first claim's lease does, and a tombstone is never worked by two runs.
	TombstoneLease = 5 * time.Minute
	// TombstoneActiveRunBound is how long a tombstone defers to a non-terminal
	// execution job of its run, measured from the job's admission. Within it
	// the worker may still be writing the index's vectors and the claim skips
	// the tombstone; past it the job has outlived every bound a run has — the
	// index admission deadline is 24 hours (runtimecomposition's
	// indexDeadlineTTL; a test there pins this constant above it) — so it is
	// presumed dead and the deletion proceeds, logged. Without the bound a job
	// stuck non-terminal (a lost worker, a settle that never landed) would
	// keep the index's vectors for ever.
	TombstoneActiveRunBound = 24*time.Hour + time.Hour
)

// deleteBudget bounds one tombstone's deletion inside a run of batch
// tombstones: the lease shared by batch + 1, so the whole batch, worked in
// sequence, ends inside the lease with a share to spare for the bookkeeping.
func deleteBudget(batch int) time.Duration {
	return TombstoneLease / time.Duration(batch+1)
}

// TombstoneBackoff is the pause after the attempts-th failed deletion
// (attempts >= 1), before jitter.
func TombstoneBackoff(attempts int32) time.Duration {
	delay := TombstoneBackoffBase
	for i := int32(1); i < attempts && delay < TombstoneBackoffMax; i++ {
		delay *= 2
	}
	if delay > TombstoneBackoffMax {
		delay = TombstoneBackoffMax
	}
	return delay
}

// TombstoneSweeperStore is the persistence the sweeper needs. The Postgres
// implementation is infra/db/repos.IndexRegistryRepository.
type TombstoneSweeperStore interface {
	// ClaimTombstones returns up to limit tombstones that are due (never tried,
	// or past their next_attempt_at), oldest first, and pushes each one's
	// next_attempt_at out by lease so no other replica takes it meanwhile.
	// Replicas never claim the same row (FOR UPDATE SKIP LOCKED); no lock is
	// held after it returns.
	// Each returned tombstone carries the lease's end (Tombstone.LeaseUntil)
	// and the store's clock at the claim (Tombstone.ClaimedAt), on the store's
	// clock. A tombstone whose run is still active (its execution job is not
	// terminal and younger than TombstoneActiveRunBound) is not due: its
	// worker may still be writing the vectors, and a deletion now would leave
	// the ones written after it.
	ClaimTombstones(ctx context.Context, limit int, lease time.Duration) ([]Tombstone, error)
	// PurgeClaimed removes a claimed tombstone and its documents, ONLY while
	// the claim stands (the row's next_attempt_at is still the claim's
	// LeaseUntil). A row another run has claimed since is left alone.
	PurgeClaimed(ctx context.Context, tombstone Tombstone) error
	// RescheduleClaimed stores the next attempt time of a claimed tombstone
	// under the same fence. A failed deletion also counts as an attempt; a
	// deferral does not.
	RescheduleClaimed(ctx context.Context, tombstone Tombstone, nextAttemptAt time.Time, failed bool) error
}

// TombstoneSweeper finishes the deletion of indexes whose vectors could not be
// deleted when the user deleted them: it retries IndexVectorDeleter for every
// due tombstone and purges the ones that succeed. The deletion is idempotent,
// so a replayed attempt is harmless.
type TombstoneSweeper struct {
	store   TombstoneSweeperStore
	vectors indexingapp.IndexVectorDeleter
	report  func(error)
	batch   int
	now     func() time.Time
	jitter  func() float64
}

func NewTombstoneSweeper(
	store TombstoneSweeperStore,
	vectors indexingapp.IndexVectorDeleter,
	report func(error),
	batch int,
) (*TombstoneSweeper, error) {
	if store == nil || vectors == nil || report == nil || batch <= 0 || batch > 1000 {
		return nil, errors.New("index tombstone sweeper dependencies are required")
	}
	return &TombstoneSweeper{
		store: store, vectors: vectors, report: report, batch: batch,
		now:    time.Now,
		jitter: func() float64 { return 0.8 + 0.4*rand.Float64() },
	}, nil
}

// Deferred reports that vector deletion has no implementation yet. The sweeper
// then has nothing it could ever succeed at: it does not touch the table, does
// not count attempts, and a runner should not poll.
func (s *TombstoneSweeper) Deferred() bool {
	return s.vectors.Deferred()
}

// RunOnce works one batch and returns how many tombstones it tried. A failure
// of one tombstone is reported and backed off in its row; the error is non-nil
// only when claiming failed or ctx ended.
func (s *TombstoneSweeper) RunOnce(ctx context.Context) (int, error) {
	if s == nil || ctx == nil {
		return 0, errors.New("index tombstone sweeper is incomplete")
	}
	if s.Deferred() {
		return 0, nil
	}
	tombstones, err := s.store.ClaimTombstones(ctx, s.batch, TombstoneLease)
	// The claim moment on THIS clock: every lease is measured from here as a
	// duration (leaseLeft), never by comparing this clock with the store's.
	claimedAt := s.now()
	if err != nil {
		return 0, err
	}
	for i, tombstone := range tombstones {
		if err := ctx.Err(); err != nil {
			return i, err
		}
		s.sweep(ctx, tombstone, claimedAt)
	}
	return len(tombstones), nil
}

// leaseLeft is how much of the tombstone's lease remains, on this process's
// clock: the lease's length on the store's clock (LeaseUntil − ClaimedAt) less
// the time this process has spent since its claim. The two clocks are never
// compared with each other. ok is false for a tombstone that carries no lease.
// A tombstone with a lease but no claim clock (a store that does not report
// one) falls back to the store's absolute end against this clock.
func (s *TombstoneSweeper) leaseLeft(tombstone Tombstone, claimedAt time.Time) (time.Duration, bool) {
	if tombstone.LeaseUntil.IsZero() {
		return 0, false
	}
	if tombstone.ClaimedAt.IsZero() {
		return tombstone.LeaseUntil.Sub(s.now()), true
	}
	return tombstone.LeaseUntil.Sub(tombstone.ClaimedAt) - s.now().Sub(claimedAt), true
}

// ErrTombstoneLeaseExpired reports a claimed tombstone this run did not touch
// because its lease had run out before the run reached it: another replica may
// hold it by now. The budget makes this unreachable in a healthy run; it is
// reported, never acted on.
var ErrTombstoneLeaseExpired = errors.New("index tombstone lease expired before this run reached it")

func (s *TombstoneSweeper) sweep(ctx context.Context, tombstone Tombstone, claimedAt time.Time) {
	// A tombstone whose lease has ended is another run's now (or nobody's
	// until the next claim); this run has no fence on it and leaves it.
	left, leased := s.leaseLeft(tombstone, claimedAt)
	if leased && left <= 0 {
		s.report(fmt.Errorf("%w: index %s", ErrTombstoneLeaseExpired, tombstone.IndexID))
		return
	}
	// One deletion's bound: its share of the lease, and never past the lease
	// itself.
	budget := deleteBudget(s.batch)
	if leased && left < budget {
		budget = left
	}
	deleteCtx, cancel := context.WithTimeout(ctx, budget)
	vectorErr := s.vectors.DeleteIndexVectors(deleteCtx, indexingapp.IndexVectorNamespace{
		ProjectID: tombstone.ProjectID, IndexID: tombstone.IndexID,
	})
	cancel()
	switch {
	case vectorErr == nil:
		if err := s.store.PurgeClaimed(ctx, tombstone); err != nil {
			// The vectors are gone and the deletion is idempotent: the next pass
			// repeats it and purges. Back off like any other failure.
			s.report(err)
			s.reschedule(ctx, tombstone, true, false)
		}
	case errors.Is(vectorErr, indexingapp.ErrIndexVectorDeletionDeferred):
		s.reschedule(ctx, tombstone, false, false)
	case errors.Is(vectorErr, indexingapp.ErrIndexVectorDeletionRefused):
		// The store does not admit this process; only an operator changes
		// that. Reported, counted, and waited out at the longest pause rather
		// than polled up to it.
		s.report(vectorErr)
		s.reschedule(ctx, tombstone, true, true)
	default:
		if ctx.Err() == nil {
			s.report(vectorErr)
		}
		s.reschedule(ctx, tombstone, true, false)
	}
}

// reschedule stores the tombstone's next attempt: the backoff of its attempts
// after a failure (or the longest pause at once, for a refusal), the base
// pause after a deferral.
func (s *TombstoneSweeper) reschedule(ctx context.Context, tombstone Tombstone, failed, longest bool) {
	attempts := tombstone.Attempts
	if failed {
		attempts++
	}
	delay := TombstoneBackoff(attempts)
	switch {
	case longest:
		delay = TombstoneBackoffMax
	case !failed:
		delay = TombstoneBackoffBase
	}
	delay = time.Duration(float64(delay) * s.jitter())
	if err := s.store.RescheduleClaimed(ctx, tombstone, s.now().Add(delay), failed); err != nil && ctx.Err() == nil {
		s.report(err)
	}
}
