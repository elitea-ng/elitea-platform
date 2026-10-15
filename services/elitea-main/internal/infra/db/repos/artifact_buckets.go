package repos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// BucketRow is the domain-shaped view of one elitea_storage.buckets row. All
// bucket-returning queries in this file select every table column (including
// deleted_at, always NULL for a row any of them can return), so sqlc reuses
// its single EliteaStorageBucket model instead of generating a distinct Row
// type per query — this is the one place that model is mapped from.
type BucketRow struct {
	ID            int64
	ProjectID     int64
	Name          string
	DisplayName   string
	BucketType    string
	IsPinned      bool
	Tags          json.RawMessage
	RetentionDays *int32
	ExpiresAt     *time.Time
	NotifiedAt    *time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

func bucketRowFromModel(row sqlcgen.EliteaStorageBucket) BucketRow {
	return BucketRow{
		ID:            row.ID,
		ProjectID:     row.ProjectID,
		Name:          row.Name,
		DisplayName:   row.DisplayName,
		BucketType:    row.BucketType,
		IsPinned:      row.IsPinned,
		Tags:          json.RawMessage(row.Tags),
		RetentionDays: row.RetentionDays,
		ExpiresAt:     fromTimestamptz(row.ExpiresAt),
		NotifiedAt:    fromTimestamptz(row.NotifiedAt),
		CreatedAt:     row.CreatedAt.Time,
		UpdatedAt:     row.UpdatedAt.Time,
	}
}

func toTimestamptz(t *time.Time) pgtype.Timestamptz {
	if t == nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: *t, Valid: true}
}

func fromTimestamptz(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	tm := t.Time
	return &tm
}

// NewBucketInput is the set of caller-supplied fields for CreateBucket.
type NewBucketInput struct {
	ProjectID     int64
	Name          string
	DisplayName   string
	BucketType    string
	RetentionDays *int32
	ExpiresAt     *time.Time
}

type artifactBucketQueries interface {
	ListArtifactBuckets(context.Context, int64) ([]sqlcgen.EliteaStorageBucket, error)
	GetArtifactBucket(context.Context, sqlcgen.GetArtifactBucketParams) (sqlcgen.EliteaStorageBucket, error)
	GetArtifactBucketByID(context.Context, int64) (sqlcgen.EliteaStorageBucket, error)
	CreateArtifactBucket(context.Context, sqlcgen.CreateArtifactBucketParams) (sqlcgen.EliteaStorageBucket, error)
	UpdateArtifactBucketRetention(context.Context, sqlcgen.UpdateArtifactBucketRetentionParams) (sqlcgen.EliteaStorageBucket, error)
	SetArtifactBucketPinned(context.Context, sqlcgen.SetArtifactBucketPinnedParams) (sqlcgen.EliteaStorageBucket, error)
	UpdateArtifactBucketTags(context.Context, sqlcgen.UpdateArtifactBucketTagsParams) (sqlcgen.EliteaStorageBucket, error)
	SoftDeleteArtifactBucket(context.Context, int64) (int64, error)
	ListArtifactBucketsNeedingExpiryNotice(context.Context, sqlcgen.ListArtifactBucketsNeedingExpiryNoticeParams) ([]sqlcgen.EliteaStorageBucket, error)
	MarkArtifactBucketNotified(context.Context, int64) (int64, error)
	ListArtifactProjectIDsWithBuckets(context.Context) ([]int64, error)
}

// ArtifactBucketsRepository is the metadata store for elitea_storage.buckets
// — the logical-bucket records (name, retention, tags, pin state) that sit
// alongside the physical ObjectStore backend (S1-S5), not a replacement for
// it. See docs/plans/storage-migration-plan.md S6.
type ArtifactBucketsRepository struct {
	queries artifactBucketQueries
}

func NewArtifactBucketsRepository(pool *pgxpool.Pool) (*ArtifactBucketsRepository, error) {
	if pool == nil {
		return nil, errors.New("artifact buckets database is required")
	}
	return newArtifactBucketsRepository(sqlcgen.New(pool))
}

func newArtifactBucketsRepository(queries artifactBucketQueries) (*ArtifactBucketsRepository, error) {
	if queries == nil {
		return nil, errors.New("artifact buckets database is required")
	}
	return &ArtifactBucketsRepository{queries: queries}, nil
}

func (r *ArtifactBucketsRepository) ListBuckets(ctx context.Context, projectID int64) ([]BucketRow, error) {
	rows, err := r.queries.ListArtifactBuckets(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("list artifact buckets: %w", err)
	}
	result := make([]BucketRow, len(rows))
	for i, row := range rows {
		result[i] = bucketRowFromModel(row)
	}
	return result, nil
}

func (r *ArtifactBucketsRepository) GetBucket(ctx context.Context, projectID int64, name string) (BucketRow, error) {
	row, err := r.queries.GetArtifactBucket(ctx, sqlcgen.GetArtifactBucketParams{
		ProjectID: projectID,
		Name:      name,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return BucketRow{}, storage.ErrNotFound
	}
	if err != nil {
		return BucketRow{}, fmt.Errorf("get artifact bucket: %w", err)
	}
	return bucketRowFromModel(row), nil
}

// CreateBucket maps a unique-violation (23505, the project_id+name partial
// unique index) to storage.ErrAlreadyExists.
// GetBucketByID looks up a bucket by its primary key, without a request-bound
// projectID — the retention sweeper (S14) is this method's only caller,
// since ListExpiredObjects scans across every project and only has a
// bucket_id to resolve back to (project_id, name) for a physical ObjectRef.
func (r *ArtifactBucketsRepository) GetBucketByID(ctx context.Context, id int64) (BucketRow, error) {
	row, err := r.queries.GetArtifactBucketByID(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return BucketRow{}, storage.ErrNotFound
	}
	if err != nil {
		return BucketRow{}, fmt.Errorf("get artifact bucket by id: %w", err)
	}
	return bucketRowFromModel(row), nil
}

func (r *ArtifactBucketsRepository) CreateBucket(ctx context.Context, input NewBucketInput) (BucketRow, error) {
	// The workspace bucket is created by the workspace store alone
	// (WorkspaceStoreRepository.EnsureWorkspaceBucket). A reserved name is an
	// invalid name here, never a conflict, so no caller learns that the hidden
	// bucket exists (ADR-0034 decision 3).
	if storage.ReservedBucketName(input.Name) || input.BucketType == storage.WorkspaceBucketType {
		return BucketRow{}, fmt.Errorf("%w: reserved bucket name", storage.ErrInvalidKey)
	}
	row, err := r.queries.CreateArtifactBucket(ctx, sqlcgen.CreateArtifactBucketParams{
		ProjectID:     input.ProjectID,
		Name:          input.Name,
		DisplayName:   input.DisplayName,
		BucketType:    input.BucketType,
		RetentionDays: input.RetentionDays,
		ExpiresAt:     toTimestamptz(input.ExpiresAt),
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return BucketRow{}, storage.ErrAlreadyExists
		}
		return BucketRow{}, fmt.Errorf("create artifact bucket: %w", err)
	}
	return bucketRowFromModel(row), nil
}

func (r *ArtifactBucketsRepository) UpdateBucketRetention(ctx context.Context, id int64, retentionDays *int32, expiresAt *time.Time) (BucketRow, error) {
	row, err := r.queries.UpdateArtifactBucketRetention(ctx, sqlcgen.UpdateArtifactBucketRetentionParams{
		ID:            id,
		RetentionDays: retentionDays,
		ExpiresAt:     toTimestamptz(expiresAt),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return BucketRow{}, storage.ErrNotFound
	}
	if err != nil {
		return BucketRow{}, fmt.Errorf("update artifact bucket retention: %w", err)
	}
	return bucketRowFromModel(row), nil
}

func (r *ArtifactBucketsRepository) SetBucketPinned(ctx context.Context, id int64, pinned bool) (BucketRow, error) {
	row, err := r.queries.SetArtifactBucketPinned(ctx, sqlcgen.SetArtifactBucketPinnedParams{
		ID:       id,
		IsPinned: pinned,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return BucketRow{}, storage.ErrNotFound
	}
	if err != nil {
		return BucketRow{}, fmt.Errorf("set artifact bucket pinned: %w", err)
	}
	return bucketRowFromModel(row), nil
}

// UpdateBucketTags replaces the bucket's tags wholesale — closes the gap S19's
// conformance suite otherwise finds (see S6/S8 in the plan): without this
// method there is no way to persist a tag update at all.
func (r *ArtifactBucketsRepository) UpdateBucketTags(ctx context.Context, id int64, tags json.RawMessage) (BucketRow, error) {
	if len(tags) == 0 {
		tags = json.RawMessage(`{}`)
	}
	row, err := r.queries.UpdateArtifactBucketTags(ctx, sqlcgen.UpdateArtifactBucketTagsParams{
		ID:   id,
		Tags: tags,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return BucketRow{}, storage.ErrNotFound
	}
	if err != nil {
		return BucketRow{}, fmt.Errorf("update artifact bucket tags: %w", err)
	}
	return bucketRowFromModel(row), nil
}

func (r *ArtifactBucketsRepository) SoftDeleteBucket(ctx context.Context, id int64) error {
	rows, err := r.queries.SoftDeleteArtifactBucket(ctx, id)
	if err != nil {
		return fmt.Errorf("soft delete artifact bucket: %w", err)
	}
	if rows == 0 {
		return storage.ErrNotFound
	}
	return nil
}

// ListBucketsNeedingExpiryNotice returns buckets whose expires_at falls
// within the next `within` duration and that have not yet been notified. A
// background sweeper (S14) has no request-bound projectID to scope by, so
// this scans across every project.
func (r *ArtifactBucketsRepository) ListBucketsNeedingExpiryNotice(ctx context.Context, within time.Duration, limit int32) ([]BucketRow, error) {
	threshold := time.Now().Add(within)
	rows, err := r.queries.ListArtifactBucketsNeedingExpiryNotice(ctx, sqlcgen.ListArtifactBucketsNeedingExpiryNoticeParams{
		ExpiresAt: toTimestamptz(&threshold),
		Limit:     limit,
	})
	if err != nil {
		return nil, fmt.Errorf("list artifact buckets needing expiry notice: %w", err)
	}
	result := make([]BucketRow, len(rows))
	for i, row := range rows {
		result[i] = bucketRowFromModel(row)
	}
	return result, nil
}

// ListProjectIDsWithBuckets enumerates every project that owns at least one
// non-deleted bucket — S18's per-project byte-usage gauge needs this to
// know which projects to call SumProjectBytes for on each retention sweeper
// tick. This service has no "projects" table of its own to enumerate
// instead; "has a bucket" is the operational definition of a known project
// here.
func (r *ArtifactBucketsRepository) ListProjectIDsWithBuckets(ctx context.Context) ([]int64, error) {
	ids, err := r.queries.ListArtifactProjectIDsWithBuckets(ctx)
	if err != nil {
		return nil, fmt.Errorf("list artifact project ids with buckets: %w", err)
	}
	return ids, nil
}

func (r *ArtifactBucketsRepository) MarkBucketNotified(ctx context.Context, bucketID int64) error {
	rows, err := r.queries.MarkArtifactBucketNotified(ctx, bucketID)
	if err != nil {
		return fmt.Errorf("mark artifact bucket notified: %w", err)
	}
	if rows == 0 {
		return storage.ErrNotFound
	}
	return nil
}
