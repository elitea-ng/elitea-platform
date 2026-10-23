package repos

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	notificationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/notifications"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/jackc/pgx/v5/pgxpool"
)

const maxCurrentNotificationEventBatch = 100

var ErrInvalidCurrentNotificationEventRead = errors.New("invalid current notification event read")

type currentNotificationEventQueries interface {
	CurrentNotificationHighWater(context.Context, int32) (int64, error)
	ListCurrentNotificationEventsAfter(
		context.Context,
		sqlcgen.ListCurrentNotificationEventsAfterParams,
	) ([]sqlcgen.ListCurrentNotificationEventsAfterRow, error)
}

type CurrentNotificationEventRepository struct {
	queries currentNotificationEventQueries
}

func NewCurrentNotificationEventRepository(
	pool *pgxpool.Pool,
) (*CurrentNotificationEventRepository, error) {
	if pool == nil {
		return nil, ErrInvalidCurrentNotificationEventRead
	}
	return newCurrentNotificationEventRepository(sqlcgen.New(pool))
}

func newCurrentNotificationEventRepository(
	queries currentNotificationEventQueries,
) (*CurrentNotificationEventRepository, error) {
	if queries == nil {
		return nil, ErrInvalidCurrentNotificationEventRead
	}
	return &CurrentNotificationEventRepository{queries: queries}, nil
}

func (repository *CurrentNotificationEventRepository) HighWater(
	ctx context.Context,
	userID int64,
) (int64, error) {
	user, ok := currentNotificationUserID(userID)
	if repository == nil || repository.queries == nil || ctx == nil || !ok {
		return 0, ErrInvalidCurrentNotificationEventRead
	}
	cursor, err := repository.queries.CurrentNotificationHighWater(ctx, user)
	if err != nil {
		return 0, fmt.Errorf("read current notification high water: %w", err)
	}
	if cursor < 0 || cursor > math.MaxInt32 {
		return 0, ErrInvalidCurrentNotificationEventRead
	}
	return cursor, nil
}

func (repository *CurrentNotificationEventRepository) ListAfter(
	ctx context.Context,
	userID,
	afterCursor int64,
	limit int32,
) ([]notificationapp.Event, error) {
	user, userOK := currentNotificationUserID(userID)
	after, afterOK := currentNotificationCursor(afterCursor)
	if repository == nil || repository.queries == nil || ctx == nil ||
		!userOK || !afterOK || limit <= 0 || limit > maxCurrentNotificationEventBatch {
		return nil, ErrInvalidCurrentNotificationEventRead
	}
	rows, err := repository.queries.ListCurrentNotificationEventsAfter(
		ctx,
		sqlcgen.ListCurrentNotificationEventsAfterParams{
			UserID:      user,
			AfterCursor: after,
			PageLimit:   limit,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("list current notification events: %w", err)
	}
	events := make([]notificationapp.Event, 0, len(rows))
	for _, row := range rows {
		if row.ID <= after || row.ID <= 0 || row.UserID != user || row.Uuid == "" ||
			row.EventType == "" || len(row.Meta) == 0 {
			return nil, ErrInvalidCurrentNotificationEventRead
		}
		var updatedAt *time.Time
		if row.UpdatedAt.Valid {
			value := row.UpdatedAt.Time.UTC()
			updatedAt = &value
		}
		if !row.CreatedAt.Valid {
			return nil, ErrInvalidCurrentNotificationEventRead
		}
		events = append(events, notificationapp.Event{
			Cursor:    int64(row.ID),
			UUID:      row.Uuid,
			IsSeen:    row.IsSeen,
			ProjectID: row.ProjectID,
			UserID:    row.UserID,
			Meta:      append([]byte(nil), row.Meta...),
			EventType: row.EventType,
			CreatedAt: row.CreatedAt.Time.UTC(),
			UpdatedAt: updatedAt,
		})
		after = row.ID
	}
	return events, nil
}

func currentNotificationUserID(value int64) (int32, bool) {
	return int32(value), value > 0 && value <= math.MaxInt32
}

func currentNotificationCursor(value int64) (int32, bool) {
	return int32(value), value >= 0 && value <= math.MaxInt32
}

var _ notificationapp.EventReader = (*CurrentNotificationEventRepository)(nil)
