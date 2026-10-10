package runtimecomposition

// The project-deletion reconciler (#1211).
//
// A project delete removes the project row in one transaction and records what
// is left to clean up in the cleanup journal (centry.project_deletions). It then
// runs the cleanup steps itself; a step can fail, the process can die. The
// journal row stays incomplete, and this loop finishes it: it claims incomplete
// rows older than a grace period and past their backoff, and resumes their
// remaining steps.
//
// The journal row is the single source of truth. The claim is a short
// FOR UPDATE SKIP LOCKED transaction that stamps a lease on the rows
// (projectprovisioning.ClaimStaleDeletions), so concurrent replicas never work
// the same row and no lock or connection is held while the steps run. The
// backoff is stored in the row (attempts, next_attempt_at), not in memory, so a
// restart or another replica keeps it.
//
// Shape (the same as index_manual_stop_cleanup_reconciler.go): a bounded batch
// per pass and a jittered interval so replicas do not tick in step.

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"sync/atomic"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/projectprovisioning"
)

const (
	// DefaultProjectDeletionGracePeriod is how old a journal row must be before
	// the reconciler touches it, so it never competes with the delete that
	// wrote it and is still running its cleanup (that delete also holds the
	// row's lease).
	DefaultProjectDeletionGracePeriod = 5 * time.Minute
	// DefaultProjectDeletionInterval is the pause between passes.
	DefaultProjectDeletionInterval = time.Minute
	// DefaultProjectDeletionBatch bounds the journal rows one pass claims.
	DefaultProjectDeletionBatch = 5
)

// ProjectDeletionJournal is the cleanup journal the reconciler drains.
// *projectprovisioning.Provisioner implements it.
type ProjectDeletionJournal interface {
	ClaimStaleDeletions(ctx context.Context, grace time.Duration, limit int) ([]int64, error)
	ResumeDeletion(ctx context.Context, projectID int64) (projectprovisioning.Result, error)
}

// ProjectDeletionReconcilerConfig tunes the loop. Zero fields take the defaults.
type ProjectDeletionReconcilerConfig struct {
	GracePeriod time.Duration
	Interval    time.Duration
	BatchSize   int
}

// ProjectDeletionReconciler finishes project deletes whose cleanup did not
// complete. Build it with NewProjectDeletionReconciler and run it with Run.
type ProjectDeletionReconciler struct {
	journal ProjectDeletionJournal
	config  ProjectDeletionReconcilerConfig
	logger  *slog.Logger

	wait func(context.Context, time.Duration) error

	finished atomic.Int64
	failed   atomic.Int64
	errored  atomic.Int64
}

// NewProjectDeletionReconciler validates its collaborators and fills the
// configuration defaults.
func NewProjectDeletionReconciler(
	journal ProjectDeletionJournal,
	config ProjectDeletionReconcilerConfig,
	logger *slog.Logger,
) (*ProjectDeletionReconciler, error) {
	if journal == nil {
		return nil, errors.New("project deletion reconciler: the deletion journal is required")
	}
	if config.GracePeriod < 0 || config.Interval < 0 || config.BatchSize < 0 {
		return nil, errors.New("project deletion reconciler: configuration is invalid")
	}
	if config.GracePeriod == 0 {
		config.GracePeriod = DefaultProjectDeletionGracePeriod
	}
	if config.Interval == 0 {
		config.Interval = DefaultProjectDeletionInterval
	}
	if config.BatchSize == 0 {
		config.BatchSize = DefaultProjectDeletionBatch
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &ProjectDeletionReconciler{
		journal: journal,
		config:  config,
		logger:  logger,
		wait:    waitCurrentIndexMetaTerminalReconciler,
	}, nil
}

// Finished is the number of journal rows the reconciler has completed.
func (r *ProjectDeletionReconciler) Finished() int64 { return r.finished.Load() }

// Failed is the number of resumed cleanups that left a step undone (the row
// backs off and is retried).
func (r *ProjectDeletionReconciler) Failed() int64 { return r.failed.Load() }

// Errors is the number of passes that could not claim rows at all.
func (r *ProjectDeletionReconciler) Errors() int64 { return r.errored.Load() }

// RunOnce claims up to one batch of stale journal rows and resumes each. It
// returns how many rows it worked. A failed cleanup is logged and counted; the
// journal row records its own backoff. The error is non-nil only when the claim
// itself failed or ctx ended.
func (r *ProjectDeletionReconciler) RunOnce(ctx context.Context) (int, error) {
	ids, err := r.journal.ClaimStaleDeletions(ctx, r.config.GracePeriod, r.config.BatchSize)
	if err != nil {
		return 0, err
	}
	worked := 0
	for _, id := range ids {
		if ctx.Err() != nil {
			// The claimed rows' leases expire and the next pass takes them.
			return worked, ctx.Err()
		}
		worked++
		_, err := r.journal.ResumeDeletion(ctx, id)
		switch {
		case err == nil, errors.Is(err, projectprovisioning.ErrProjectNotFound):
			r.finished.Add(1)
			r.logger.InfoContext(ctx, "finished the cleanup of a deleted project", "project_id", id)
		default:
			r.failed.Add(1)
			r.logger.ErrorContext(ctx, "the cleanup of a deleted project did not finish; the journal retries it after its backoff",
				"project_id", id, "failed_total", r.failed.Load(), "err", err)
		}
	}
	return worked, nil
}

// Run loops until ctx is cancelled. The interval is jittered by up to +-20% so
// replicas do not run their passes in step.
func (r *ProjectDeletionReconciler) Run(ctx context.Context) error {
	if r == nil || r.journal == nil || ctx == nil {
		return errors.New("project deletion reconciler is incomplete")
	}
	for {
		if _, err := r.RunOnce(ctx); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			r.errored.Add(1)
			r.logger.ErrorContext(ctx, "project deletion reconciliation failed", "err", err)
		}
		jitter := 0.8 + 0.4*rand.Float64()
		if err := r.wait(ctx, time.Duration(float64(r.config.Interval)*jitter)); err != nil {
			return err
		}
	}
}

var _ publisherRunner = (*ProjectDeletionReconciler)(nil)
