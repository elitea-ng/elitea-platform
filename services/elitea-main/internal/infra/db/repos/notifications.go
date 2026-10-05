package repos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/changesync"
	notificationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/notifications"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

const maxCurrentNotificationPage = int32(1000)

var ErrInvalidCurrentNotificationOperation = errors.New("invalid current notification operation")

type currentNotificationQueries interface {
	CountCurrentNotifications(context.Context, sqlcgen.CountCurrentNotificationsParams) (int64, error)
	ListCurrentNotifications(context.Context, sqlcgen.ListCurrentNotificationsParams) ([]sqlcgen.ListCurrentNotificationsRow, error)
	GetCurrentNotification(context.Context, sqlcgen.GetCurrentNotificationParams) (sqlcgen.GetCurrentNotificationRow, error)
	MarkCurrentNotificationSeen(context.Context, sqlcgen.MarkCurrentNotificationSeenParams) (sqlcgen.MarkCurrentNotificationSeenRow, error)
	DeleteCurrentNotification(context.Context, sqlcgen.DeleteCurrentNotificationParams) (int64, error)
	BulkSetCurrentNotificationsSeen(context.Context, sqlcgen.BulkSetCurrentNotificationsSeenParams) (int64, error)
	BulkDeleteCurrentNotifications(context.Context, sqlcgen.BulkDeleteCurrentNotificationsParams) (int64, error)
	CurrentNotificationSyncClock(context.Context) (pgtype.Timestamptz, error)
	ListCurrentNotificationChanges(context.Context, sqlcgen.ListCurrentNotificationChangesParams) ([]sqlcgen.ListCurrentNotificationChangesRow, error)
	ListCurrentNotificationTombstones(context.Context, sqlcgen.ListCurrentNotificationTombstonesParams) ([]sqlcgen.ListCurrentNotificationTombstonesRow, error)
}

type CurrentNotificationRepository struct {
	queries currentNotificationQueries
}

func NewCurrentNotificationRepository(pool *pgxpool.Pool) (*CurrentNotificationRepository, error) {
	if pool == nil {
		return nil, ErrInvalidCurrentNotificationOperation
	}
	return newCurrentNotificationRepository(sqlcgen.New(pool))
}

func newCurrentNotificationRepository(queries currentNotificationQueries) (*CurrentNotificationRepository, error) {
	if queries == nil {
		return nil, ErrInvalidCurrentNotificationOperation
	}
	return &CurrentNotificationRepository{queries: queries}, nil
}

func (repository *CurrentNotificationRepository) Count(
	ctx context.Context,
	userID int64,
	filter notificationapp.ListFilter,
) (int64, error) {
	user, params, err := repository.listParams(ctx, userID, filter)
	if err != nil {
		return 0, err
	}
	count, err := repository.queries.CountCurrentNotifications(ctx, sqlcgen.CountCurrentNotificationsParams{
		UserID:      user,
		OnlyNew:     params.OnlyNew,
		EventType:   params.EventType,
		SearchWords: params.SearchWords,
	})
	if err != nil {
		return 0, fmt.Errorf("count current notifications: %w", err)
	}
	return count, nil
}

func (repository *CurrentNotificationRepository) List(
	ctx context.Context,
	userID int64,
	filter notificationapp.ListFilter,
) ([]notificationapp.Notification, error) {
	_, params, err := repository.listParams(ctx, userID, filter)
	if err != nil {
		return nil, err
	}
	rows, err := repository.queries.ListCurrentNotifications(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("list current notifications: %w", err)
	}
	notifications := make([]notificationapp.Notification, 0, len(rows))
	for _, row := range rows {
		notification, mapErr := currentNotificationFromRow(
			row.ID, row.Uuid, row.IsSeen, row.ProjectID, row.UserID, row.Meta,
			row.EventType, row.CreatedAt, row.UpdatedAt,
		)
		if mapErr != nil {
			return nil, mapErr
		}
		notifications = append(notifications, notification)
	}
	return notifications, nil
}

func (repository *CurrentNotificationRepository) Get(
	ctx context.Context,
	userID,
	notificationID int64,
) (notificationapp.Notification, error) {
	user, id, err := currentNotificationOperationIDs(repository, ctx, userID, notificationID)
	if err != nil {
		return notificationapp.Notification{}, err
	}
	row, err := repository.queries.GetCurrentNotification(ctx, sqlcgen.GetCurrentNotificationParams{
		NotificationID: id,
		UserID:         user,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return notificationapp.Notification{}, notificationapp.ErrNotificationNotFound
	}
	if err != nil {
		return notificationapp.Notification{}, fmt.Errorf("get current notification: %w", err)
	}
	return currentNotificationFromRow(
		row.ID, row.Uuid, row.IsSeen, row.ProjectID, row.UserID, row.Meta,
		row.EventType, row.CreatedAt, row.UpdatedAt,
	)
}

func (repository *CurrentNotificationRepository) MarkSeen(
	ctx context.Context,
	userID,
	notificationID int64,
) (notificationapp.Notification, error) {
	user, id, err := currentNotificationOperationIDs(repository, ctx, userID, notificationID)
	if err != nil {
		return notificationapp.Notification{}, err
	}
	row, err := repository.queries.MarkCurrentNotificationSeen(
		ctx,
		sqlcgen.MarkCurrentNotificationSeenParams{NotificationID: id, UserID: user},
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return notificationapp.Notification{}, notificationapp.ErrNotificationNotFound
	}
	if err != nil {
		return notificationapp.Notification{}, fmt.Errorf("mark current notification seen: %w", err)
	}
	return currentNotificationFromRow(
		row.ID, row.Uuid, row.IsSeen, row.ProjectID, row.UserID, row.Meta,
		row.EventType, row.CreatedAt, row.UpdatedAt,
	)
}

func (repository *CurrentNotificationRepository) Delete(
	ctx context.Context,
	userID,
	notificationID int64,
) error {
	user, id, err := currentNotificationOperationIDs(repository, ctx, userID, notificationID)
	if err != nil {
		return err
	}
	rows, err := repository.queries.DeleteCurrentNotification(
		ctx,
		sqlcgen.DeleteCurrentNotificationParams{NotificationID: id, UserID: user},
	)
	if err != nil {
		return fmt.Errorf("delete current notification: %w", err)
	}
	if rows == 0 {
		return notificationapp.ErrNotificationNotFound
	}
	if rows != 1 {
		return ErrInvalidCurrentNotificationOperation
	}
	return nil
}

func (repository *CurrentNotificationRepository) BulkSetSeen(
	ctx context.Context,
	userID int64,
	ids []int64,
	all,
	isSeen bool,
) (int64, error) {
	user, ok := currentNotificationUserID(userID)
	converted, idsOK := currentNotificationIDs(ids, all)
	if repository == nil || repository.queries == nil || ctx == nil || !ok || !idsOK {
		return 0, ErrInvalidCurrentNotificationOperation
	}
	rows, err := repository.queries.BulkSetCurrentNotificationsSeen(
		ctx,
		sqlcgen.BulkSetCurrentNotificationsSeenParams{
			IsSeen:           isSeen,
			UserID:           user,
			AllNotifications: all,
			NotificationIds:  converted,
		},
	)
	if err != nil {
		return 0, fmt.Errorf("bulk update current notifications: %w", err)
	}
	return rows, nil
}

func (repository *CurrentNotificationRepository) BulkDelete(
	ctx context.Context,
	userID int64,
	ids []int64,
) (int64, error) {
	user, ok := currentNotificationUserID(userID)
	converted, idsOK := currentNotificationIDs(ids, false)
	if repository == nil || repository.queries == nil || ctx == nil || !ok || !idsOK {
		return 0, ErrInvalidCurrentNotificationOperation
	}
	rows, err := repository.queries.BulkDeleteCurrentNotifications(
		ctx,
		sqlcgen.BulkDeleteCurrentNotificationsParams{UserID: user, NotificationIds: converted},
	)
	if err != nil {
		return 0, fmt.Errorf("bulk delete current notifications: %w", err)
	}
	return rows, nil
}

func (repository *CurrentNotificationRepository) listParams(
	ctx context.Context,
	userID int64,
	filter notificationapp.ListFilter,
) (int32, sqlcgen.ListCurrentNotificationsParams, error) {
	user, ok := currentNotificationUserID(userID)
	if repository == nil || repository.queries == nil || ctx == nil || !ok ||
		filter.Limit <= 0 || filter.Limit > maxCurrentNotificationPage || filter.Offset < 0 ||
		!currentNotificationSortValid(filter.SortBy, filter.SortOrder) || len(filter.EventType) > 255 ||
		len(filter.SearchWords) > 32 {
		return 0, sqlcgen.ListCurrentNotificationsParams{}, ErrInvalidCurrentNotificationOperation
	}
	words := make([]string, 0, len(filter.SearchWords))
	for _, word := range filter.SearchWords {
		if word == "" || len(word) > 256 {
			return 0, sqlcgen.ListCurrentNotificationsParams{}, ErrInvalidCurrentNotificationOperation
		}
		words = append(words, currentNotificationLIKEWord(word))
	}
	return user, sqlcgen.ListCurrentNotificationsParams{
		UserID:      user,
		OnlyNew:     filter.OnlyNew,
		EventType:   filter.EventType,
		SearchWords: words,
		SortBy:      filter.SortBy,
		SortOrder:   filter.SortOrder,
		PageOffset:  filter.Offset,
		PageLimit:   filter.Limit,
	}, nil
}

func currentNotificationOperationIDs(
	repository *CurrentNotificationRepository,
	ctx context.Context,
	userID,
	notificationID int64,
) (int32, int32, error) {
	user, userOK := currentNotificationUserID(userID)
	id, idOK := currentNotificationCursor(notificationID)
	if repository == nil || repository.queries == nil || ctx == nil || !userOK || !idOK || id == 0 {
		return 0, 0, ErrInvalidCurrentNotificationOperation
	}
	return user, id, nil
}

func currentNotificationIDs(ids []int64, all bool) ([]int32, bool) {
	if all {
		return []int32{}, len(ids) == 0
	}
	converted := make([]int32, 0, len(ids))
	for _, value := range ids {
		id, ok := currentNotificationCursor(value)
		if !ok || id == 0 {
			return nil, false
		}
		converted = append(converted, id)
	}
	return converted, true
}

func currentNotificationSortValid(field, order string) bool {
	switch field {
	case "id", "uuid", "is_seen", "project_id", "user_id", "meta", "event_type", "created_at", "updated_at":
	default:
		return false
	}
	return order == "asc" || order == "desc"
}

func currentNotificationLIKEWord(value string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return replacer.Replace(value)
}

func currentNotificationFromRow(
	id int32,
	uuid string,
	isSeen bool,
	projectID,
	userID int32,
	meta []byte,
	eventType string,
	createdAt,
	updatedAt pgtype.Timestamp,
) (notificationapp.Notification, error) {
	if id <= 0 || uuid == "" || projectID <= 0 || userID <= 0 || eventType == "" ||
		!createdAt.Valid || len(meta) == 0 || !json.Valid(meta) {
		return notificationapp.Notification{}, ErrInvalidCurrentNotificationOperation
	}
	var updated *time.Time
	if updatedAt.Valid {
		value := updatedAt.Time.UTC()
		updated = &value
	}
	return notificationapp.Notification{
		ID:        id,
		UUID:      uuid,
		IsSeen:    isSeen,
		ProjectID: projectID,
		UserID:    userID,
		Meta:      append([]byte(nil), meta...),
		EventType: eventType,
		CreatedAt: createdAt.Time.UTC(),
		UpdatedAt: updated,
	}, nil
}

// ListChanges is the caller's notification delta (`changes_since`, ADR-0025
// WP6). The cursor is scoped to the user: notifications are user-scoped across
// projects, so the project in the route does not enter it. Stamps and
// tombstones are written by shared migration 0144's triggers.
func (repository *CurrentNotificationRepository) ListChanges(
	ctx context.Context,
	userID int64,
	changesSince string,
	limit int,
) (notificationapp.Changes, error) {
	user, ok := currentNotificationUserID(userID)
	if repository == nil || repository.queries == nil || ctx == nil || !ok {
		return notificationapp.Changes{}, ErrInvalidCurrentNotificationOperation
	}
	if limit < 1 || limit > changesync.MaxLimit {
		limit = changesync.DefaultLimit
	}
	cursor, err := changesync.Decode(changesSince, changesync.StreamNotifications, strconv.FormatInt(userID, 10))
	if err != nil {
		return notificationapp.Changes{}, err
	}
	clock, err := repository.queries.CurrentNotificationSyncClock(ctx)
	if err != nil || !clock.Valid {
		return notificationapp.Changes{}, fmt.Errorf("current notification sync clock: %w", errors.Join(err, ErrInvalidCurrentNotificationOperation))
	}
	if err := cursor.CheckExpiry(clock.Time); err != nil {
		return notificationapp.Changes{}, err
	}
	bound := changesync.SettleBound(clock.Time)

	rowsStart := cursor.RowsStart()
	rows, err := repository.queries.ListCurrentNotificationChanges(ctx, sqlcgen.ListCurrentNotificationChangesParams{
		UserID:    user,
		AfterAt:   pgtype.Timestamptz{Time: rowsStart.At, Valid: true},
		AfterID:   rowsStart.ID,
		PageLimit: int32(limit + 1),
	})
	if err != nil {
		return notificationapp.Changes{}, fmt.Errorf("list current notification changes: %w", err)
	}
	page := notificationapp.Changes{Rows: []notificationapp.Notification{}, Tombstones: []changesync.Tombstone{}}
	positions := []changesync.Position{}
	for _, row := range rows {
		notification, mapErr := currentNotificationFromRow(
			row.ID, row.Uuid, row.IsSeen, row.ProjectID, row.UserID, row.Meta,
			row.EventType, row.CreatedAt, row.UpdatedAt,
		)
		if mapErr != nil {
			return notificationapp.Changes{}, mapErr
		}
		page.Rows = append(page.Rows, notification)
		positions = append(positions, changesync.Position{At: row.SyncAt.Time, ID: int64(row.ID)})
	}
	rowsTruncated := len(page.Rows) > limit
	if rowsTruncated {
		page.Rows, positions = page.Rows[:limit], positions[:limit]
	}

	tombsStart := cursor.TombstonesStart(bound)
	tombs, err := repository.queries.ListCurrentNotificationTombstones(ctx, sqlcgen.ListCurrentNotificationTombstonesParams{
		UserID:    user,
		AfterAt:   pgtype.Timestamptz{Time: tombsStart.At, Valid: true},
		AfterID:   tombsStart.ID,
		PageLimit: int32(limit + 1),
	})
	if err != nil {
		return notificationapp.Changes{}, fmt.Errorf("list current notification tombstones: %w", err)
	}
	tombPositions := []changesync.Position{}
	for _, tomb := range tombs {
		var uuid *string
		if tomb.NotificationUuid != "" {
			value := tomb.NotificationUuid
			uuid = &value
		}
		page.Tombstones = append(page.Tombstones, changesync.Tombstone{
			ID:        int64(tomb.NotificationID),
			UUID:      uuid,
			Reason:    changesync.ReasonDeleted,
			DeletedAt: changesync.FormatTime(tomb.DeletedAt.Time),
		})
		tombPositions = append(tombPositions, changesync.Position{At: tomb.DeletedAt.Time, ID: tomb.ID})
	}
	tombsTruncated := len(page.Tombstones) > limit
	if tombsTruncated {
		page.Tombstones, tombPositions = page.Tombstones[:limit], tombPositions[:limit]
	}

	nextRows, moreRows := changesync.Advance(rowsStart, lastSyncPosition(positions), rowsTruncated, bound)
	nextTombs, moreTombs := changesync.Advance(tombsStart, lastSyncPosition(tombPositions), tombsTruncated, bound)
	page.NextCursor = changesync.Encode(changesync.Cursor{
		Stream: cursor.Stream, Scope: cursor.Scope, Rows: nextRows, Tombs: nextTombs,
	})
	page.HasMore = moreRows || moreTombs
	return page, nil
}

var (
	_ notificationapp.Store        = (*CurrentNotificationRepository)(nil)
	_ notificationapp.ChangesStore = (*CurrentNotificationRepository)(nil)
)
