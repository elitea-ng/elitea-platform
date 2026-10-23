package repos

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	indexscheduleapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/indexschedule"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/jackc/pgx/v5/pgxpool"
)

type currentIndexScheduleNotificationQueries interface {
	InsertCurrentIndexScheduleNotification(
		context.Context,
		sqlcgen.InsertCurrentIndexScheduleNotificationParams,
	) (int64, error)
}

type CurrentIndexScheduleNotificationRepository struct {
	queries currentIndexScheduleNotificationQueries
}

func NewCurrentIndexScheduleNotificationRepository(
	pool *pgxpool.Pool,
) (*CurrentIndexScheduleNotificationRepository, error) {
	if pool == nil {
		return nil, errors.New(
			"current index schedule notification database is required",
		)
	}
	return newCurrentIndexScheduleNotificationRepository(sqlcgen.New(pool))
}

func newCurrentIndexScheduleNotificationRepository(
	queries currentIndexScheduleNotificationQueries,
) (*CurrentIndexScheduleNotificationRepository, error) {
	if queries == nil {
		return nil, errors.New(
			"current index schedule notification database is required",
		)
	}
	return &CurrentIndexScheduleNotificationRepository{queries: queries}, nil
}

func (repository *CurrentIndexScheduleNotificationRepository) Persist(
	ctx context.Context,
	effect indexscheduleapp.FailureEffect,
) error {
	if repository == nil || repository.queries == nil || ctx == nil ||
		effect.Validate() != nil {
		return indexscheduleapp.ErrInvalidScheduleFailure
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	metadata, err := json.Marshal(map[string]any{
		"id":         nil,
		"index_name": effect.IndexMetaID,
		"state":      "failed",
		"error":      effect.SafeReason,
		"reindex":    false,
		"indexed":    0,
		"updated":    0,
		"toolkit_id": effect.ToolkitID,
		"initiator":  "schedule",
		"message": fmt.Sprintf(
			"Index [%s]() is failed.",
			effect.IndexMetaID,
		),
	})
	if err != nil {
		return indexscheduleapp.ErrInvalidScheduleFailure
	}
	rows, err := repository.queries.InsertCurrentIndexScheduleNotification(
		ctx,
		sqlcgen.InsertCurrentIndexScheduleNotificationParams{
			NotificationUuid: scheduleFailureUUID(effect.EffectID),
			ProjectID:        int32(effect.ProjectID),
			UserID:           int32(effect.UserID),
			Meta:             metadata,
		},
	)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
		return indexscheduleapp.ErrScheduleDependency
	}
	if rows < 0 || rows > 1 {
		return indexscheduleapp.ErrScheduleDependency
	}
	return nil
}

func scheduleFailureUUID(effectID string) string {
	digest := sha256.Sum256([]byte(effectID))
	value := append([]byte(nil), digest[:16]...)
	// The current notifications API accepts UUIDv4 values. Keep the identifier
	// deterministic for outbox replay while setting the version/variant bits to
	// the same shape produced by the current notifications plugin.
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(value)
	return encoded[0:8] + "-" + encoded[8:12] + "-" +
		encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32]
}
