package repos

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/jackc/pgx/v5/pgxpool"
)

type artifactRetentionNotificationQueries interface {
	GetArtifactBucketOwningProjectUserID(context.Context, int32) (int32, error)
	InsertArtifactBucketExpiryNotification(context.Context, sqlcgen.InsertArtifactBucketExpiryNotificationParams) (int64, error)
}

// ArtifactRetentionNotificationRepository resolves a project's owning user
// and writes the centry.notifications row the retention sweeper (S14) emits
// when a bucket is within one day of expires_at. See
// index_schedule_notification.go for the sibling pattern this
// mirrors — same table, different event_type.
type ArtifactRetentionNotificationRepository struct {
	queries artifactRetentionNotificationQueries
}

func NewArtifactRetentionNotificationRepository(pool *pgxpool.Pool) (*ArtifactRetentionNotificationRepository, error) {
	if pool == nil {
		return nil, errors.New("artifact retention notification database is required")
	}
	return newArtifactRetentionNotificationRepository(sqlcgen.New(pool))
}

func newArtifactRetentionNotificationRepository(queries artifactRetentionNotificationQueries) (*ArtifactRetentionNotificationRepository, error) {
	if queries == nil {
		return nil, errors.New("artifact retention notification database is required")
	}
	return &ArtifactRetentionNotificationRepository{queries: queries}, nil
}

// ProjectOwnerUserID resolves projectID's owning user via centry.project —
// see the wrapped sqlc query's own comment for why this, not a new
// bucket-owner column, is what the sweeper notifies.
func (r *ArtifactRetentionNotificationRepository) ProjectOwnerUserID(ctx context.Context, projectID int64) (int64, error) {
	ownerID, err := r.queries.GetArtifactBucketOwningProjectUserID(ctx, int32(projectID))
	if err != nil {
		return 0, fmt.Errorf("get project owner: %w", err)
	}
	return int64(ownerID), nil
}

// NotifyBucketExpiring inserts one centry.notifications row
// (event_type=artifact_bucket_expiring) for bucketID. The UUID is
// deterministic from (bucketID, expiresAt) — stable across a retried
// sweeper tick within the same notification cycle (ON CONFLICT (uuid) DO
// NOTHING keeps a retry idempotent), but distinct across cycles: when
// UpdateArtifactBucketRetention resets notified_at and the bucket becomes
// eligible again under a new expires_at, this produces a fresh UUID rather
// than silently colliding with (and dropping) the earlier notification.
// Callers still must call MarkBucketNotified so a later tick does not
// immediately reselect the same bucket via
// ListArtifactBucketsNeedingExpiryNotice.
func (r *ArtifactRetentionNotificationRepository) NotifyBucketExpiring(ctx context.Context, projectID, userID, bucketID int64, expiresAt time.Time) error {
	meta, err := json.Marshal(map[string]any{
		"bucket_id":  bucketID,
		"expires_at": expiresAt.UTC().Format(time.RFC3339),
	})
	if err != nil {
		return fmt.Errorf("marshal artifact bucket expiry notification metadata: %w", err)
	}
	if _, err := r.queries.InsertArtifactBucketExpiryNotification(ctx, sqlcgen.InsertArtifactBucketExpiryNotificationParams{
		NotificationUuid: artifactBucketExpiryNotificationUUID(bucketID, expiresAt),
		ProjectID:        int32(projectID),
		UserID:           int32(userID),
		Meta:             meta,
	}); err != nil {
		return fmt.Errorf("insert artifact bucket expiry notification: %w", err)
	}
	return nil
}

// artifactBucketExpiryNotificationUUID derives a deterministic UUID from
// (bucketID, expiresAt), mirroring index_schedule_notification.go's
// scheduleFailureUUID (same hash-to-UUIDv4 shape). Kept as its own small
// copy rather than a shared export: the two have no other coupling, and the
// notifications API only requires "looks like a UUIDv4," not a specific
// derivation.
func artifactBucketExpiryNotificationUUID(bucketID int64, expiresAt time.Time) string {
	key := fmt.Sprintf("artifact-bucket-expiring-%d-%d", bucketID, expiresAt.UTC().Unix())
	digest := sha256.Sum256([]byte(key))
	value := append([]byte(nil), digest[:16]...)
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(value)
	return encoded[0:8] + "-" + encoded[8:12] + "-" +
		encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32]
}
