package repos

// The Elitea workspace store (ADR-0034 decision 2) over shared/0167 and 0168:
// elitea_runtime.workspace_handoffs (the thread's ownership row, locked by
// every publish), workspace_refs, workspace_segments, and the project's hidden
// `workspace` bucket in elitea_storage.buckets with its transfer grants.
//
// Every statement names (project_id, conversation_uuid); no query reads a
// tenant table, so the store has no tenant dependency and no foreign key.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/workspacestore"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
)

// WorkspaceStoreRepository implements workspacestore.Repository.
type WorkspaceStoreRepository struct {
	pool *pgxpool.Pool
}

var _ workspacestore.Repository = (*WorkspaceStoreRepository)(nil)

func NewWorkspaceStoreRepository(pool *pgxpool.Pool) (*WorkspaceStoreRepository, error) {
	if pool == nil {
		return nil, errors.New("workspace store database is required")
	}
	return &WorkspaceStoreRepository{pool: pool}, nil
}

func (r *WorkspaceStoreRepository) ReadThread(ctx context.Context, thread workspacestore.Thread) (workspacestore.ThreadRow, error) {
	var (
		row     workspacestore.ThreadRow
		state   string
		running *string
	)
	err := r.pool.QueryRow(ctx, `
SELECT state, epoch, owner_user_id, running_execution_id, has_upstream, last_seq
FROM elitea_runtime.workspace_handoffs
WHERE project_id = $1 AND conversation_uuid = $2::uuid`, thread.ProjectID, thread.ConversationUUID).
		Scan(&state, &row.Epoch, &row.OwnerUserID, &running, &row.HasUpstream, &row.LastSeq)
	if errors.Is(err, pgx.ErrNoRows) {
		return workspacestore.ThreadRow{}, workspacestore.ErrNotFound
	}
	if err != nil {
		return workspacestore.ThreadRow{}, fmt.Errorf("workspace store: read thread: %w", err)
	}
	row.State = workspacestore.State(state)
	if running != nil {
		row.RunningExecutionID = *running
	}
	return row, nil
}

// EnsureWorkspaceBucket creates the project's hidden bucket on first use
// (decision 3). It is found by TYPE (the partial unique index on (project_id)
// WHERE bucket_type = 'workspace' allows one), never by name. Its name is
// elitea-workspaces; when a user bucket already holds that name (created
// before the name was reserved), the hidden bucket takes
// elitea-workspaces-<random> and the user bucket keeps working. Two first
// uses at once meet the type index; the loser reads the winner's row.
func (r *WorkspaceStoreRepository) EnsureWorkspaceBucket(ctx context.Context, projectID int64) (workspacestore.WorkspaceBucket, error) {
	if bucket, ok, err := r.WorkspaceBucket(ctx, projectID); err != nil || ok {
		return bucket, err
	}
	for attempt := 0; attempt < 4; attempt++ {
		name := storage.WorkspaceBucketName
		if attempt > 0 {
			var suffix [4]byte
			if _, err := rand.Read(suffix[:]); err != nil {
				return workspacestore.WorkspaceBucket{}, err
			}
			name += "-" + hex.EncodeToString(suffix[:])
		}
		var id int64
		err := r.pool.QueryRow(ctx, `
INSERT INTO elitea_storage.buckets (project_id, name, display_name, bucket_type)
VALUES ($1, $2, 'Workspace store', 'workspace')
RETURNING id`, projectID, name).Scan(&id)
		if err == nil {
			return workspacestore.WorkspaceBucket{ID: id, Name: name}, nil
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
			return workspacestore.WorkspaceBucket{}, fmt.Errorf("workspace store: create workspace bucket: %w", err)
		}
		// Another first use won the type index, or a user bucket holds the
		// name: read by type, else retry with a suffix.
		if bucket, ok, readErr := r.WorkspaceBucket(ctx, projectID); readErr != nil || ok {
			return bucket, readErr
		}
	}
	return workspacestore.WorkspaceBucket{}, fmt.Errorf("workspace store: project %d: no free workspace bucket name", projectID)
}

// WorkspaceBucket reads the project's workspace bucket by type.
func (r *WorkspaceStoreRepository) WorkspaceBucket(ctx context.Context, projectID int64) (workspacestore.WorkspaceBucket, bool, error) {
	var bucket workspacestore.WorkspaceBucket
	err := r.pool.QueryRow(ctx, `
SELECT id, name FROM elitea_storage.buckets
WHERE project_id = $1 AND bucket_type = 'workspace' AND deleted_at IS NULL`, projectID).Scan(&bucket.ID, &bucket.Name)
	if errors.Is(err, pgx.ErrNoRows) {
		return workspacestore.WorkspaceBucket{}, false, nil
	}
	if err != nil {
		return workspacestore.WorkspaceBucket{}, false, fmt.Errorf("workspace store: read workspace bucket: %w", err)
	}
	return bucket, true, nil
}

const segmentColumns = `id, seq, epoch, kind, sha256, byte_length, prerequisites, refs, writer,
       object_key, physical_bucket, created_at, superseded_at`

func scanSegment(row pgx.Row) (workspacestore.SegmentRow, error) {
	var (
		segment             workspacestore.SegmentRow
		prerequisites, refs []byte
		writer              string
	)
	if err := row.Scan(&segment.ID, &segment.Seq, &segment.Epoch, &segment.Kind, &segment.SHA256, &segment.Bytes,
		&prerequisites, &refs, &writer, &segment.ObjectKey, &segment.PhysicalBucket, &segment.CreatedAt,
		&segment.SupersededAt); err != nil {
		return workspacestore.SegmentRow{}, err
	}
	segment.Writer = workspacestore.Writer(writer)
	_ = json.Unmarshal(prerequisites, &segment.Prerequisites)
	_ = json.Unmarshal(refs, &segment.Refs)
	return segment, nil
}

func (r *WorkspaceStoreRepository) oneSegment(ctx context.Context, query string, args ...any) (workspacestore.SegmentRow, bool, error) {
	segment, err := scanSegment(r.pool.QueryRow(ctx, query, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return workspacestore.SegmentRow{}, false, nil
	}
	if err != nil {
		return workspacestore.SegmentRow{}, false, fmt.Errorf("workspace store: read segment: %w", err)
	}
	return segment, true, nil
}

func (r *WorkspaceStoreRepository) LiveSegment(ctx context.Context, thread workspacestore.Thread, sha string) (workspacestore.SegmentRow, bool, error) {
	return r.oneSegment(ctx, `SELECT `+segmentColumns+`
FROM elitea_runtime.workspace_segments
WHERE conversation_uuid = $2::uuid AND project_id = $1 AND sha256 = $3 AND superseded_at IS NULL
ORDER BY seq DESC LIMIT 1`, thread.ProjectID, thread.ConversationUUID, sha)
}

func (r *WorkspaceStoreRepository) StoredSegment(ctx context.Context, thread workspacestore.Thread, sha string) (workspacestore.SegmentRow, bool, error) {
	return r.oneSegment(ctx, `SELECT `+segmentColumns+`
FROM elitea_runtime.workspace_segments
WHERE conversation_uuid = $2::uuid AND project_id = $1 AND sha256 = $3 AND object_deleted_at IS NULL
ORDER BY seq DESC LIMIT 1`, thread.ProjectID, thread.ConversationUUID, sha)
}

func (r *WorkspaceStoreRepository) ProjectWorkspaceBytes(ctx context.Context, projectID int64) (int64, error) {
	var total int64
	if err := r.pool.QueryRow(ctx, `
SELECT COALESCE(sum(byte_length), 0)::bigint FROM elitea_runtime.workspace_segments
WHERE project_id = $1 AND superseded_at IS NULL`, projectID).Scan(&total); err != nil {
		return 0, fmt.Errorf("workspace store: project bytes: %w", err)
	}
	return total, nil
}

func (r *WorkspaceStoreRepository) ThreadWorkspaceBytes(ctx context.Context, thread workspacestore.Thread) (int64, error) {
	var total int64
	if err := r.pool.QueryRow(ctx, `
SELECT COALESCE(sum(byte_length), 0)::bigint FROM elitea_runtime.workspace_segments
WHERE conversation_uuid = $2::uuid AND project_id = $1 AND superseded_at IS NULL`,
		thread.ProjectID, thread.ConversationUUID).Scan(&total); err != nil {
		return 0, fmt.Errorf("workspace store: thread bytes: %w", err)
	}
	return total, nil
}

func (r *WorkspaceStoreRepository) CreateGrant(ctx context.Context, grant workspacestore.GrantRecord) error {
	if _, err := r.pool.Exec(ctx, `
INSERT INTO elitea_storage.transfer_grants (
    id, project_id, bucket_id, key, method, content_type, max_bytes, digest_alg, digest, upload_id, expires_at
) VALUES ($1::uuid, $2, $3, $4, 'PUT', $5, $6, 'sha256', $7, $8, $9)`,
		grant.ID, grant.ProjectID, grant.BucketID, grant.Key, grant.ContentType, grant.MaxBytes,
		grant.Digest, grant.UploadID, grant.ExpiresAt); err != nil {
		return fmt.Errorf("workspace store: create grant: %w", err)
	}
	return nil
}

const grantColumns = `g.id::text, g.project_id, g.bucket_id, g.key, g.max_bytes, g.digest, g.upload_id,
       g.expires_at, g.content_type, g.consumed_at`

func scanGrant(row pgx.Row) (workspacestore.GrantRow, bool, error) {
	var grant workspacestore.GrantRow
	err := row.Scan(&grant.ID, &grant.ProjectID, &grant.BucketID, &grant.Key, &grant.MaxBytes, &grant.Digest,
		&grant.UploadID, &grant.ExpiresAt, &grant.ContentType, &grant.ConsumedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return workspacestore.GrantRow{}, false, nil
	}
	if err != nil {
		return workspacestore.GrantRow{}, false, fmt.Errorf("workspace store: read grant: %w", err)
	}
	return grant, true, nil
}

func (r *WorkspaceStoreRepository) OpenGrant(ctx context.Context, projectID int64, key string) (workspacestore.GrantRow, bool, error) {
	return scanGrant(r.pool.QueryRow(ctx, `
SELECT `+grantColumns+`
FROM elitea_storage.transfer_grants AS g
JOIN elitea_storage.buckets AS b ON b.id = g.bucket_id AND b.bucket_type = 'workspace'
WHERE g.project_id = $1 AND g.key = $2 AND g.consumed_at IS NULL AND g.expires_at > clock_timestamp()
ORDER BY g.created_at DESC LIMIT 1`, projectID, key))
}

func (r *WorkspaceStoreRepository) ReadGrant(ctx context.Context, projectID int64, grantID string) (workspacestore.GrantRow, bool, error) {
	return scanGrant(r.pool.QueryRow(ctx, `
SELECT `+grantColumns+`
FROM elitea_storage.transfer_grants AS g
JOIN elitea_storage.buckets AS b ON b.id = g.bucket_id AND b.bucket_type = 'workspace'
WHERE g.project_id = $1 AND g.id = $2::uuid`, projectID, grantID))
}

// Publish is the store's one short transaction (decisions 2 and 15).
func (r *WorkspaceStoreRepository) Publish(ctx context.Context, record workspacestore.PublishRecord) (workspacestore.PublishResult, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return workspacestore.PublishResult{}, fmt.Errorf("workspace store: begin publish: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// 1. Lock the thread's handoff row, then check the state and the epoch.
	var (
		state       string
		epoch       int64
		owner       int64
		running     *string
		response    *string
		hasUpstream bool
		lastSeq     int64
	)
	err = tx.QueryRow(ctx, `
SELECT state, epoch, owner_user_id, running_execution_id, running_response_message_id::text, has_upstream, last_seq
FROM elitea_runtime.workspace_handoffs
WHERE project_id = $1 AND conversation_uuid = $2::uuid
FOR UPDATE`, record.Thread.ProjectID, record.Thread.ConversationUUID).
		Scan(&state, &epoch, &owner, &running, &response, &hasUpstream, &lastSeq)
	if errors.Is(err, pgx.ErrNoRows) {
		return workspacestore.PublishResult{}, workspacestore.ErrNotFound
	}
	if err != nil {
		return workspacestore.PublishResult{}, fmt.Errorf("workspace store: lock thread: %w", err)
	}
	if record.Caller.Writer == workspacestore.WriterDesktop && owner != record.Caller.UserID {
		return workspacestore.PublishResult{}, workspacestore.ErrNotFound
	}
	if workspacestore.State(state) != workspacestore.WriterState(record.Caller.Writer) || epoch != record.Epoch ||
		(record.Caller.Writer == workspacestore.WriterCloudExecutor && (running == nil || *running != record.Caller.ExecutionID)) {
		return workspacestore.PublishResult{}, workspacestore.ErrStaleEpoch
	}
	allowed := map[string]bool{}
	for _, name := range workspacestore.AllowedRefs(record.Caller.Writer, workspacestore.State(state), hasUpstream) {
		allowed[name] = true
	}
	for _, ref := range record.Refs {
		if !allowed[ref.Name] {
			return workspacestore.PublishResult{}, workspacestore.ErrRefNotAllowed
		}
	}

	current := map[string]string{}
	rows, err := tx.Query(ctx, `
SELECT name, sha FROM elitea_runtime.workspace_refs
WHERE project_id = $1 AND conversation_uuid = $2::uuid`, record.Thread.ProjectID, record.Thread.ConversationUUID)
	if err != nil {
		return workspacestore.PublishResult{}, fmt.Errorf("workspace store: read refs: %w", err)
	}
	for rows.Next() {
		var name, sha string
		if err := rows.Scan(&name, &sha); err != nil {
			rows.Close()
			return workspacestore.PublishResult{}, fmt.Errorf("workspace store: read refs: %w", err)
		}
		current[name] = sha
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return workspacestore.PublishResult{}, fmt.Errorf("workspace store: read refs: %w", err)
	}

	// A retried publish that was already applied answers the original
	// result: the same segment is live in this epoch and every ref already
	// holds the value the publish moves it to.
	var replaySeq int64
	err = tx.QueryRow(ctx, `
SELECT seq FROM elitea_runtime.workspace_segments
WHERE conversation_uuid = $2::uuid AND project_id = $1 AND sha256 = $3 AND epoch = $4 AND superseded_at IS NULL
ORDER BY seq DESC LIMIT 1`, record.Thread.ProjectID, record.Thread.ConversationUUID, record.SHA256, record.Epoch).Scan(&replaySeq)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return workspacestore.PublishResult{}, fmt.Errorf("workspace store: read replay: %w", err)
	}
	if err == nil {
		applied := true
		for _, ref := range record.Refs {
			if current[ref.Name] != ref.New {
				applied = false
				break
			}
		}
		if applied {
			return workspacestore.PublishResult{Seq: replaySeq, Refs: current, Superseded: []int64{}, Replayed: true}, nil
		}
	}

	// 2. Every expected old value, all or nothing.
	conflict := false
	for _, ref := range record.Refs {
		stored, present := current[ref.Name]
		if (ref.Old == nil) != !present || (ref.Old != nil && *ref.Old != stored) {
			conflict = true
		}
	}
	if conflict {
		values := map[string]*string{}
		for _, ref := range record.Refs {
			if stored, present := current[ref.Name]; present {
				value := stored
				values[ref.Name] = &value
			} else {
				values[ref.Name] = nil
			}
		}
		return workspacestore.PublishResult{}, &workspacestore.RefConflictError{Current: values}
	}

	// 3. The segment, with the next per-thread seq. A COMPACT covers every
	// live ref, so it supersedes every earlier live segment of the thread.
	seq := lastSeq + 1
	superseded := []int64{}
	rows, err = tx.Query(ctx, `
UPDATE elitea_runtime.workspace_segments SET superseded_at = clock_timestamp()
WHERE conversation_uuid = $2::uuid AND project_id = $1 AND superseded_at IS NULL
RETURNING seq`, record.Thread.ProjectID, record.Thread.ConversationUUID)
	if err != nil {
		return workspacestore.PublishResult{}, fmt.Errorf("workspace store: supersede: %w", err)
	}
	for rows.Next() {
		var value int64
		if err := rows.Scan(&value); err != nil {
			rows.Close()
			return workspacestore.PublishResult{}, fmt.Errorf("workspace store: supersede: %w", err)
		}
		superseded = append(superseded, value)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return workspacestore.PublishResult{}, fmt.Errorf("workspace store: supersede: %w", err)
	}
	for _, ref := range record.Refs {
		current[ref.Name] = ref.New
	}
	refsJSON, _ := json.Marshal(current)
	prerequisites, _ := json.Marshal(record.Prerequisites)
	if _, err := tx.Exec(ctx, `
INSERT INTO elitea_runtime.workspace_segments (
    project_id, conversation_uuid, seq, epoch, kind, sha256, byte_length, prerequisites, refs, writer,
    object_key, physical_bucket
) VALUES ($1, $2::uuid, $3, $4, $5, $6, $7, $8::jsonb, $9::jsonb, $10, $11, $12)`,
		record.Thread.ProjectID, record.Thread.ConversationUUID, seq, record.Epoch, record.Kind, record.SHA256,
		record.Bytes, string(prerequisites), string(refsJSON), string(record.Caller.Writer),
		record.ObjectKey, record.PhysicalBucket); err != nil {
		return workspacestore.PublishResult{}, fmt.Errorf("workspace store: insert segment: %w", err)
	}

	// 4. Move the refs, and the handoff row's cache of them.
	for _, ref := range record.Refs {
		if _, err := tx.Exec(ctx, `
INSERT INTO elitea_runtime.workspace_refs (project_id, conversation_uuid, name, sha, seq)
VALUES ($1, $2::uuid, $3, $4, $5)
ON CONFLICT (project_id, conversation_uuid, name)
DO UPDATE SET sha = EXCLUDED.sha, seq = EXCLUDED.seq, updated_at = clock_timestamp()`,
			record.Thread.ProjectID, record.Thread.ConversationUUID, ref.Name, ref.New, seq); err != nil {
			return workspacestore.PublishResult{}, fmt.Errorf("workspace store: move ref: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `
UPDATE elitea_runtime.workspace_handoffs
SET last_seq = $3,
    base_sha = COALESCE($4, base_sha),
    upstream_sha = COALESCE($5, upstream_sha),
    handoff_sha = COALESCE($6, handoff_sha),
    cloud_sha = COALESCE($7, cloud_sha),
    updated_at = clock_timestamp()
WHERE project_id = $1 AND conversation_uuid = $2::uuid`,
		record.Thread.ProjectID, record.Thread.ConversationUUID, seq,
		refValue(record.Refs, workspacestore.RefBase), refValue(record.Refs, workspacestore.RefUpstream),
		refValue(record.Refs, workspacestore.RefHandoff1), refValue(record.Refs, workspacestore.RefCloud)); err != nil {
		return workspacestore.PublishResult{}, fmt.Errorf("workspace store: advance thread: %w", err)
	}
	// A cloud turn's commit is stamped on its answer (decision 8): the
	// desktop imports cloud turns by executed_by, workspace_epoch and
	// workspace_commit.
	if record.Caller.Writer == workspacestore.WriterCloudExecutor && response != nil {
		if err := StampCloudTurnAnswer(ctx, tx, record.Thread.ProjectID, *response, record.Epoch, current[workspacestore.RefCloud]); err != nil {
			return workspacestore.PublishResult{}, err
		}
	}
	// The grant that carried the bytes is spent.
	if _, err := tx.Exec(ctx, `
UPDATE elitea_storage.transfer_grants AS g SET consumed_at = clock_timestamp()
FROM elitea_storage.buckets AS b
WHERE b.id = g.bucket_id AND b.bucket_type = 'workspace'
  AND g.project_id = $1 AND g.key = $2 AND g.consumed_at IS NULL`,
		record.Thread.ProjectID, record.ObjectKey); err != nil {
		return workspacestore.PublishResult{}, fmt.Errorf("workspace store: consume grant: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return workspacestore.PublishResult{}, fmt.Errorf("workspace store: commit publish: %w", err)
	}
	return workspacestore.PublishResult{Seq: seq, Refs: current, Superseded: superseded}, nil
}

func refValue(refs []workspacestore.RefUpdate, name string) *string {
	for _, ref := range refs {
		if ref.Name == name {
			value := ref.New
			return &value
		}
	}
	return nil
}

func (r *WorkspaceStoreRepository) FetchRows(ctx context.Context, thread workspacestore.Thread, sinceSeq int64) (workspacestore.FetchRows, error) {
	result := workspacestore.FetchRows{Refs: map[string]string{}}
	if err := r.pool.QueryRow(ctx, `
SELECT last_seq FROM elitea_runtime.workspace_handoffs WHERE project_id = $1 AND conversation_uuid = $2::uuid`,
		thread.ProjectID, thread.ConversationUUID).Scan(&result.HeadSeq); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return workspacestore.FetchRows{}, workspacestore.ErrNotFound
		}
		return workspacestore.FetchRows{}, fmt.Errorf("workspace store: read head: %w", err)
	}
	rows, err := r.pool.Query(ctx, `
SELECT name, sha FROM elitea_runtime.workspace_refs WHERE project_id = $1 AND conversation_uuid = $2::uuid`,
		thread.ProjectID, thread.ConversationUUID)
	if err != nil {
		return workspacestore.FetchRows{}, fmt.Errorf("workspace store: read refs: %w", err)
	}
	for rows.Next() {
		var name, sha string
		if err := rows.Scan(&name, &sha); err != nil {
			rows.Close()
			return workspacestore.FetchRows{}, err
		}
		result.Refs[name] = sha
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return workspacestore.FetchRows{}, err
	}
	segment, ok, err := r.oneSegment(ctx, `SELECT `+segmentColumns+`
FROM elitea_runtime.workspace_segments
WHERE conversation_uuid = $2::uuid AND project_id = $1 AND kind = 'COMPACT' AND superseded_at IS NULL AND seq >= $3
ORDER BY seq DESC LIMIT 1`, thread.ProjectID, thread.ConversationUUID, sinceSeq)
	if err != nil {
		return workspacestore.FetchRows{}, err
	}
	if ok {
		result.Newest = &segment
	}
	return result, nil
}

// ClaimSupersededObjects stamps object_deleted_at on a batch of superseded
// segments FIRST (FOR UPDATE SKIP LOCKED, so replicas share the work), and the
// caller deletes the objects after the transaction. A row whose object a live
// segment of the same thread still uses is Shared: the row is retired, the
// object kept.
func (r *WorkspaceStoreRepository) ClaimSupersededObjects(ctx context.Context, before time.Time, limit int) ([]workspacestore.ObjectSweep, error) {
	rows, err := r.pool.Query(ctx, `
WITH claimed AS (
    SELECT id FROM elitea_runtime.workspace_segments
    WHERE superseded_at IS NOT NULL AND superseded_at < $1 AND object_deleted_at IS NULL
    ORDER BY superseded_at
    LIMIT $2
    FOR UPDATE SKIP LOCKED
)
UPDATE elitea_runtime.workspace_segments AS s SET object_deleted_at = clock_timestamp()
FROM claimed
WHERE s.id = claimed.id
RETURNING s.id, s.project_id, s.object_key, s.physical_bucket,
    EXISTS (SELECT 1 FROM elitea_runtime.workspace_segments AS live
            WHERE live.project_id = s.project_id AND live.conversation_uuid = s.conversation_uuid
              AND live.object_key = s.object_key AND live.id <> s.id AND live.object_deleted_at IS NULL)`,
		before, limit)
	if err != nil {
		return nil, fmt.Errorf("workspace store: claim superseded: %w", err)
	}
	defer rows.Close()
	out := []workspacestore.ObjectSweep{}
	for rows.Next() {
		var sweep workspacestore.ObjectSweep
		if err := rows.Scan(&sweep.ID, &sweep.ProjectID, &sweep.ObjectKey, &sweep.PhysicalBucket, &sweep.Shared); err != nil {
			return nil, err
		}
		out = append(out, sweep)
	}
	return out, rows.Err()
}

func (r *WorkspaceStoreRepository) ReleaseObjectSweep(ctx context.Context, ids []int64) error {
	if _, err := r.pool.Exec(ctx, `
UPDATE elitea_runtime.workspace_segments SET object_deleted_at = NULL WHERE id = ANY($1::bigint[])`, ids); err != nil {
		return fmt.Errorf("workspace store: release sweep: %w", err)
	}
	return nil
}

func (r *WorkspaceStoreRepository) KeyHasLiveObject(ctx context.Context, projectID int64, key string) (bool, error) {
	var live bool
	if err := r.pool.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM elitea_runtime.workspace_segments
               WHERE project_id = $1 AND object_key = $2 AND object_deleted_at IS NULL)`, projectID, key).Scan(&live); err != nil {
		return false, fmt.Errorf("workspace store: key liveness: %w", err)
	}
	return live, nil
}

func (r *WorkspaceStoreRepository) RetireThreadSegments(ctx context.Context, thread workspacestore.Thread) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("workspace store: begin retire: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
UPDATE elitea_runtime.workspace_segments
SET superseded_at = COALESCE(superseded_at, clock_timestamp()),
    object_deleted_at = COALESCE(object_deleted_at, clock_timestamp())
WHERE conversation_uuid = $2::uuid AND project_id = $1`, thread.ProjectID, thread.ConversationUUID); err != nil {
		return fmt.Errorf("workspace store: retire segments: %w", err)
	}
	if _, err := tx.Exec(ctx, `
DELETE FROM elitea_runtime.workspace_refs WHERE project_id = $1 AND conversation_uuid = $2::uuid`,
		thread.ProjectID, thread.ConversationUUID); err != nil {
		return fmt.Errorf("workspace store: retire refs: %w", err)
	}
	return tx.Commit(ctx)
}

func (r *WorkspaceStoreRepository) WorkspaceProjects(ctx context.Context, after int64, limit int) ([]int64, error) {
	rows, err := r.pool.Query(ctx, `
SELECT project_id FROM elitea_storage.buckets
WHERE bucket_type = 'workspace' AND deleted_at IS NULL AND project_id > $1
ORDER BY project_id LIMIT $2`, after, limit)
	if err != nil {
		return nil, fmt.Errorf("workspace store: workspace projects: %w", err)
	}
	defer rows.Close()
	out := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// StampCloudTurnAnswer merges the cloud turn markers into the answer group's
// meta (decision 8): executed_by = cloud_executor, workspace_epoch and, when
// the turn published, workspace_commit. The turn's finalisation keeps them
// (it removes only its own keys), and a regenerate strips the two workspace
// keys. A tenant schema without the chat tables takes no stamp.
func StampCloudTurnAnswer(ctx context.Context, tx pgx.Tx, projectID int64, responseMessageID string, epoch int64, commit string) error {
	schema, err := tenantSchema(strconv.FormatInt(projectID, 10))
	if err != nil {
		return nil
	}
	var present bool
	if err := tx.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, schema+".chat_message_group").Scan(&present); err != nil {
		return fmt.Errorf("workspace store: tenant schema: %w", err)
	}
	if !present {
		return nil
	}
	stamp := map[string]any{"executed_by": "cloud_executor", "workspace_epoch": epoch}
	if commit != "" {
		stamp["workspace_commit"] = commit
	}
	encoded, _ := json.Marshal(stamp)
	if _, err := tx.Exec(ctx, fmt.Sprintf(`UPDATE %s.chat_message_group SET meta = meta || $2::jsonb WHERE uuid = $1::uuid`, schema),
		responseMessageID, string(encoded)); err != nil {
		return fmt.Errorf("workspace store: stamp cloud turn answer: %w", err)
	}
	return nil
}
