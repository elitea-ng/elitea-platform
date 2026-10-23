package repos

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	schedulingapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/scheduling"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ScheduleOccurrenceRepository is the shared PostgreSQL authority for
// capability-neutral job planning and occurrence claims.
type ScheduleOccurrenceRepository struct {
	pool    *pgxpool.Pool
	queries *sqlcgen.Queries
}

func NewScheduleOccurrenceRepository(pool *pgxpool.Pool) (*ScheduleOccurrenceRepository, error) {
	if pool == nil {
		return nil, errors.New("scheduled occurrence database pool is required")
	}
	return &ScheduleOccurrenceRepository{pool: pool, queries: sqlcgen.New(pool)}, nil
}

func (r *ScheduleOccurrenceRepository) Now(ctx context.Context) (time.Time, error) {
	now, err := r.queries.ScheduledDatabaseNow(ctx)
	if err != nil {
		return time.Time{}, fmt.Errorf("query scheduler database clock: %w", err)
	}
	if !now.Valid {
		return time.Time{}, errors.New("scheduler database clock is unavailable")
	}
	return now.Time.UTC(), nil
}

func (r *ScheduleOccurrenceRepository) ClaimPlanning(
	ctx context.Context,
	job schedulingapp.RegisteredJob,
	owner string,
	now time.Time,
	leaseDuration time.Duration,
	initialLookback time.Duration,
) (schedulingapp.PlanningClaim, bool, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite,
	})
	if err != nil {
		return schedulingapp.PlanningClaim{}, false, fmt.Errorf("begin scheduled planning claim: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	queries := sqlcgen.New(tx)

	row, err := queries.GetScheduledJobCursorForUpdate(ctx, job.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		row.ScheduleRevision = job.Revision
		row.ObservedThrough = timestamptz(now.UTC().Add(-initialLookback))
		if err := queries.InsertScheduledJobCursor(ctx, sqlcgen.InsertScheduledJobCursorParams{
			JobID:            job.ID,
			ScheduleRevision: job.Revision,
			ObservedThrough:  row.ObservedThrough,
		}); err != nil {
			return schedulingapp.PlanningClaim{}, false, fmt.Errorf("insert scheduled job cursor: %w", err)
		}
	} else if err != nil {
		return schedulingapp.PlanningClaim{}, false, fmt.Errorf("lock scheduled job cursor: %w", err)
	}

	if row.LeaseExpiresAt.Valid && row.LeaseExpiresAt.Time.After(now) {
		return schedulingapp.PlanningClaim{}, false, nil
	}
	if row.ScheduleRevision != job.Revision {
		if err := queries.SupersedeScheduledJobRevision(
			ctx,
			sqlcgen.SupersedeScheduledJobRevisionParams{
				CompletedAt:      timestamptz(now.UTC()),
				JobID:            job.ID,
				ScheduleRevision: row.ScheduleRevision,
			},
		); err != nil {
			return schedulingapp.PlanningClaim{}, false, fmt.Errorf("supersede old scheduled job revision: %w", err)
		}
		row.ObservedThrough = timestamptz(now.UTC().Add(-initialLookback))
	}

	fence, err := randomFence()
	if err != nil {
		return schedulingapp.PlanningClaim{}, false, err
	}
	leaseEpoch := row.LeaseEpoch + 1
	affected, err := queries.ClaimScheduledJobCursor(ctx, sqlcgen.ClaimScheduledJobCursorParams{
		ScheduleRevision: job.Revision,
		ObservedThrough:  row.ObservedThrough,
		LeaseOwner:       stringPointer(owner),
		LeaseEpoch:       leaseEpoch,
		ClaimFence:       fence[:],
		LeaseExpiresAt:   timestamptz(now.UTC().Add(leaseDuration)),
		UpdatedAt:        timestamptz(now.UTC()),
		JobID:            job.ID,
	})
	if err != nil {
		return schedulingapp.PlanningClaim{}, false, fmt.Errorf("claim scheduled job cursor: %w", err)
	}
	if affected != 1 {
		return schedulingapp.PlanningClaim{}, false, schedulingapp.ErrStaleFence
	}
	if err := tx.Commit(ctx); err != nil {
		return schedulingapp.PlanningClaim{}, false, fmt.Errorf("commit scheduled planning claim: %w", err)
	}
	return schedulingapp.PlanningClaim{
		JobID:            job.ID,
		ScheduleRevision: job.Revision,
		ObservedThrough:  row.ObservedThrough.Time.UTC(),
		LeaseEpoch:       leaseEpoch,
		ClaimFence:       fence,
	}, true, nil
}

func (r *ScheduleOccurrenceRepository) MaterializeAndAdvance(
	ctx context.Context,
	claim schedulingapp.PlanningClaim,
	seeds []schedulingapp.OccurrenceSeed,
	through time.Time,
) error {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite,
	})
	if err != nil {
		return fmt.Errorf("begin scheduled occurrence materialization: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	queries := sqlcgen.New(tx)
	for _, seed := range seeds {
		if seed.JobID != claim.JobID ||
			seed.ScheduleRevision != claim.ScheduleRevision ||
			!seed.DueAt.After(claim.ObservedThrough) ||
			seed.DueAt.After(through) {
			return errors.New("scheduled occurrence seed is outside its planning claim")
		}
		if err := queries.InsertScheduledOccurrence(ctx, sqlcgen.InsertScheduledOccurrenceParams{
			InvocationID:     seed.InvocationID,
			JobID:            seed.JobID,
			ScheduleRevision: seed.ScheduleRevision,
			DueAt:            timestamptz(seed.DueAt.UTC()),
			OutcomeMode:      string(seed.Mode),
		}); err != nil {
			return fmt.Errorf("insert scheduled occurrence: %w", err)
		}
	}
	affected, err := queries.AdvanceScheduledJobCursor(ctx, sqlcgen.AdvanceScheduledJobCursorParams{
		ObservedThrough:  timestamptz(through.UTC()),
		JobID:            claim.JobID,
		ScheduleRevision: claim.ScheduleRevision,
		LeaseEpoch:       claim.LeaseEpoch,
		ClaimFence:       claim.ClaimFence[:],
	})
	if err != nil {
		return fmt.Errorf("advance scheduled job cursor: %w", err)
	}
	if affected != 1 {
		return schedulingapp.ErrStaleFence
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit scheduled occurrence materialization: %w", err)
	}
	return nil
}

// DisableAndAdvance supersedes a switched-off job's pending occurrences, so
// switching it back on does not run them late, and advances its cursor. An
// occurrence a replica holds a live lease on is left to settle on its own.
func (r *ScheduleOccurrenceRepository) DisableAndAdvance(
	ctx context.Context,
	claim schedulingapp.PlanningClaim,
	through time.Time,
) error {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite,
	})
	if err != nil {
		return fmt.Errorf("begin disabled scheduled job advance: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	queries := sqlcgen.New(tx)
	if err := queries.SupersedePendingScheduledJobOccurrences(
		ctx,
		sqlcgen.SupersedePendingScheduledJobOccurrencesParams{
			CompletedAt: timestamptz(through.UTC()),
			JobID:       claim.JobID,
		},
	); err != nil {
		return fmt.Errorf("supersede disabled scheduled job occurrences: %w", err)
	}
	affected, err := queries.AdvanceScheduledJobCursor(ctx, sqlcgen.AdvanceScheduledJobCursorParams{
		ObservedThrough:  timestamptz(through.UTC()),
		JobID:            claim.JobID,
		ScheduleRevision: claim.ScheduleRevision,
		LeaseEpoch:       claim.LeaseEpoch,
		ClaimFence:       claim.ClaimFence[:],
	})
	if err != nil {
		return fmt.Errorf("advance disabled scheduled job cursor: %w", err)
	}
	if affected != 1 {
		return schedulingapp.ErrStaleFence
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit disabled scheduled job advance: %w", err)
	}
	return nil
}

func (r *ScheduleOccurrenceRepository) ClaimPage(
	ctx context.Context,
	registered []schedulingapp.RegisteredJob,
	owner string,
	now time.Time,
	leaseDuration time.Duration,
	limit int,
) ([]schedulingapp.ClaimedOccurrence, error) {
	if len(registered) == 0 || len(registered) > schedulingapp.MaxRegisteredJobs {
		return nil, fmt.Errorf("registered scheduled jobs must contain 1..%d entries", schedulingapp.MaxRegisteredJobs)
	}
	if limit < 1 || limit > schedulingapp.MaxPageSize {
		return nil, fmt.Errorf("scheduled occurrence page limit must be in 1..%d", schedulingapp.MaxPageSize)
	}
	if leaseDuration <= 0 {
		return nil, errors.New("scheduled occurrence lease duration must be positive")
	}
	jobIDs := make([]string, len(registered))
	revisions := make([]string, len(registered))
	for index, job := range registered {
		jobIDs[index] = job.ID
		revisions[index] = job.Revision
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite,
	})
	if err != nil {
		return nil, fmt.Errorf("begin scheduled occurrence claim page: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	queries := sqlcgen.New(tx)
	rows, err := queries.ListClaimableScheduledOccurrences(
		ctx,
		sqlcgen.ListClaimableScheduledOccurrencesParams{
			JobIds:            jobIDs,
			ScheduleRevisions: revisions,
			ObservedAt:        timestamptz(now.UTC()),
			PageLimit:         int32(limit),
		},
	)
	if err != nil {
		return nil, fmt.Errorf("select claimable scheduled occurrences: %w", err)
	}
	claimed := make([]schedulingapp.ClaimedOccurrence, 0, len(rows))
	for _, row := range rows {
		if !row.DueAt.Valid {
			return nil, errors.New("scheduled occurrence due time is unavailable")
		}
		fence, err := randomFence()
		if err != nil {
			return nil, err
		}
		leaseEpoch := row.LeaseEpoch + 1
		affected, err := queries.ClaimScheduledOccurrence(
			ctx,
			sqlcgen.ClaimScheduledOccurrenceParams{
				LeaseOwner:     stringPointer(owner),
				LeaseEpoch:     leaseEpoch,
				ClaimFence:     fence[:],
				LeaseExpiresAt: timestamptz(now.UTC().Add(leaseDuration)),
				UpdatedAt:      timestamptz(now.UTC()),
				InvocationID:   row.InvocationID,
			},
		)
		if err != nil {
			return nil, fmt.Errorf("claim scheduled occurrence: %w", err)
		}
		if affected != 1 {
			return nil, schedulingapp.ErrStaleFence
		}
		claimed = append(claimed, schedulingapp.ClaimedOccurrence{
			Occurrence: schedulingapp.Occurrence{
				InvocationID:     row.InvocationID,
				JobID:            row.JobID,
				ScheduleRevision: row.ScheduleRevision,
				DueAt:            row.DueAt.Time.UTC(),
				LeaseEpoch:       leaseEpoch,
				ClaimFence:       hex.EncodeToString(fence[:]),
			},
			Mode:       schedulingapp.Mode(row.OutcomeMode),
			ClaimBytes: fence,
		})
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit scheduled occurrence claim page: %w", err)
	}
	return claimed, nil
}

func (r *ScheduleOccurrenceRepository) Complete(
	ctx context.Context,
	claim schedulingapp.ClaimedOccurrence,
	outcome schedulingapp.Outcome,
) error {
	outcomeValue := string(outcome)
	affected, err := r.queries.CompleteScheduledOccurrence(
		ctx,
		sqlcgen.CompleteScheduledOccurrenceParams{
			Outcome:      &outcomeValue,
			InvocationID: claim.InvocationID,
			LeaseEpoch:   claim.LeaseEpoch,
			ClaimFence:   claim.ClaimBytes[:],
			OutcomeMode:  string(claim.Mode),
		},
	)
	if err != nil {
		return fmt.Errorf("complete scheduled occurrence: %w", err)
	}
	if affected != 1 {
		return schedulingapp.ErrStaleFence
	}
	return nil
}

func (r *ScheduleOccurrenceRepository) ReleaseForRetry(
	ctx context.Context,
	claim schedulingapp.ClaimedOccurrence,
	errorCode string,
	retryDelay time.Duration,
) error {
	if len(errorCode) == 0 || len(errorCode) > 64 {
		return errors.New("scheduled occurrence error code must contain 1..64 bytes")
	}
	if retryDelay < time.Millisecond || retryDelay > schedulingapp.MaxRetryDelay {
		return fmt.Errorf("scheduled occurrence retry delay must be between 1ms and %s", schedulingapp.MaxRetryDelay)
	}
	affected, err := r.queries.ReleaseScheduledOccurrenceForRetry(
		ctx,
		sqlcgen.ReleaseScheduledOccurrenceForRetryParams{
			RetryDelayMilliseconds: retryDelay.Milliseconds(),
			LastErrorCode:          &errorCode,
			InvocationID:           claim.InvocationID,
			LeaseEpoch:             claim.LeaseEpoch,
			ClaimFence:             claim.ClaimBytes[:],
		},
	)
	if err != nil {
		return fmt.Errorf("release scheduled occurrence: %w", err)
	}
	if affected != 1 {
		return schedulingapp.ErrStaleFence
	}
	return nil
}

func randomFence() ([32]byte, error) {
	var fence [32]byte
	if _, err := rand.Read(fence[:]); err != nil {
		return [32]byte{}, fmt.Errorf("generate scheduled occurrence fence: %w", err)
	}
	return fence, nil
}

func timestamptz(value time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: value, Valid: true}
}

func stringPointer(value string) *string {
	return &value
}

var _ schedulingapp.Store = (*ScheduleOccurrenceRepository)(nil)
