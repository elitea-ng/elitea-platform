-- name: ListArtifactBuckets :many
SELECT id, project_id, name, display_name, bucket_type, is_pinned, tags,
       retention_days, expires_at, notified_at, created_at, updated_at, deleted_at
FROM elitea_storage.buckets
WHERE project_id = $1 AND deleted_at IS NULL
ORDER BY name;

-- name: GetArtifactBucket :one
SELECT id, project_id, name, display_name, bucket_type, is_pinned, tags,
       retention_days, expires_at, notified_at, created_at, updated_at, deleted_at
FROM elitea_storage.buckets
WHERE project_id = $1 AND name = $2 AND deleted_at IS NULL;

-- name: GetArtifactBucketByID :one
-- ListExpiredArtifactObjects (below) has no request-bound project scope — it
-- scans elitea_storage.objects across every project, which only carries
-- bucket_id, not project_id/name. The retention sweeper (S14) uses this to
-- resolve the (project_id, bucket name) a physical ObjectRef needs before it
-- can delete an expired object's bytes from the ObjectStore backend.
SELECT id, project_id, name, display_name, bucket_type, is_pinned, tags,
       retention_days, expires_at, notified_at, created_at, updated_at, deleted_at
FROM elitea_storage.buckets
WHERE id = $1 AND deleted_at IS NULL;

-- name: CreateArtifactBucket :one
INSERT INTO elitea_storage.buckets (
    project_id, name, display_name, bucket_type, retention_days, expires_at
) VALUES (
    $1, $2, $3, $4, $5, $6
)
RETURNING id, project_id, name, display_name, bucket_type, is_pinned, tags,
    retention_days, expires_at, notified_at, created_at, updated_at, deleted_at;

-- name: UpdateArtifactBucketRetention :one
-- notified_at resets to NULL on every retention change: a bucket already
-- notified once for an earlier expires_at must be eligible for a fresh
-- notification against whatever new expires_at this update just set,
-- otherwise ListArtifactBucketsNeedingExpiryNotice's `notified_at IS NULL`
-- filter would permanently exclude it after the first notice.
UPDATE elitea_storage.buckets
SET retention_days = $2, expires_at = $3, notified_at = NULL, updated_at = now()
WHERE id = $1 AND deleted_at IS NULL
RETURNING id, project_id, name, display_name, bucket_type, is_pinned, tags,
    retention_days, expires_at, notified_at, created_at, updated_at, deleted_at;

-- name: SetArtifactBucketPinned :one
UPDATE elitea_storage.buckets
SET is_pinned = $2, updated_at = now()
WHERE id = $1 AND deleted_at IS NULL
RETURNING id, project_id, name, display_name, bucket_type, is_pinned, tags,
    retention_days, expires_at, notified_at, created_at, updated_at, deleted_at;

-- name: UpdateArtifactBucketTags :one
UPDATE elitea_storage.buckets
SET tags = $2, updated_at = now()
WHERE id = $1 AND deleted_at IS NULL
RETURNING id, project_id, name, display_name, bucket_type, is_pinned, tags,
    retention_days, expires_at, notified_at, created_at, updated_at, deleted_at;

-- name: SoftDeleteArtifactBucket :execrows
UPDATE elitea_storage.buckets
SET deleted_at = now(), updated_at = now()
WHERE id = $1 AND deleted_at IS NULL;

-- name: ListArtifactBucketsNeedingExpiryNotice :many
SELECT id, project_id, name, display_name, bucket_type, is_pinned, tags,
       retention_days, expires_at, notified_at, created_at, updated_at, deleted_at
FROM elitea_storage.buckets
WHERE deleted_at IS NULL
  AND expires_at IS NOT NULL
  AND expires_at <= $1
  AND notified_at IS NULL
ORDER BY expires_at
LIMIT $2;

-- name: MarkArtifactBucketNotified :execrows
UPDATE elitea_storage.buckets
SET notified_at = now()
WHERE id = $1;

-- name: ListArtifactProjectIDsWithBuckets :many
-- Required by S18's per-project byte-usage gauge: the retention sweeper
-- (S14) needs to enumerate every project that owns at least one non-deleted
-- bucket before it can call SumArtifactProjectBytes per project. This
-- service owns no "projects" table of its own (see elitea_storage.buckets'
-- own project_id column, a bare BIGINT with no local FK) — the set of
-- known projects is defined operationally as "has at least one bucket."
SELECT DISTINCT project_id
FROM elitea_storage.buckets
WHERE deleted_at IS NULL
ORDER BY project_id;

-- name: UpsertArtifactObject :one
INSERT INTO elitea_storage.objects (
    bucket_id, key, byte_length, media_type, digest_alg, digest,
    classification, scan_state, expires_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9
)
ON CONFLICT (bucket_id, key) DO UPDATE SET
    byte_length = EXCLUDED.byte_length,
    media_type = EXCLUDED.media_type,
    digest_alg = EXCLUDED.digest_alg,
    digest = EXCLUDED.digest,
    classification = EXCLUDED.classification,
    scan_state = EXCLUDED.scan_state,
    expires_at = EXCLUDED.expires_at,
    updated_at = now()
RETURNING id, bucket_id, key, byte_length, media_type, digest_alg, digest,
    classification, scan_state, expires_at, created_at, updated_at;

-- name: ListArtifactObjects :many
SELECT id, bucket_id, key, byte_length, media_type, digest_alg, digest,
       classification, scan_state, expires_at, created_at, updated_at
FROM elitea_storage.objects
WHERE bucket_id = $1
  AND (sqlc.arg('key_prefix')::text = '' OR key LIKE sqlc.arg('key_prefix')::text || '%')
ORDER BY key;

-- name: DeleteArtifactObjects :execrows
DELETE FROM elitea_storage.objects
WHERE bucket_id = $1 AND key = ANY(sqlc.arg('keys')::text[]);

-- name: SumArtifactBucketBytes :one
SELECT COALESCE(SUM(o.byte_length), 0)::bigint
FROM elitea_storage.objects o
JOIN elitea_storage.buckets b ON b.id = o.bucket_id
WHERE o.bucket_id = $1 AND b.deleted_at IS NULL;

-- name: CountArtifactBucketObjects :one
SELECT COUNT(*)::bigint
FROM elitea_storage.objects o
JOIN elitea_storage.buckets b ON b.id = o.bucket_id
WHERE o.bucket_id = $1 AND b.deleted_at IS NULL;

-- name: SumArtifactProjectBytes :one
SELECT COALESCE(SUM(o.byte_length), 0)::bigint
FROM elitea_storage.objects o
JOIN elitea_storage.buckets b ON b.id = o.bucket_id
WHERE b.project_id = $1 AND b.deleted_at IS NULL;

-- name: GetArtifactProjectStoragePolicy :one
SELECT project_id, max_object_bytes, max_total_bytes, retention_default_days,
       retention_max_days, attachment_bucket, updated_at
FROM elitea_storage.project_storage_policy
WHERE project_id = $1;

-- name: ListExpiredArtifactObjects :many
SELECT id, bucket_id, key, byte_length, media_type, digest_alg, digest,
       classification, scan_state, expires_at, created_at, updated_at
FROM elitea_storage.objects
WHERE expires_at IS NOT NULL AND expires_at < $1
ORDER BY expires_at
LIMIT $2;

-- name: DeleteArtifactObjectRows :execrows
DELETE FROM elitea_storage.objects WHERE id = ANY(sqlc.arg('ids')::bigint[]);

-- name: GetArtifactBucketOwningProjectUserID :one
-- elitea_storage.buckets has no user-identifying column of its own (S8 never
-- captured a creating user) — the retention sweeper (S14) resolves a
-- notification recipient via centry.project.owner_id instead, the only
-- existing project->user mapping that needs no new schema. See
-- docs/plans/storage-migration-plan.md S14's note on this open question.
SELECT owner_id FROM centry.project WHERE id = $1;

-- name: InsertArtifactBucketExpiryNotification :execrows
INSERT INTO centry.notifications (
    uuid, is_seen, project_id, user_id, meta, event_type, created_at, updated_at
) VALUES (
    sqlc.arg('notification_uuid')::text::uuid, FALSE,
    sqlc.arg('project_id')::integer, sqlc.arg('user_id')::integer,
    sqlc.arg('meta')::jsonb, 'artifact_bucket_expiring', now() AT TIME ZONE 'UTC', now() AT TIME ZONE 'UTC'
) ON CONFLICT (uuid) DO NOTHING;
