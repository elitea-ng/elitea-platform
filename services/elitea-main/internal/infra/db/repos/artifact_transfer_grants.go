package repos

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TransferGrantRow is the domain-shaped view of one
// elitea_storage.transfer_grants row (S15). UploadID (S16) is non-nil only
// for a grant CreateTransferGrant started as a native multipart upload.
type TransferGrantRow struct {
	ID          string
	ProjectID   int64
	BucketID    int64
	Key         string
	Method      string
	ContentType string
	MaxBytes    int64
	DigestAlg   *string
	Digest      []byte
	UploadID    *string
	ExpiresAt   time.Time
	ConsumedAt  *time.Time
	CreatedAt   time.Time
}

// NewTransferGrantInput is the set of caller-supplied fields for
// CreateTransferGrant.
type NewTransferGrantInput struct {
	ID          string
	ProjectID   int64
	BucketID    int64
	Key         string
	Method      string
	ContentType string
	MaxBytes    int64
	DigestAlg   *string
	Digest      []byte
	UploadID    *string
	ExpiresAt   time.Time
}

// ExpiredTransferGrantRow is one grant the sweeper has claimed. It carries
// only the fields the byte reclamation needs.
type ExpiredTransferGrantRow struct {
	ID        string
	ProjectID int64
	BucketID  int64
	Key       string
	Method    string
	UploadID  *string
	ExpiresAt time.Time
}

type artifactTransferGrantQueries interface {
	CreateArtifactTransferGrant(context.Context, sqlcgen.CreateArtifactTransferGrantParams) (sqlcgen.CreateArtifactTransferGrantRow, error)
	GetArtifactTransferGrant(context.Context, sqlcgen.GetArtifactTransferGrantParams) (sqlcgen.GetArtifactTransferGrantRow, error)
	GetArtifactTransferGrantByID(context.Context, string) (sqlcgen.GetArtifactTransferGrantByIDRow, error)
	MarkArtifactTransferGrantConsumed(context.Context, string) (int64, error)
	ClaimArtifactTransferGrantForCommit(context.Context, string) (pgtype.Timestamptz, error)
	ReleaseArtifactTransferGrantClaim(context.Context, sqlcgen.ReleaseArtifactTransferGrantClaimParams) (int64, error)
	ClaimExpiredArtifactTransferGrants(context.Context, sqlcgen.ClaimExpiredArtifactTransferGrantsParams) ([]sqlcgen.ClaimExpiredArtifactTransferGrantsRow, error)
	ListExpiredArtifactGrantStaging(context.Context, sqlcgen.ListExpiredArtifactGrantStagingParams) ([]sqlcgen.ListExpiredArtifactGrantStagingRow, error)
	ClearArtifactGrantStaging(context.Context, sqlcgen.ClearArtifactGrantStagingParams) error
}

// GrantStagingSuffix makes a presigned single-shot grant's staging key:
// the grant id plus this suffix (api/v2/artifacts grantStagingKey). The
// ListExpiredArtifactGrantStaging query spells the same literal.
const GrantStagingSuffix = ".upload"

// IsGrantStagingKey reports whether key is the staging key of grant id.
func IsGrantStagingKey(id, key string) bool {
	return key == id+GrantStagingSuffix
}

// GrantStagingRow is one expired grant whose staging object the sweeper
// reclaims (ListExpiredGrantStaging).
type GrantStagingRow struct {
	ID        string
	ProjectID int64
	BucketID  int64
	Key       string
}

// ArtifactTransferGrantsRepository is the metadata store for
// elitea_storage.transfer_grants. See docs/plans/storage-migration-plan.md
// S15.
type ArtifactTransferGrantsRepository struct {
	queries artifactTransferGrantQueries
}

func NewArtifactTransferGrantsRepository(pool *pgxpool.Pool) (*ArtifactTransferGrantsRepository, error) {
	if pool == nil {
		return nil, errors.New("artifact transfer grants database is required")
	}
	return newArtifactTransferGrantsRepository(sqlcgen.New(pool))
}

func newArtifactTransferGrantsRepository(queries artifactTransferGrantQueries) (*ArtifactTransferGrantsRepository, error) {
	if queries == nil {
		return nil, errors.New("artifact transfer grants database is required")
	}
	return &ArtifactTransferGrantsRepository{queries: queries}, nil
}

func (r *ArtifactTransferGrantsRepository) CreateTransferGrant(ctx context.Context, input NewTransferGrantInput) (TransferGrantRow, error) {
	expiresAt := input.ExpiresAt
	row, err := r.queries.CreateArtifactTransferGrant(ctx, sqlcgen.CreateArtifactTransferGrantParams{
		ID:          input.ID,
		ProjectID:   input.ProjectID,
		BucketID:    input.BucketID,
		Key:         input.Key,
		Method:      input.Method,
		ContentType: input.ContentType,
		MaxBytes:    input.MaxBytes,
		DigestAlg:   input.DigestAlg,
		Digest:      input.Digest,
		UploadID:    input.UploadID,
		ExpiresAt:   toTimestamptz(&expiresAt),
	})
	if err != nil {
		return TransferGrantRow{}, fmt.Errorf("create artifact transfer grant: %w", err)
	}
	return TransferGrantRow{
		ID: row.ID, ProjectID: row.ProjectID, BucketID: row.BucketID, Key: row.Key,
		Method: row.Method, ContentType: row.ContentType, MaxBytes: row.MaxBytes,
		DigestAlg: row.DigestAlg, Digest: row.Digest, UploadID: row.UploadID,
		ExpiresAt: row.ExpiresAt.Time, ConsumedAt: fromTimestamptz(row.ConsumedAt), CreatedAt: row.CreatedAt.Time,
	}, nil
}

// GetTransferGrant is scoped by projectID — see the wrapped sqlc query's own
// comment for why.
func (r *ArtifactTransferGrantsRepository) GetTransferGrant(ctx context.Context, id string, projectID int64) (TransferGrantRow, error) {
	row, err := r.queries.GetArtifactTransferGrant(ctx, sqlcgen.GetArtifactTransferGrantParams{ID: id, ProjectID: projectID})
	if errors.Is(err, pgx.ErrNoRows) {
		return TransferGrantRow{}, storage.ErrNotFound
	}
	if err != nil {
		return TransferGrantRow{}, fmt.Errorf("get artifact transfer grant: %w", err)
	}
	return TransferGrantRow{
		ID: row.ID, ProjectID: row.ProjectID, BucketID: row.BucketID, Key: row.Key,
		Method: row.Method, ContentType: row.ContentType, MaxBytes: row.MaxBytes,
		DigestAlg: row.DigestAlg, Digest: row.Digest, UploadID: row.UploadID,
		ExpiresAt: row.ExpiresAt.Time, ConsumedAt: fromTimestamptz(row.ConsumedAt), CreatedAt: row.CreatedAt.Time,
	}, nil
}

// GetTransferGrantByID is unscoped by projectID — see the wrapped sqlc
// query's own comment for why S16's multipart continuation endpoints need
// this alongside the project-scoped GetTransferGrant.
func (r *ArtifactTransferGrantsRepository) GetTransferGrantByID(ctx context.Context, id string) (TransferGrantRow, error) {
	row, err := r.queries.GetArtifactTransferGrantByID(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return TransferGrantRow{}, storage.ErrNotFound
	}
	if err != nil {
		return TransferGrantRow{}, fmt.Errorf("get artifact transfer grant by id: %w", err)
	}
	return TransferGrantRow{
		ID: row.ID, ProjectID: row.ProjectID, BucketID: row.BucketID, Key: row.Key,
		Method: row.Method, ContentType: row.ContentType, MaxBytes: row.MaxBytes,
		DigestAlg: row.DigestAlg, Digest: row.Digest, UploadID: row.UploadID,
		ExpiresAt: row.ExpiresAt.Time, ConsumedAt: fromTimestamptz(row.ConsumedAt), CreatedAt: row.CreatedAt.Time,
	}, nil
}

// MarkTransferGrantConsumed stamps consumed_at, enforcing single-use: a
// grant already consumed (or a nonexistent id) returns
// storage.ErrAlreadyExists — matching the 409 the plan's acceptance
// criteria requires for "a second commit on the same grant." The caller
// (commitTransferGrant) has always already fetched the row by the time it
// calls this, so an id that genuinely does not exist is not a realistic
// path here — see the wrapped sqlc query's own comment.
func (r *ArtifactTransferGrantsRepository) MarkTransferGrantConsumed(ctx context.Context, id string) error {
	rows, err := r.queries.MarkArtifactTransferGrantConsumed(ctx, id)
	if err != nil {
		return fmt.Errorf("mark artifact transfer grant consumed: %w", err)
	}
	if rows == 0 {
		return storage.ErrAlreadyExists
	}
	return nil
}

// ClaimTransferGrant stamps consumed_at like MarkTransferGrantConsumed, but
// up front: a single-shot commit claims the grant before it writes the
// grant's final key, so two concurrent commits can never both copy into it.
// It returns the stamp ReleaseTransferGrantClaim needs. An already consumed
// (or already claimed) grant returns storage.ErrAlreadyExists.
func (r *ArtifactTransferGrantsRepository) ClaimTransferGrant(ctx context.Context, id string) (time.Time, error) {
	claimedAt, err := r.queries.ClaimArtifactTransferGrantForCommit(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, storage.ErrAlreadyExists
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("claim artifact transfer grant: %w", err)
	}
	return claimedAt.Time, nil
}

// ReleaseTransferGrantClaim hands back a claim ClaimTransferGrant took, after
// a commit that did not complete, so the caller can retry on the same grant.
// It reports whether the claim was still this commit's to release.
func (r *ArtifactTransferGrantsRepository) ReleaseTransferGrantClaim(ctx context.Context, id string, claimedAt time.Time) (bool, error) {
	rows, err := r.queries.ReleaseArtifactTransferGrantClaim(ctx, sqlcgen.ReleaseArtifactTransferGrantClaimParams{
		ID: id, ClaimedAt: pgtype.Timestamptz{Time: claimedAt, Valid: true},
	})
	if err != nil {
		return false, fmt.Errorf("release artifact transfer grant claim: %w", err)
	}
	return rows > 0, nil
}

// ClaimExpiredTransferGrants marks a bounded batch of expired, unconsumed
// grants consumed and returns them for byte reclamation. See the wrapped
// sqlc query for why the claim and the read are one statement.
//
// Pass an olderThan that already includes a grace margin. A commit that
// started just before the grant's own expiry must finish without racing the
// sweeper.
func (r *ArtifactTransferGrantsRepository) ClaimExpiredTransferGrants(ctx context.Context, olderThan time.Time, limit int32) ([]ExpiredTransferGrantRow, error) {
	rows, err := r.queries.ClaimExpiredArtifactTransferGrants(ctx, sqlcgen.ClaimExpiredArtifactTransferGrantsParams{
		OlderThan: toTimestamptz(&olderThan),
		RowLimit:  limit,
	})
	if err != nil {
		return nil, fmt.Errorf("claim expired artifact transfer grants: %w", err)
	}
	out := make([]ExpiredTransferGrantRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, ExpiredTransferGrantRow{
			ID: row.ID, ProjectID: row.ProjectID, BucketID: row.BucketID, Key: row.Key,
			Method: row.Method, UploadID: row.UploadID, ExpiresAt: row.ExpiresAt.Time,
		})
	}
	return out, nil
}

// ListExpiredGrantStaging returns a bounded batch of expired grants whose
// presigned staging object may still exist. See the wrapped sqlc query.
// olderThan must include the same grace margin ClaimExpiredTransferGrants
// gets, so no URL that could still write is reclaimed under.
func (r *ArtifactTransferGrantsRepository) ListExpiredGrantStaging(ctx context.Context, olderThan time.Time, limit int32) ([]GrantStagingRow, error) {
	rows, err := r.queries.ListExpiredArtifactGrantStaging(ctx, sqlcgen.ListExpiredArtifactGrantStagingParams{
		OlderThan: toTimestamptz(&olderThan),
		RowLimit:  limit,
	})
	if err != nil {
		return nil, fmt.Errorf("list expired artifact grant staging: %w", err)
	}
	out := make([]GrantStagingRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, GrantStagingRow{ID: row.ID, ProjectID: row.ProjectID, BucketID: row.BucketID, Key: row.Key})
	}
	return out, nil
}

// ClearGrantStaging records that a grant's staging object is gone.
func (r *ArtifactTransferGrantsRepository) ClearGrantStaging(ctx context.Context, id, key string) error {
	if err := r.queries.ClearArtifactGrantStaging(ctx, sqlcgen.ClearArtifactGrantStagingParams{ID: id, Key: key}); err != nil {
		return fmt.Errorf("clear artifact grant staging: %w", err)
	}
	return nil
}
