package repos

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ObjectRow is the domain-shaped view of one elitea_storage.objects row —
// per-object metadata (size, digest, media type, classification, scan
// state) tracked alongside the physical bytes an ObjectStore backend (S1-S5)
// holds. It is not a cache of object listings; S9's list endpoint reads the
// backend directly via ObjectStore.List.
type ObjectRow struct {
	ID             int64
	BucketID       int64
	Key            string
	ByteLength     int64
	MediaType      string
	DigestAlg      *string
	Digest         []byte
	Classification string
	ScanState      string
	ExpiresAt      *time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

func objectRowFromModel(row sqlcgen.EliteaStorageObject) ObjectRow {
	return ObjectRow{
		ID:             row.ID,
		BucketID:       row.BucketID,
		Key:            row.Key,
		ByteLength:     row.ByteLength,
		MediaType:      row.MediaType,
		DigestAlg:      row.DigestAlg,
		Digest:         row.Digest,
		Classification: row.Classification,
		ScanState:      row.ScanState,
		ExpiresAt:      fromTimestamptz(row.ExpiresAt),
		CreatedAt:      row.CreatedAt.Time,
		UpdatedAt:      row.UpdatedAt.Time,
	}
}

// ProjectStoragePolicy is the domain-shaped view of one
// elitea_storage.project_storage_policy row. A nil field means unlimited or
// default — see GetProjectStoragePolicy.
type ProjectStoragePolicy struct {
	ProjectID            int64
	MaxObjectBytes       *int64
	MaxTotalBytes        *int64
	RetentionDefaultDays *int32
	RetentionMaxDays     *int32
	AttachmentBucket     *string
}

func projectStoragePolicyFromModel(row sqlcgen.EliteaStorageProjectStoragePolicy) ProjectStoragePolicy {
	return ProjectStoragePolicy{
		ProjectID:            row.ProjectID,
		MaxObjectBytes:       row.MaxObjectBytes,
		MaxTotalBytes:        row.MaxTotalBytes,
		RetentionDefaultDays: row.RetentionDefaultDays,
		RetentionMaxDays:     row.RetentionMaxDays,
		AttachmentBucket:     row.AttachmentBucket,
	}
}

// NewObjectInput is the set of caller-supplied fields for UpsertObject.
type NewObjectInput struct {
	BucketID       int64
	Key            string
	ByteLength     int64
	MediaType      string
	DigestAlg      *string
	Digest         []byte
	Classification string
	ScanState      string
	ExpiresAt      *time.Time
}

type artifactObjectQueries interface {
	UpsertArtifactObject(context.Context, sqlcgen.UpsertArtifactObjectParams) (sqlcgen.EliteaStorageObject, error)
	ReserveArtifactObjectKey(context.Context, sqlcgen.ReserveArtifactObjectKeyParams) (int64, error)
	ListArtifactObjects(context.Context, sqlcgen.ListArtifactObjectsParams) ([]sqlcgen.EliteaStorageObject, error)
	DeleteArtifactObjects(context.Context, sqlcgen.DeleteArtifactObjectsParams) (int64, error)
	SumArtifactBucketBytes(context.Context, int64) (int64, error)
	CountArtifactBucketObjects(context.Context, int64) (int64, error)
	SumArtifactProjectBytes(context.Context, int64) (int64, error)
	GetArtifactProjectStoragePolicy(context.Context, int64) (sqlcgen.EliteaStorageProjectStoragePolicy, error)
	ListExpiredArtifactObjects(context.Context, sqlcgen.ListExpiredArtifactObjectsParams) ([]sqlcgen.EliteaStorageObject, error)
	DeleteArtifactObjectRows(context.Context, []int64) (int64, error)
}

// ArtifactObjectsRepository is the metadata store for elitea_storage.objects
// and the project-level storage policy that gates it. See
// docs/plans/storage-migration-plan.md S6.
type ArtifactObjectsRepository struct {
	queries artifactObjectQueries
}

func NewArtifactObjectsRepository(pool *pgxpool.Pool) (*ArtifactObjectsRepository, error) {
	if pool == nil {
		return nil, errors.New("artifact objects database is required")
	}
	return newArtifactObjectsRepository(sqlcgen.New(pool))
}

func newArtifactObjectsRepository(queries artifactObjectQueries) (*ArtifactObjectsRepository, error) {
	if queries == nil {
		return nil, errors.New("artifact objects database is required")
	}
	return &ArtifactObjectsRepository{queries: queries}, nil
}

// UpsertObject inserts or, on a (bucket_id, key) conflict, overwrites an
// object's metadata row — the source of truth for size/digest/classification
// tracking is whatever the caller just wrote to the physical ObjectStore
// backend (S1-S5); this call records that fact, it does not itself write
// bytes anywhere.
func (r *ArtifactObjectsRepository) UpsertObject(ctx context.Context, input NewObjectInput) (ObjectRow, error) {
	classification := input.Classification
	if classification == "" {
		classification = "internal"
	}
	scanState := input.ScanState
	if scanState == "" {
		scanState = "not_scanned"
	}
	row, err := r.queries.UpsertArtifactObject(ctx, sqlcgen.UpsertArtifactObjectParams{
		BucketID:       input.BucketID,
		Key:            input.Key,
		ByteLength:     input.ByteLength,
		MediaType:      input.MediaType,
		DigestAlg:      input.DigestAlg,
		Digest:         input.Digest,
		Classification: classification,
		ScanState:      scanState,
		ExpiresAt:      toTimestamptz(input.ExpiresAt),
	})
	if err != nil {
		return ObjectRow{}, fmt.Errorf("upsert artifact object: %w", err)
	}
	return objectRowFromModel(row), nil
}

// ReserveObjectKey claims (bucketID, key) for bytes the caller is about to
// write, with a zero-length placeholder row, and reports whether it got the
// key. It never overwrites a row that already holds the key, and the unique
// (bucket_id, key) index decides between two concurrent claims, so two
// callers never both get one key. UpsertObject replaces the placeholder once
// the bytes are stored; DeleteObjects releases a key whose write failed.
func (r *ArtifactObjectsRepository) ReserveObjectKey(ctx context.Context, bucketID int64, key, mediaType string, expiresAt *time.Time) (bool, error) {
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	affected, err := r.queries.ReserveArtifactObjectKey(ctx, sqlcgen.ReserveArtifactObjectKeyParams{
		BucketID:  bucketID,
		Key:       key,
		MediaType: mediaType,
		ExpiresAt: toTimestamptz(expiresAt),
	})
	if err != nil {
		return false, fmt.Errorf("reserve artifact object key: %w", err)
	}
	return affected == 1, nil
}

// ListObjects lists an individual bucket's object metadata rows, optionally
// filtered by a literal key prefix. This is a metadata-table read, not the
// primary object listing path — S9's list endpoint calls ObjectStore.List
// against the physical backend directly.
func (r *ArtifactObjectsRepository) ListObjects(ctx context.Context, bucketID int64, keyPrefix string) ([]ObjectRow, error) {
	rows, err := r.queries.ListArtifactObjects(ctx, sqlcgen.ListArtifactObjectsParams{
		BucketID:  bucketID,
		KeyPrefix: keyPrefix,
	})
	if err != nil {
		return nil, fmt.Errorf("list artifact objects: %w", err)
	}
	result := make([]ObjectRow, len(rows))
	for i, row := range rows {
		result[i] = objectRowFromModel(row)
	}
	return result, nil
}

// DeleteObjects removes metadata rows for the given keys within one bucket.
// It does not delete bytes from the physical backend — pair it with
// ObjectStore.DeleteBatch (S1-S5), which does.
func (r *ArtifactObjectsRepository) DeleteObjects(ctx context.Context, bucketID int64, keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	if _, err := r.queries.DeleteArtifactObjects(ctx, sqlcgen.DeleteArtifactObjectsParams{
		BucketID: bucketID,
		Keys:     keys,
	}); err != nil {
		return fmt.Errorf("delete artifact objects: %w", err)
	}
	return nil
}

// SumBucketBytes is a single-bucket aggregate — it feeds one bucket's
// size_bytes response field (S8). It does not compose into a project total;
// see SumProjectBytes for that.
func (r *ArtifactObjectsRepository) SumBucketBytes(ctx context.Context, bucketID int64) (int64, error) {
	sum, err := r.queries.SumArtifactBucketBytes(ctx, bucketID)
	if err != nil {
		return 0, fmt.Errorf("sum artifact bucket bytes: %w", err)
	}
	return sum, nil
}

func (r *ArtifactObjectsRepository) CountBucketObjects(ctx context.Context, bucketID int64) (int64, error) {
	count, err := r.queries.CountArtifactBucketObjects(ctx, bucketID)
	if err != nil {
		return 0, fmt.Errorf("count artifact bucket objects: %w", err)
	}
	return count, nil
}

// SumProjectBytes sums across every non-deleted bucket a project owns. S12's
// project-quota check is the only caller — SumBucketBytes cannot be looped
// over to produce this; see the plan's S6/S12 notes.
func (r *ArtifactObjectsRepository) SumProjectBytes(ctx context.Context, projectID int64) (int64, error) {
	sum, err := r.queries.SumArtifactProjectBytes(ctx, projectID)
	if err != nil {
		return 0, fmt.Errorf("sum artifact project bytes: %w", err)
	}
	return sum, nil
}

// GetProjectStoragePolicy reads the project's storage policy row. A missing
// row is not an error — every field is unlimited/default in that case, per
// the plan: S8's retention-limit check and S12's quota check both rely on
// this not erroring on absence.
func (r *ArtifactObjectsRepository) GetProjectStoragePolicy(ctx context.Context, projectID int64) (ProjectStoragePolicy, error) {
	row, err := r.queries.GetArtifactProjectStoragePolicy(ctx, projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProjectStoragePolicy{ProjectID: projectID}, nil
	}
	if err != nil {
		return ProjectStoragePolicy{}, fmt.Errorf("get artifact project storage policy: %w", err)
	}
	return projectStoragePolicyFromModel(row), nil
}

// ListExpiredObjects returns a bounded batch of objects whose expires_at has
// passed olderThan, backed by the objects_expiry partial index (S6's
// migration) — the retention sweeper (S14) is this method's only caller.
func (r *ArtifactObjectsRepository) ListExpiredObjects(ctx context.Context, olderThan time.Time, limit int32) ([]ObjectRow, error) {
	rows, err := r.queries.ListExpiredArtifactObjects(ctx, sqlcgen.ListExpiredArtifactObjectsParams{
		ExpiresAt: toTimestamptz(&olderThan),
		Limit:     limit,
	})
	if err != nil {
		return nil, fmt.Errorf("list expired artifact objects: %w", err)
	}
	result := make([]ObjectRow, len(rows))
	for i, row := range rows {
		result[i] = objectRowFromModel(row)
	}
	return result, nil
}

// DeleteObjectRows deletes metadata rows by primary key, across buckets —
// the retention sweeper (S14) has no request-bound bucket to scope
// DeleteObjects by, so it deletes by the ids ListExpiredObjects returned.
func (r *ArtifactObjectsRepository) DeleteObjectRows(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	if _, err := r.queries.DeleteArtifactObjectRows(ctx, ids); err != nil {
		return fmt.Errorf("delete artifact object rows: %w", err)
	}
	return nil
}
