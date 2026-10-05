package api

// S20a adapter: bridges internal/api/v2/conversations.AttachmentStore
// (a locally-defined interface — see attachments.go's own doc comment on
// why it cannot import internal/infra/db/repos directly) to the real
// repos.Artifact{Buckets,Objects}Repository and
// repos.AttachmentChunksRepository. Only this package (which already
// imports both internal/infra/db/repos and internal/api/v2/conversations
// freely) can wire the two together.

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	v2convs "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/conversations"
	dbrepos "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
)

type attachmentRepoAdapter struct {
	buckets *dbrepos.ArtifactBucketsRepository
	objects *dbrepos.ArtifactObjectsRepository
	chunks  *dbrepos.AttachmentChunksRepository
}

// newAttachmentStore returns nil when pool is nil — mirrors
// newArtifactHandler's own "no database configured, degrade gracefully"
// convention (router.go) rather than erroring at router construction time.
func newAttachmentStore(pool *pgxpool.Pool) v2convs.AttachmentStore {
	if pool == nil {
		return nil
	}
	buckets, err := dbrepos.NewArtifactBucketsRepository(pool)
	if err != nil {
		return nil
	}
	objects, err := dbrepos.NewArtifactObjectsRepository(pool)
	if err != nil {
		return nil
	}
	chunks, err := dbrepos.NewAttachmentChunksRepository(pool)
	if err != nil {
		return nil
	}
	return &attachmentRepoAdapter{buckets: buckets, objects: objects, chunks: chunks}
}

func (a *attachmentRepoAdapter) AttachmentPolicy(ctx context.Context, projectID int64) (bucketName string, maxFileBytes *int64, retentionDays *int32, err error) {
	policy, err := a.objects.GetProjectStoragePolicy(ctx, projectID)
	if err != nil {
		return "", nil, nil, err
	}
	if policy.AttachmentBucket != nil {
		bucketName = *policy.AttachmentBucket
	}
	return bucketName, policy.MaxObjectBytes, policy.RetentionDefaultDays, nil
}

// RequireAttachmentBucket returns the reserved system bucket every
// attachment for projectID lands in, creating it on first use — there is no
// project-creation hook wired anywhere in this service yet (S13's own
// bootstrapper is deliberately unwired for the same reason), so this stage
// cannot depend on one either.
//
// It returns the bucket id only. It deliberately does NOT return the
// bucket's expires_at, which is one absolute instant computed when the
// bucket row was created. An attachment that copied that instant was born
// expired once the project passed retentionDays after its FIRST attachment.
// The retention sweeper then deleted it minutes after upload. The caller
// stamps each attachment with its own deadline instead.
func (a *attachmentRepoAdapter) RequireAttachmentBucket(ctx context.Context, projectID int64, bucketName string, retentionDays int32) (int64, error) {
	row, err := a.buckets.GetBucket(ctx, projectID, bucketName)
	if err == nil {
		return row.ID, nil
	}
	if !errors.Is(err, storage.ErrNotFound) {
		return 0, err
	}

	expiresAt := time.Now().AddDate(0, 0, int(retentionDays))
	row, err = a.buckets.CreateBucket(ctx, dbrepos.NewBucketInput{
		ProjectID:     projectID,
		Name:          bucketName,
		DisplayName:   bucketName,
		BucketType:    "system",
		RetentionDays: &retentionDays,
		ExpiresAt:     &expiresAt,
	})
	if err == nil {
		return row.ID, nil
	}
	if errors.Is(err, storage.ErrAlreadyExists) {
		// Lost a create race against a concurrent first-upload request —
		// the bucket now exists either way.
		row, err = a.buckets.GetBucket(ctx, projectID, bucketName)
		if err != nil {
			return 0, err
		}
		return row.ID, nil
	}
	return 0, err
}

// LookupAttachmentBucket is RequireAttachmentBucket's read-only twin: it
// resolves the bucket id and, when there is no such bucket, propagates the
// repository's storage.ErrNotFound untouched instead of creating one. The
// delete path is its only caller and must not be able to mint a bucket row
// for a project that never uploaded an attachment.
func (a *attachmentRepoAdapter) LookupAttachmentBucket(ctx context.Context, projectID int64, bucketName string) (int64, error) {
	row, err := a.buckets.GetBucket(ctx, projectID, bucketName)
	if err != nil {
		return 0, err
	}
	return row.ID, nil
}

// ListAttachmentObjectKeys projects the objects repository's rows down to the
// keys alone — the delete path needs nothing else off the row, and returning
// the full ObjectRow would drag dbrepos types across the interface boundary
// that this whole adapter exists to keep closed (see the header comment).
func (a *attachmentRepoAdapter) ListAttachmentObjectKeys(ctx context.Context, bucketID int64, keyPrefix string) ([]string, error) {
	rows, err := a.objects.ListObjects(ctx, bucketID, keyPrefix)
	if err != nil {
		return nil, err
	}
	keys := make([]string, len(rows))
	for i, row := range rows {
		keys[i] = row.Key
	}
	return keys, nil
}

// AttachmentObject resolves one attachment's metadata row under the
// runtime reader's gates: the project's own bucket, the reserved system
// bucket type, and an exact key match (ListObjects is a LIKE prefix query).
func (a *attachmentRepoAdapter) AttachmentObject(ctx context.Context, projectID int64, bucketName, key string) (v2convs.AttachmentObjectInfo, error) {
	bucket, err := a.buckets.GetBucket(ctx, projectID, bucketName)
	if err != nil {
		return v2convs.AttachmentObjectInfo{}, err
	}
	if bucket.ProjectID != projectID || bucket.BucketType != "system" {
		return v2convs.AttachmentObjectInfo{}, storage.ErrNotFound
	}
	rows, err := a.objects.ListObjects(ctx, bucket.ID, key)
	if err != nil {
		return v2convs.AttachmentObjectInfo{}, err
	}
	for _, row := range rows {
		if row.Key == key {
			return v2convs.AttachmentObjectInfo{MediaType: row.MediaType, ByteLength: row.ByteLength}, nil
		}
	}
	return v2convs.AttachmentObjectInfo{}, storage.ErrNotFound
}

func (a *attachmentRepoAdapter) DeleteAttachmentObjects(ctx context.Context, bucketID int64, keys []string) error {
	return a.objects.DeleteObjects(ctx, bucketID, keys)
}

func (a *attachmentRepoAdapter) RecordAttachmentObject(ctx context.Context, bucketID int64, key string, byteLength int64, mediaType string, expiresAt *time.Time) error {
	_, err := a.objects.UpsertObject(ctx, dbrepos.NewObjectInput{
		BucketID:   bucketID,
		Key:        key,
		ByteLength: byteLength,
		MediaType:  mediaType,
		ExpiresAt:  expiresAt,
	})
	return err
}

func (a *attachmentRepoAdapter) UpsertAttachmentChunk(ctx context.Context, projectID int64, conversationID, fileID string, chunkIndex, totalChunks int32, fileName, contentType string, body []byte) error {
	return a.chunks.UpsertChunk(ctx, projectID, conversationID, fileID, chunkIndex, totalChunks, fileName, contentType, body)
}

func (a *attachmentRepoAdapter) CountAttachmentChunks(ctx context.Context, projectID int64, conversationID, fileID string) (int64, error) {
	return a.chunks.CountChunks(ctx, projectID, conversationID, fileID)
}

func (a *attachmentRepoAdapter) ListAttachmentChunksOrdered(ctx context.Context, projectID int64, conversationID, fileID string) ([]v2convs.AttachmentChunk, error) {
	rows, err := a.chunks.ListChunksOrdered(ctx, projectID, conversationID, fileID)
	if err != nil {
		return nil, err
	}
	out := make([]v2convs.AttachmentChunk, len(rows))
	for i, row := range rows {
		out[i] = v2convs.AttachmentChunk{ChunkIndex: row.ChunkIndex, Bytes: row.Bytes}
	}
	return out, nil
}

func (a *attachmentRepoAdapter) DeleteAttachmentChunks(ctx context.Context, projectID int64, conversationID, fileID string) error {
	return a.chunks.DeleteChunks(ctx, projectID, conversationID, fileID)
}

var _ v2convs.AttachmentStore = (*attachmentRepoAdapter)(nil)
