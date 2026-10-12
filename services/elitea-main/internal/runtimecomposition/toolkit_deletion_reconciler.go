package runtimecomposition

// The toolkit-deletion reconciler (#1244), the sibling of
// project_deletion_reconciler.go over the toolkit cleanup journal
// (centry.toolkit_deletions, internal/application/toolkitcleanup).
//
// A toolkit delete removes the toolkit row in one transaction and, for a
// toolkit whose application engine keeps data per toolkit (an Inventory
// graph), records the engine-side deletion owed in the journal. The request
// never makes that call; this loop does: it claims incomplete rows past their
// backoff, one at a time under a lease (FOR UPDATE SKIP LOCKED), asks the
// engine, and lets the journal close or back off the row. The backoff lives in
// the row, so a restart or another replica keeps it.
//
// It is a sibling rather than the project reconciler generalised: the two
// journals key their rows differently (a project id against a surrogate id and
// its generation) and report different terminal errors, and the project one is
// covered by #1245's tests as it stands. What the two loops share word for word
// — the jittered pass loop — is runReconcilerPasses.

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitcleanup"
)

const (
	// DefaultToolkitDeletionInterval is the pause between passes. A deleted
	// toolkit's graph is therefore gone within about a minute of the delete.
	DefaultToolkitDeletionInterval = time.Minute
	// DefaultToolkitDeletionBatch bounds the journal rows one pass claims.
	DefaultToolkitDeletionBatch = 20
)

// ToolkitDeletionJournal is the cleanup journal the reconciler drains.
// *toolkitcleanup.Journal implements it.
type ToolkitDeletionJournal interface {
	// ClaimNext leases one row and returns the claim (the row's id and its
	// generation at the claim), which Resume must present.
	ClaimNext(ctx context.Context, grace time.Duration) (claim toolkitcleanup.Claim, ok bool, err error)
	// Resume reports completed only when the journal row ended closed.
	Resume(ctx context.Context, claim toolkitcleanup.Claim) (completed bool, err error)
}

// ToolkitDeletionReconcilerConfig tunes the loop. Zero Interval and BatchSize
// take the defaults. GracePeriod is how old a row must be before it is claimed;
// zero means no grace, which is the right value here: no request ever works
// its own row, so there is nothing to keep out of the way of.
type ToolkitDeletionReconcilerConfig struct {
	GracePeriod time.Duration
	Interval    time.Duration
	BatchSize   int
}

// ToolkitDeletionReconciler finishes toolkit deletes whose engine-side deletion
// is still owed. Build it with NewToolkitDeletionReconciler and run it with Run.
type ToolkitDeletionReconciler struct {
	journal ToolkitDeletionJournal
	config  ToolkitDeletionReconcilerConfig
	logger  *slog.Logger

	wait func(context.Context, time.Duration) error

	finished atomic.Int64
	failed   atomic.Int64
	errored  atomic.Int64
}

// NewToolkitDeletionReconciler validates its collaborators and fills the
// configuration defaults.
func NewToolkitDeletionReconciler(
	journal ToolkitDeletionJournal,
	config ToolkitDeletionReconcilerConfig,
	logger *slog.Logger,
) (*ToolkitDeletionReconciler, error) {
	if journal == nil {
		return nil, errors.New("toolkit deletion reconciler: the cleanup journal is required")
	}
	if config.GracePeriod < 0 || config.Interval < 0 || config.BatchSize < 0 {
		return nil, errors.New("toolkit deletion reconciler: configuration is invalid")
	}
	if config.Interval == 0 {
		config.Interval = DefaultToolkitDeletionInterval
	}
	if config.BatchSize == 0 {
		config.BatchSize = DefaultToolkitDeletionBatch
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &ToolkitDeletionReconciler{
		journal: journal,
		config:  config,
		logger:  logger,
		wait:    waitCurrentIndexMetaTerminalReconciler,
	}, nil
}

// Finished is the number of journal rows the reconciler has closed.
func (r *ToolkitDeletionReconciler) Finished() int64 { return r.finished.Load() }

// Failed is the number of resumed rows that stayed open (the row backs off and
// is retried).
func (r *ToolkitDeletionReconciler) Failed() int64 { return r.failed.Load() }

// Errors is the number of passes that could not claim rows at all.
func (r *ToolkitDeletionReconciler) Errors() int64 { return r.errored.Load() }

// RunOnce works up to one batch of journal rows, claiming and resuming them
// one at a time. It returns how many rows it worked. A row that stays open is
// logged and counted; the journal row records its own backoff. The error is
// non-nil only when a claim itself failed or ctx ended.
func (r *ToolkitDeletionReconciler) RunOnce(ctx context.Context) (int, error) {
	worked := 0
	for worked < r.config.BatchSize {
		if err := ctx.Err(); err != nil {
			return worked, err
		}
		claim, ok, err := r.journal.ClaimNext(ctx, r.config.GracePeriod)
		if err != nil {
			return worked, err
		}
		if !ok {
			return worked, nil
		}
		worked++
		completed, err := r.journal.Resume(ctx, claim)
		switch {
		case err == nil && completed:
			r.finished.Add(1)
			r.logger.InfoContext(ctx, "finished the engine-side cleanup of a deleted toolkit", "journal_id", claim.ID, "generation", claim.Generation)
		case errors.Is(err, toolkitcleanup.ErrRowNotFound):
			// The row vanished between the claim and the read: nothing left to
			// finish, nothing finished by this pass.
			r.logger.WarnContext(ctx, "a claimed toolkit deletion has no journal row", "journal_id", claim.ID)
		case errors.Is(err, toolkitcleanup.ErrClaimLost):
			// The row was reopened under the claim (the toolkit id was deleted
			// again): the new obligation is claimed on its own, and this run
			// touched nothing. Not a failure of the row.
			r.logger.InfoContext(ctx, "a toolkit deletion was reopened under its claim; the new obligation is worked on its own",
				"journal_id", claim.ID, "generation", claim.Generation)
		case err == nil && ctx.Err() != nil:
			// Not completed, and the process is stopping: the run released its
			// lease without counting an attempt.
			r.logger.InfoContext(ctx, "the cleanup of a deleted toolkit was interrupted; the journal keeps it open", "journal_id", claim.ID)
		default:
			r.failed.Add(1)
			r.logger.ErrorContext(ctx, "the engine-side cleanup of a deleted toolkit did not finish; the journal retries it after its backoff",
				"journal_id", claim.ID, "generation", claim.Generation, "failed_total", r.failed.Load(), "err", err)
		}
	}
	return worked, nil
}

// Run loops until ctx is cancelled. The interval is jittered by up to +-20% so
// replicas do not run their passes in step (runReconcilerPasses).
func (r *ToolkitDeletionReconciler) Run(ctx context.Context) error {
	if r == nil || r.journal == nil || ctx == nil {
		return errors.New("toolkit deletion reconciler is incomplete")
	}
	return runReconcilerPasses(ctx, r.config.Interval, r.wait, r.RunOnce, func(err error) {
		r.errored.Add(1)
		r.logger.ErrorContext(ctx, "toolkit deletion reconciliation failed", "err", err)
	})
}

var _ publisherRunner = (*ToolkitDeletionReconciler)(nil)
