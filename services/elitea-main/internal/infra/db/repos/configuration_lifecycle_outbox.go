package repos

import (
	"context"
	"errors"
	"time"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrInvalidCurrentConfigurationLifecycleOutbox = errors.New("invalid current configuration lifecycle outbox request")
	ErrCurrentConfigurationLifecycleLeaseLost     = errors.New("current configuration lifecycle lease was lost")
	ErrCurrentConfigurationLifecycleUnavailable   = errors.New("current configuration lifecycle outbox is unavailable")
)

type currentConfigurationLifecycleQueries interface {
	ClaimConfigurationLifecycleEvents(context.Context, sqlcgen.ClaimConfigurationLifecycleEventsParams) ([]sqlcgen.ClaimConfigurationLifecycleEventsRow, error)
	MarkConfigurationLifecycleDelivered(context.Context, sqlcgen.MarkConfigurationLifecycleDeliveredParams) (int64, error)
	MarkConfigurationLifecycleRetry(context.Context, sqlcgen.MarkConfigurationLifecycleRetryParams) (int64, error)
	MarkConfigurationLifecycleDead(context.Context, sqlcgen.MarkConfigurationLifecycleDeadParams) (int64, error)
}

// CurrentConfigurationLifecycleOutboxRepository owns bounded claim and fenced
// settlement statements. It never holds a transaction while reconciliation
// performs external work.
type CurrentConfigurationLifecycleOutboxRepository struct {
	queries currentConfigurationLifecycleQueries
}

func NewCurrentConfigurationLifecycleOutboxRepository(
	pool *pgxpool.Pool,
) (*CurrentConfigurationLifecycleOutboxRepository, error) {
	if pool == nil {
		return nil, errors.New("current configuration lifecycle database is required")
	}
	return newCurrentConfigurationLifecycleOutboxRepository(sqlcgen.New(pool))
}

func newCurrentConfigurationLifecycleOutboxRepository(
	queries currentConfigurationLifecycleQueries,
) (*CurrentConfigurationLifecycleOutboxRepository, error) {
	if queries == nil {
		return nil, errors.New("current configuration lifecycle database is required")
	}
	return &CurrentConfigurationLifecycleOutboxRepository{queries: queries}, nil
}

func (r *CurrentConfigurationLifecycleOutboxRepository) ClaimCurrentConfigurationLifecycle(
	ctx context.Context,
	leaseToken string,
	limit int,
	leaseTTL time.Duration,
) ([]configurationapp.CurrentConfigurationLifecycleEvent, error) {
	leaseMillis, ok := currentConfigurationLifecycleDurationMillis(
		leaseTTL,
		configurationapp.MaxCurrentConfigurationLifecycleLeaseTTL,
	)
	if r == nil || r.queries == nil || ctx == nil || !validCurrentConfigurationLifecycleLeaseTokenForStore(leaseToken) ||
		limit <= 0 || limit > configurationapp.MaxCurrentConfigurationLifecycleBatchSize || !ok {
		return nil, ErrInvalidCurrentConfigurationLifecycleOutbox
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	rows, err := r.queries.ClaimConfigurationLifecycleEvents(
		ctx,
		sqlcgen.ClaimConfigurationLifecycleEventsParams{
			ClaimLimit:     int32(limit),
			LeaseToken:     leaseToken,
			LeaseTtlMillis: leaseMillis,
		},
	)
	if err != nil {
		return nil, currentConfigurationLifecycleStorageError(ctx, err)
	}
	if len(rows) > limit {
		return nil, ErrCurrentConfigurationLifecycleUnavailable
	}

	events := make([]configurationapp.CurrentConfigurationLifecycleEvent, len(rows))
	seen := make(map[currentConfigurationLifecycleIdentity]struct{}, len(rows))
	for index := range rows {
		event, identity, valid := mapCurrentConfigurationLifecycleEvent(rows[index], leaseToken)
		if !valid {
			return nil, ErrCurrentConfigurationLifecycleUnavailable
		}
		if _, duplicate := seen[identity]; duplicate {
			return nil, ErrCurrentConfigurationLifecycleUnavailable
		}
		seen[identity] = struct{}{}
		events[index] = event
	}
	return events, nil
}

func (r *CurrentConfigurationLifecycleOutboxRepository) MarkCurrentConfigurationLifecycleDelivered(
	ctx context.Context,
	eventID string,
	leaseToken string,
) error {
	if err := validateCurrentConfigurationLifecycleTransition(r, ctx, eventID, leaseToken); err != nil {
		return err
	}
	rows, err := r.queries.MarkConfigurationLifecycleDelivered(
		ctx,
		sqlcgen.MarkConfigurationLifecycleDeliveredParams{
			EventID: eventID, LeaseToken: leaseToken,
		},
	)
	return currentConfigurationLifecycleTransitionResult(ctx, rows, err)
}

func (r *CurrentConfigurationLifecycleOutboxRepository) MarkCurrentConfigurationLifecycleRetry(
	ctx context.Context,
	eventID string,
	leaseToken string,
	errorCode string,
	delay time.Duration,
) error {
	if err := validateCurrentConfigurationLifecycleTransition(r, ctx, eventID, leaseToken); err != nil {
		return err
	}
	if !validCurrentConfigurationLifecycleErrorCodeForStore(errorCode) {
		return ErrInvalidCurrentConfigurationLifecycleOutbox
	}
	delayMillis, ok := currentConfigurationLifecycleDurationMillis(
		delay,
		configurationapp.MaxCurrentConfigurationLifecycleRetryDelay,
	)
	if !ok {
		return ErrInvalidCurrentConfigurationLifecycleOutbox
	}
	rows, err := r.queries.MarkConfigurationLifecycleRetry(
		ctx,
		sqlcgen.MarkConfigurationLifecycleRetryParams{
			RetryDelayMillis: delayMillis,
			ErrorCode:        errorCode,
			EventID:          eventID,
			LeaseToken:       leaseToken,
		},
	)
	return currentConfigurationLifecycleTransitionResult(ctx, rows, err)
}

func (r *CurrentConfigurationLifecycleOutboxRepository) MarkCurrentConfigurationLifecycleDead(
	ctx context.Context,
	eventID string,
	leaseToken string,
	errorCode string,
) error {
	if err := validateCurrentConfigurationLifecycleTransition(r, ctx, eventID, leaseToken); err != nil {
		return err
	}
	if !validCurrentConfigurationLifecycleErrorCodeForStore(errorCode) {
		return ErrInvalidCurrentConfigurationLifecycleOutbox
	}
	rows, err := r.queries.MarkConfigurationLifecycleDead(
		ctx,
		sqlcgen.MarkConfigurationLifecycleDeadParams{
			ErrorCode: errorCode, EventID: eventID, LeaseToken: leaseToken,
		},
	)
	return currentConfigurationLifecycleTransitionResult(ctx, rows, err)
}

type currentConfigurationLifecycleIdentity struct {
	projectID         int32
	configurationUUID string
}

func mapCurrentConfigurationLifecycleEvent(
	row sqlcgen.ClaimConfigurationLifecycleEventsRow,
	leaseToken string,
) (configurationapp.CurrentConfigurationLifecycleEvent, currentConfigurationLifecycleIdentity, bool) {
	operation := configurationapp.CurrentConfigurationLifecycleOperation(row.Operation)
	if !validCurrentPersistenceUUID(row.EventID, true) || row.ResourceProjectID <= 0 ||
		!validCurrentPersistenceUUID(row.ConfigurationUuid, false) || row.Revision <= 0 ||
		!validCurrentConfigurationLifecycleOperationForStore(operation) || row.ActorID <= 0 ||
		len(row.SanitizedSnapshot) < 2 || len(row.SanitizedSnapshot) > maxCurrentLifecycleSnapshotBytes ||
		len(row.SnapshotDigest) != 32 || row.AttemptCount <= 0 ||
		row.AttemptCount > configurationapp.MaxCurrentConfigurationLifecycleAttempts ||
		row.LeaseToken != leaseToken {
		return configurationapp.CurrentConfigurationLifecycleEvent{}, currentConfigurationLifecycleIdentity{}, false
	}

	var digest [32]byte
	copy(digest[:], row.SnapshotDigest)
	return configurationapp.CurrentConfigurationLifecycleEvent{
			EventID:           row.EventID,
			ProjectID:         row.ResourceProjectID,
			ConfigurationUUID: row.ConfigurationUuid,
			Revision:          row.Revision,
			Operation:         operation,
			ActorID:           row.ActorID,
			Snapshot:          append([]byte(nil), row.SanitizedSnapshot...),
			SnapshotDigest:    digest,
			AttemptCount:      int(row.AttemptCount),
			LeaseToken:        row.LeaseToken,
		}, currentConfigurationLifecycleIdentity{
			projectID: row.ResourceProjectID, configurationUUID: row.ConfigurationUuid,
		}, true
}

func validateCurrentConfigurationLifecycleTransition(
	r *CurrentConfigurationLifecycleOutboxRepository,
	ctx context.Context,
	eventID string,
	leaseToken string,
) error {
	if r == nil || r.queries == nil || ctx == nil || !validCurrentPersistenceUUID(eventID, true) ||
		!validCurrentConfigurationLifecycleLeaseTokenForStore(leaseToken) {
		return ErrInvalidCurrentConfigurationLifecycleOutbox
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

func currentConfigurationLifecycleTransitionResult(
	ctx context.Context,
	rows int64,
	err error,
) error {
	if err != nil {
		return currentConfigurationLifecycleStorageError(ctx, err)
	}
	if rows == 0 {
		return ErrCurrentConfigurationLifecycleLeaseLost
	}
	if rows != 1 {
		return ErrCurrentConfigurationLifecycleUnavailable
	}
	return nil
}

func currentConfigurationLifecycleStorageError(ctx context.Context, err error) error {
	if ctx != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return ErrCurrentConfigurationLifecycleUnavailable
}

func currentConfigurationLifecycleDurationMillis(value, maximum time.Duration) (int64, bool) {
	if value <= 0 || value > maximum || value%time.Millisecond != 0 {
		return 0, false
	}
	return int64(value / time.Millisecond), true
}

func validCurrentConfigurationLifecycleOperationForStore(
	operation configurationapp.CurrentConfigurationLifecycleOperation,
) bool {
	switch operation {
	case configurationapp.CurrentConfigurationCreated,
		configurationapp.CurrentConfigurationUpdated,
		configurationapp.CurrentConfigurationDeleted:
		return true
	default:
		return false
	}
}

func validCurrentConfigurationLifecycleLeaseTokenForStore(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		if (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') &&
			(character < '0' || character > '9') && character != '.' && character != '_' &&
			character != ':' && character != '-' {
			return false
		}
	}
	return true
}

func validCurrentConfigurationLifecycleErrorCodeForStore(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		if (character < 'A' || character > 'Z') && (character < '0' || character > '9') && character != '_' {
			return false
		}
	}
	return true
}

var _ configurationapp.CurrentConfigurationLifecycleStore = (*CurrentConfigurationLifecycleOutboxRepository)(nil)
