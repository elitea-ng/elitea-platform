-- name: CreateArtifactTransferGrant :one
-- id::text/RETURNING id::text: transfer_grants.id is a native uuid column;
-- casting to text on both sides keeps the Go-side type a plain string,
-- matching this codebase's established convention (e.g.
-- InsertArtifactBucketExpiryNotification, S14) rather than pgtype.UUID.
-- upload_id (S16) is set only when CreateTransferGrant started a native
-- multipart upload; a normal single-shot grant passes it as NULL.
INSERT INTO elitea_storage.transfer_grants (
    id, project_id, bucket_id, key, method, content_type, max_bytes,
    digest_alg, digest, upload_id, expires_at
) VALUES (
    sqlc.arg('id')::text::uuid, sqlc.arg('project_id')::bigint, sqlc.arg('bucket_id')::bigint,
    sqlc.arg('key')::text, sqlc.arg('method')::text, sqlc.arg('content_type')::text,
    sqlc.arg('max_bytes')::bigint, sqlc.narg('digest_alg')::text, sqlc.narg('digest')::bytea,
    sqlc.narg('upload_id')::text, sqlc.arg('expires_at')::timestamptz
)
RETURNING id::text AS id, project_id, bucket_id, key, method, content_type, max_bytes,
    digest_alg, digest, upload_id, expires_at, consumed_at, created_at;

-- name: GetArtifactTransferGrant :one
-- Scoped by project_id, not just id: grant IDs are unguessable UUIDs, but
-- every other artifact route requires a matching {projectID}, and this one
-- should not be the exception that lets a caller commit a grant belonging
-- to a project they only guessed the ID for.
SELECT id::text AS id, project_id, bucket_id, key, method, content_type, max_bytes,
    digest_alg, digest, upload_id, expires_at, consumed_at, created_at
FROM elitea_storage.transfer_grants
WHERE id = sqlc.arg('id')::text::uuid AND project_id = sqlc.arg('project_id')::bigint
  -- A workspace-store grant (ADR-0034) is not an artifact grant: the artifact
  -- routes answer it as absent.
  AND NOT EXISTS (SELECT 1 FROM elitea_storage.buckets b
                  WHERE b.id = transfer_grants.bucket_id AND b.bucket_type = 'workspace');

-- name: GetArtifactTransferGrantByID :one
-- Unscoped by project_id, unlike GetArtifactTransferGrant above — used only
-- by S16's multipart continuation endpoints (part presign, complete,
-- abort), which must distinguish "grant does not exist" (404) from "grant
-- exists but belongs to a different project" (403 AccessDenied): the
-- plan's own S16 acceptance criterion ("a part call with another project's
-- grant returns 403") requires exactly that distinction, which a
-- project-scoped WHERE clause can't make — a wrong project and a
-- nonexistent id both return zero rows. See handler.go's
-- requireOwnedMultipartGrant for where the 403 decision actually happens.
SELECT id::text AS id, project_id, bucket_id, key, method, content_type, max_bytes,
    digest_alg, digest, upload_id, expires_at, consumed_at, created_at
FROM elitea_storage.transfer_grants
WHERE id = sqlc.arg('id')::text::uuid
  AND NOT EXISTS (SELECT 1 FROM elitea_storage.buckets b
                  WHERE b.id = transfer_grants.bucket_id AND b.bucket_type = 'workspace');

-- name: MarkArtifactTransferGrantConsumed :execrows
-- consumed_at IS NULL in the WHERE clause, not just the SET, is the actual
-- single-use enforcement: 0 rows affected means either the grant does not
-- exist or was already consumed. The caller (commitTransferGrant) has
-- already fetched the row by this point, so 0 rows here unambiguously means
-- "already consumed" — see repository.go's MarkTransferGrantConsumed.
-- S16 also uses this to mark an aborted multipart grant terminal — see
-- AbortMultipartUpload.
UPDATE elitea_storage.transfer_grants
SET consumed_at = now()
WHERE id = sqlc.arg('id')::text::uuid AND consumed_at IS NULL;

-- name: ClaimArtifactTransferGrantForCommit :one
-- The same single-use stamp as MarkArtifactTransferGrantConsumed, taken
-- BEFORE a single-shot commit touches the grant's final key, and returning
-- the stamp so a failed commit can hand the grant back
-- (ReleaseArtifactTransferGrantClaim). No row means already consumed (or
-- claimed by a commit in flight).
UPDATE elitea_storage.transfer_grants
SET consumed_at = clock_timestamp()
WHERE id = sqlc.arg('id')::text::uuid AND consumed_at IS NULL
RETURNING consumed_at;

-- name: ReleaseArtifactTransferGrantClaim :execrows
-- Undoes ClaimArtifactTransferGrantForCommit after a commit that did not
-- complete, so the caller can retry on the same grant. The stamp match makes
-- it a no-op on anything but this commit's own claim.
UPDATE elitea_storage.transfer_grants
SET consumed_at = NULL
WHERE id = sqlc.arg('id')::text::uuid
  AND consumed_at = sqlc.arg('claimed_at')::timestamptz;

-- name: ClaimExpiredArtifactTransferGrants :many
-- Claims a bounded batch of expired, unconsumed grants for the retention
-- sweeper, and returns what it needs to reclaim the bytes.
--
-- WHY THIS EXISTS. CreateArtifactTransferGrant hands out a presigned PUT and
-- writes only this row. The elitea_storage.objects row appears at commit and
-- nowhere else. A caller who uploads to the signed URL and never commits
-- therefore leaves bytes with no metadata row: SumArtifactProjectBytes cannot
-- count them against max_total_bytes, and ListExpiredArtifactObjects cannot
-- see them, because both read only the objects table. Nothing time-driven
-- reclaimed them.
--
-- WHY IT CLAIMS INSTEAD OF SELECTING. Setting consumed_at in the same
-- statement that selects the row is what makes the sweep safe against a
-- commit in flight. MarkArtifactTransferGrantConsumed already carries
-- `AND consumed_at IS NULL`, so a commit that races a claimed grant loses the
-- mark and answers 409. A plain SELECT followed by a delete has the opposite
-- outcome. The commit writes a metadata row. The sweeper then deletes the
-- bytes underneath it. The project is charged for an object that no longer
-- exists, and no later sweep can heal that.
--
-- FOR UPDATE SKIP LOCKED lets two replicas sweep at the same time without
-- either waiting on the other. The row itself is kept, not deleted, so a late
-- commit still sees the 409 rather than a 404.
UPDATE elitea_storage.transfer_grants
SET consumed_at = now()
WHERE id IN (
    SELECT id FROM elitea_storage.transfer_grants
    WHERE consumed_at IS NULL AND expires_at < sqlc.arg('older_than')::timestamptz
      -- Workspace-store grants are reclaimed by the workspace sweeper, which
      -- knows which objects a published segment still needs (ADR-0034).
      AND NOT EXISTS (SELECT 1 FROM elitea_storage.buckets b
                      WHERE b.id = transfer_grants.bucket_id AND b.bucket_type = 'workspace')
    ORDER BY expires_at
    LIMIT sqlc.arg('row_limit')::int
    FOR UPDATE SKIP LOCKED
)
RETURNING id::text AS id, project_id, bucket_id, key, method, upload_id, expires_at;

-- name: ListExpiredArtifactGrantStaging :many
-- The presigned single-shot grants whose staging object may still exist
-- after they stopped being usable (api/v2/artifacts grants.go,
-- grantStagingKey). Such a grant's URL addresses key = id || '.upload', not
-- the object a commit serves, and a commit deletes it — but the URL stays
-- valid until expires_at, so a PUT after the commit recreates it, with no
-- metadata row to count or expire it. Once the URL has expired nothing can
-- write there again, so the sweeper deletes the object and then clears the
-- marker (ClearArtifactGrantStaging).
--
-- The key predicate is EXACT on purpose: other writers record consumed
-- grants whose key differs from their id (toolkit discovery inventory), and
-- those keys name live objects.
SELECT id::text AS id, project_id, bucket_id, key
FROM elitea_storage.transfer_grants
WHERE consumed_at IS NOT NULL
  AND key = id::text || '.upload'
  AND upload_id IS NULL
  AND expires_at < sqlc.arg('older_than')::timestamptz
  AND NOT EXISTS (SELECT 1 FROM elitea_storage.buckets b
                  WHERE b.id = transfer_grants.bucket_id AND b.bucket_type = 'workspace')
ORDER BY expires_at
LIMIT sqlc.arg('row_limit')::int;

-- name: ClearArtifactGrantStaging :exec
-- Run after the staging object is gone. The row is kept (a late commit still
-- answers 409), with key reset to the grant id, which drops it from
-- ListExpiredArtifactGrantStaging.
UPDATE elitea_storage.transfer_grants
SET key = id::text
WHERE id = sqlc.arg('id')::text::uuid AND key = sqlc.arg('key')::text;
