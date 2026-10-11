package indexregistry

import (
	"context"
	"errors"
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
	// the tombstone due again after the lease.
	TombstoneLease = 5 * time.Minute
)

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
	ClaimTombstones(ctx context.Context, limit int, lease time.Duration) ([]Tombstone, error)
	// PurgeDeleted removes a tombstone and its documents.
	PurgeDeleted(ctx context.Context, indexID string) error
	// RescheduleTombstone stores the next attempt time of a tombstone. A
	// failed deletion also counts as an attempt; a deferral does not.
	RescheduleTombstone(ctx context.Context, indexID string, nextAttemptAt time.Time, failed bool) error
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
	_, deferred := s.vectors.(indexingapp.DeferredIndexVectorDeleter)
	return deferred
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
	if err != nil {
		return 0, err
	}
	for i, tombstone := range tombstones {
		if err := ctx.Err(); err != nil {
			return i, err
		}
		s.sweep(ctx, tombstone)
	}
	return len(tombstones), nil
}

func (s *TombstoneSweeper) sweep(ctx context.Context, tombstone Tombstone) {
	vectorErr := s.vectors.DeleteIndexVectors(ctx, indexingapp.IndexVectorNamespace{
		ProjectID: tombstone.ProjectID, IndexID: tombstone.IndexID,
	})
	switch {
	case vectorErr == nil:
		if err := s.store.PurgeDeleted(ctx, tombstone.IndexID); err != nil {
			// The vectors are gone and the deletion is idempotent: the next pass
			// repeats it and purges. Back off like any other failure.
			s.report(err)
			s.reschedule(ctx, tombstone, true)
		}
	case errors.Is(vectorErr, indexingapp.ErrIndexVectorDeletionDeferred):
		s.reschedule(ctx, tombstone, false)
	default:
		if ctx.Err() == nil {
			s.report(vectorErr)
		}
		s.reschedule(ctx, tombstone, true)
	}
}

func (s *TombstoneSweeper) reschedule(ctx context.Context, tombstone Tombstone, failed bool) {
	attempts := tombstone.Attempts
	if failed {
		attempts++
	}
	delay := TombstoneBackoff(attempts)
	if !failed {
		delay = TombstoneBackoffBase
	}
	delay = time.Duration(float64(delay) * s.jitter())
	if err := s.store.RescheduleTombstone(ctx, tombstone.IndexID, s.now().Add(delay), failed); err != nil && ctx.Err() == nil {
		s.report(err)
	}
}
