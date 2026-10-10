package runtimecomposition

// The project-deletion reconciler (#1211).
//
// A project delete sets the tombstone (centry.project.deleting_at) before it
// removes anything and then walks the removal steps. A step can fail, the
// process can die, a client can give up: the project is left tombstoned,
// refusing new work, and nobody is coming back to click Delete again. The
// tombstone is never cleared, so the only way out of that state is to finish the
// delete. This loop does that: it re-runs Deprovision for every project whose
// tombstone is older than a grace period, with a per-project backoff after a
// failure.
//
// Shape (the same as index_manual_stop_cleanup_reconciler.go): a bounded batch
// per pass, FOR UPDATE SKIP LOCKED on the candidate read so a project row a
// fence or another delete holds is left to its holder, a jittered interval so
// replicas do not tick in step. Concurrent replicas are safe by construction:
// Deprovision holds a per-project advisory lock, and the loser of that race
// (ErrProjectDeletionInProgress) simply moves on.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/projectprovisioning"
)

const (
	// DefaultProjectDeletionGracePeriod is how old a tombstone must be before the
	// reconciler touches it, so it never competes with the request that is still
	// running the delete.
	DefaultProjectDeletionGracePeriod = 5 * time.Minute
	// DefaultProjectDeletionInterval is the pause between passes.
	DefaultProjectDeletionInterval = time.Minute
	// DefaultProjectDeletionBatch bounds the projects one pass finishes.
	DefaultProjectDeletionBatch = 5
	// maxProjectDeletionBackoff caps the per-project retry delay.
	maxProjectDeletionBackoff = time.Hour
)

// ProjectDeprovisioner is the delete the reconciler re-runs.
type ProjectDeprovisioner interface {
	Deprovision(ctx context.Context, projectID int64, options ...projectprovisioning.DeprovisionOption) (projectprovisioning.Result, error)
}

// ProjectDeletionReconcilerConfig tunes the loop. Zero fields take the defaults.
type ProjectDeletionReconcilerConfig struct {
	GracePeriod time.Duration
	Interval    time.Duration
	BatchSize   int
}

type projectDeletionBackoff struct {
	failures int
	retryAt  time.Time
}

// ProjectDeletionReconciler finishes deletes that were started and not
// completed. Build it with NewProjectDeletionReconciler and run it with Run.
type ProjectDeletionReconciler struct {
	pool          *pgxpool.Pool
	deprovisioner ProjectDeprovisioner
	config        ProjectDeletionReconcilerConfig
	logger        *slog.Logger

	now  func() time.Time
	wait func(context.Context, time.Duration) error

	failures atomic.Int64
	finished atomic.Int64

	mu      sync.Mutex
	backoff map[int64]projectDeletionBackoff
}

// NewProjectDeletionReconciler validates its collaborators and fills the
// configuration defaults.
func NewProjectDeletionReconciler(
	pool *pgxpool.Pool,
	deprovisioner ProjectDeprovisioner,
	config ProjectDeletionReconcilerConfig,
	logger *slog.Logger,
) (*ProjectDeletionReconciler, error) {
	if pool == nil || deprovisioner == nil {
		return nil, errors.New("project deletion reconciler: pool and deprovisioner are required")
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
		pool:          pool,
		deprovisioner: deprovisioner,
		config:        config,
		logger:        logger,
		now:           time.Now,
		wait:          waitCurrentIndexMetaTerminalReconciler,
		backoff:       map[int64]projectDeletionBackoff{},
	}, nil
}

// Failures is the number of delete attempts that have failed so far. Finished is
// the number of tombstoned projects the reconciler has seen out.
func (r *ProjectDeletionReconciler) Failures() int64 { return r.failures.Load() }

// Finished: see Failures.
func (r *ProjectDeletionReconciler) Finished() int64 { return r.finished.Load() }

// candidates reads the next batch of stale tombstones, oldest first, leaving out
// the projects still backing off. FOR UPDATE SKIP LOCKED passes over a row that
// a fence, a delete step or another replica's read holds; the lock lasts only for
// this read, since Deprovision locks the same row itself.
func (r *ProjectDeletionReconciler) candidates(ctx context.Context) ([]int64, error) {
	transaction, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("project deletion reconciler: begin: %w", err)
	}
	defer func() { _ = transaction.Rollback(context.WithoutCancel(ctx)) }()

	skipped := r.backedOff()
	rows, err := transaction.Query(ctx, `
SELECT id
FROM centry.project
WHERE deleting_at IS NOT NULL
  AND deleting_at < $1
  AND NOT (id = ANY($2::bigint[]))
ORDER BY deleting_at, id
LIMIT $3
FOR UPDATE SKIP LOCKED`,
		r.now().Add(-r.config.GracePeriod), skipped, r.config.BatchSize)
	if err != nil {
		return nil, fmt.Errorf("project deletion reconciler: list tombstones: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("project deletion reconciler: read tombstone: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("project deletion reconciler: list tombstones: %w", err)
	}
	return ids, nil
}

func (r *ProjectDeletionReconciler) backedOff() []int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	ids := make([]int64, 0, len(r.backoff))
	for id, entry := range r.backoff {
		if entry.retryAt.After(now) {
			ids = append(ids, id)
		}
	}
	return ids
}

func (r *ProjectDeletionReconciler) recordFailure(id int64) time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry := r.backoff[id]
	entry.failures++
	delay := r.config.Interval
	for step := 1; step < entry.failures && delay < maxProjectDeletionBackoff; step++ {
		delay *= 2
	}
	delay = min(delay, maxProjectDeletionBackoff)
	entry.retryAt = r.now().Add(delay)
	r.backoff[id] = entry
	return delay
}

func (r *ProjectDeletionReconciler) clear(id int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.backoff, id)
}

// RunOnce finishes up to one batch of stale tombstones and returns how many
// delete attempts it made. A failed delete is logged, counted and backed off; it
// does not stop the batch. The error is non-nil only when the candidates could
// not be read.
func (r *ProjectDeletionReconciler) RunOnce(ctx context.Context) (int, error) {
	ids, err := r.candidates(ctx)
	if err != nil {
		return 0, err
	}
	attempted := 0
	for _, id := range ids {
		if ctx.Err() != nil {
			return attempted, ctx.Err()
		}
		attempted++
		// No SkipActiveWorkCheck: the tombstone is set, so the fence is skipped by
		// Deprovision anyway, and the reconciler never needs to override a count.
		_, err := r.deprovisioner.Deprovision(ctx, id)
		switch {
		case err == nil, errors.Is(err, projectprovisioning.ErrProjectNotFound):
			r.finished.Add(1)
			r.clear(id)
			r.logger.InfoContext(ctx, "finished a stalled project delete", "project_id", id)
		case errors.Is(err, projectprovisioning.ErrProjectDeletionInProgress):
			// Someone else is finishing it right now. Not a failure.
			r.logger.DebugContext(ctx, "stalled project delete is being finished elsewhere", "project_id", id)
		default:
			if ctx.Err() != nil {
				return attempted, ctx.Err()
			}
			r.failures.Add(1)
			delay := r.recordFailure(id)
			r.logger.ErrorContext(ctx, "could not finish a stalled project delete",
				"project_id", id, "retry_in", delay, "failures_total", r.failures.Load(), "err", err)
		}
	}
	return attempted, nil
}

// Run loops until ctx is cancelled. The interval is jittered by up to +-20% so
// replicas do not run their passes in step.
func (r *ProjectDeletionReconciler) Run(ctx context.Context) error {
	if r == nil || r.pool == nil || ctx == nil {
		return errors.New("project deletion reconciler is incomplete")
	}
	for {
		if _, err := r.RunOnce(ctx); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			r.failures.Add(1)
			r.logger.ErrorContext(ctx, "project deletion reconciliation failed", "err", err)
		}
		jitter := 0.8 + 0.4*rand.Float64()
		if err := r.wait(ctx, time.Duration(float64(r.config.Interval)*jitter)); err != nil {
			return err
		}
	}
}

var _ publisherRunner = (*ProjectDeletionReconciler)(nil)
