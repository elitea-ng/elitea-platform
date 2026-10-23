package repos

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
)

// AttachmentChunk is one row of elitea_storage.attachment_chunks — a single
// chunk of a not-yet-complete chunked attachment upload (S20a). See
// migrations/shared/0059_attachment_chunks.sql for why Postgres, not local
// disk, is the shared buffer: any elitea-main replica may receive any chunk
// of one upload, with no sticky session.
type AttachmentChunk struct {
	ChunkIndex int32
	Bytes      []byte
}

// AttachmentChunksRepository is the S20a metadata/byte-buffer store for
// in-flight chunked chat-attachment uploads.
type AttachmentChunksRepository struct {
	queries *sqlcgen.Queries
}

func NewAttachmentChunksRepository(pool *pgxpool.Pool) (*AttachmentChunksRepository, error) {
	if pool == nil {
		return nil, errors.New("attachment chunks database is required")
	}
	return &AttachmentChunksRepository{queries: sqlcgen.New(pool)}, nil
}

// UpsertChunk records one chunk. A retried chunk_index (client timeout,
// network retry) overwrites the previous bytes for that index rather than
// being counted twice — see the migration's own doc comment.
func (r *AttachmentChunksRepository) UpsertChunk(ctx context.Context, projectID int64, conversationID, fileID string, chunkIndex, totalChunks int32, fileName, contentType string, body []byte) error {
	if err := r.queries.UpsertAttachmentChunk(ctx, sqlcgen.UpsertAttachmentChunkParams{
		ProjectID:      projectID,
		ConversationID: conversationID,
		FileID:         fileID,
		ChunkIndex:     chunkIndex,
		TotalChunks:    totalChunks,
		FileName:       fileName,
		ContentType:    contentType,
		Bytes:          body,
	}); err != nil {
		return fmt.Errorf("upsert attachment chunk: %w", err)
	}
	return nil
}

// CountChunks reports how many distinct chunk indexes have been received so
// far for (projectID, conversationID, fileID).
func (r *AttachmentChunksRepository) CountChunks(ctx context.Context, projectID int64, conversationID, fileID string) (int64, error) {
	n, err := r.queries.CountAttachmentChunks(ctx, sqlcgen.CountAttachmentChunksParams{
		ProjectID:      projectID,
		ConversationID: conversationID,
		FileID:         fileID,
	})
	if err != nil {
		return 0, fmt.Errorf("count attachment chunks: %w", err)
	}
	return n, nil
}

// ListChunksOrdered returns every received chunk for (projectID,
// conversationID, fileID), ordered by chunk_index — ready to concatenate
// into the original byte stream.
func (r *AttachmentChunksRepository) ListChunksOrdered(ctx context.Context, projectID int64, conversationID, fileID string) ([]AttachmentChunk, error) {
	rows, err := r.queries.ListAttachmentChunksOrdered(ctx, sqlcgen.ListAttachmentChunksOrderedParams{
		ProjectID:      projectID,
		ConversationID: conversationID,
		FileID:         fileID,
	})
	if err != nil {
		return nil, fmt.Errorf("list attachment chunks: %w", err)
	}
	chunks := make([]AttachmentChunk, len(rows))
	for i, row := range rows {
		chunks[i] = AttachmentChunk{ChunkIndex: row.ChunkIndex, Bytes: row.Bytes}
	}
	return chunks, nil
}

// DeleteChunks removes every chunk row for (projectID, conversationID,
// fileID) once merged into the final object — best-effort from the
// caller's point of view: a failed delete just leaves harmless,
// self-healing rows behind (a retried "final chunk" request would simply
// re-merge idempotently, overwriting the same destination object).
func (r *AttachmentChunksRepository) DeleteChunks(ctx context.Context, projectID int64, conversationID, fileID string) error {
	if _, err := r.queries.DeleteAttachmentChunks(ctx, sqlcgen.DeleteAttachmentChunksParams{
		ProjectID:      projectID,
		ConversationID: conversationID,
		FileID:         fileID,
	}); err != nil {
		return fmt.Errorf("delete attachment chunks: %w", err)
	}
	return nil
}

// DeleteStaleChunks reclaims chunk rows whose received_at is older than
// olderThan — see the underlying query's own doc comment on why abandoned
// (never-completed) chunked uploads need this in addition to DeleteChunks.
func (r *AttachmentChunksRepository) DeleteStaleChunks(ctx context.Context, olderThan time.Time) (int64, error) {
	n, err := r.queries.DeleteStaleAttachmentChunks(ctx, toTimestamptz(&olderThan))
	if err != nil {
		return 0, fmt.Errorf("delete stale attachment chunks: %w", err)
	}
	return n, nil
}
