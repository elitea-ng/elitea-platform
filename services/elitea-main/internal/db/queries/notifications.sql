-- name: CountCurrentNotifications :one
SELECT COUNT(*)::bigint
FROM centry.notifications
WHERE user_id = sqlc.arg(user_id)
  AND (NOT sqlc.arg(only_new)::boolean OR NOT is_seen)
  AND (sqlc.arg(event_type)::text = '' OR event_type = sqlc.arg(event_type))
  AND NOT EXISTS (
      SELECT 1
      FROM unnest(sqlc.arg(search_words)::text[]) AS search_word(value)
      WHERE COALESCE(meta ->> 'message', '')
            NOT ILIKE ('%' || search_word.value || '%') ESCAPE '\'
  );

-- name: ListCurrentNotifications :many
SELECT id,
       uuid::text AS uuid,
       is_seen,
       project_id,
       user_id,
       meta,
       event_type,
       created_at,
       updated_at
FROM centry.notifications
WHERE user_id = sqlc.arg(user_id)
  AND (NOT sqlc.arg(only_new)::boolean OR NOT is_seen)
  AND (sqlc.arg(event_type)::text = '' OR event_type = sqlc.arg(event_type))
  AND NOT EXISTS (
      SELECT 1
      FROM unnest(sqlc.arg(search_words)::text[]) AS search_word(value)
      WHERE COALESCE(meta ->> 'message', '')
            NOT ILIKE ('%' || search_word.value || '%') ESCAPE '\'
  )
ORDER BY
  CASE WHEN sqlc.arg(sort_by)::text = 'id' AND sqlc.arg(sort_order)::text = 'asc' THEN id END ASC,
  CASE WHEN sqlc.arg(sort_by)::text = 'id' AND sqlc.arg(sort_order)::text = 'desc' THEN id END DESC,
  CASE WHEN sqlc.arg(sort_by)::text = 'uuid' AND sqlc.arg(sort_order)::text = 'asc' THEN uuid END ASC,
  CASE WHEN sqlc.arg(sort_by)::text = 'uuid' AND sqlc.arg(sort_order)::text = 'desc' THEN uuid END DESC,
  CASE WHEN sqlc.arg(sort_by)::text = 'is_seen' AND sqlc.arg(sort_order)::text = 'asc' THEN is_seen END ASC,
  CASE WHEN sqlc.arg(sort_by)::text = 'is_seen' AND sqlc.arg(sort_order)::text = 'desc' THEN is_seen END DESC,
  CASE WHEN sqlc.arg(sort_by)::text = 'project_id' AND sqlc.arg(sort_order)::text = 'asc' THEN project_id END ASC,
  CASE WHEN sqlc.arg(sort_by)::text = 'project_id' AND sqlc.arg(sort_order)::text = 'desc' THEN project_id END DESC,
  CASE WHEN sqlc.arg(sort_by)::text = 'user_id' AND sqlc.arg(sort_order)::text = 'asc' THEN user_id END ASC,
  CASE WHEN sqlc.arg(sort_by)::text = 'user_id' AND sqlc.arg(sort_order)::text = 'desc' THEN user_id END DESC,
  CASE WHEN sqlc.arg(sort_by)::text = 'meta' AND sqlc.arg(sort_order)::text = 'asc' THEN meta END ASC,
  CASE WHEN sqlc.arg(sort_by)::text = 'meta' AND sqlc.arg(sort_order)::text = 'desc' THEN meta END DESC,
  CASE WHEN sqlc.arg(sort_by)::text = 'event_type' AND sqlc.arg(sort_order)::text = 'asc' THEN event_type END ASC,
  CASE WHEN sqlc.arg(sort_by)::text = 'event_type' AND sqlc.arg(sort_order)::text = 'desc' THEN event_type END DESC,
  CASE WHEN sqlc.arg(sort_by)::text = 'created_at' AND sqlc.arg(sort_order)::text = 'asc' THEN created_at END ASC,
  CASE WHEN sqlc.arg(sort_by)::text = 'created_at' AND sqlc.arg(sort_order)::text = 'desc' THEN created_at END DESC,
  CASE WHEN sqlc.arg(sort_by)::text = 'updated_at' AND sqlc.arg(sort_order)::text = 'asc' THEN updated_at END ASC,
  CASE WHEN sqlc.arg(sort_by)::text = 'updated_at' AND sqlc.arg(sort_order)::text = 'desc' THEN updated_at END DESC,
  id DESC
LIMIT sqlc.arg(page_limit)
OFFSET sqlc.arg(page_offset);

-- name: GetCurrentNotification :one
SELECT id,
       uuid::text AS uuid,
       is_seen,
       project_id,
       user_id,
       meta,
       event_type,
       created_at,
       updated_at
FROM centry.notifications
WHERE id = sqlc.arg(notification_id)
  AND user_id = sqlc.arg(user_id);

-- name: MarkCurrentNotificationSeen :one
WITH changed AS (
    UPDATE centry.notifications AS notification
    SET is_seen = TRUE,
        updated_at = NOW()
    WHERE notification.id = sqlc.arg(notification_id)
      AND notification.user_id = sqlc.arg(user_id)
      AND NOT notification.is_seen
    RETURNING notification.id,
              notification.uuid::text AS uuid,
              notification.is_seen,
              notification.project_id,
              notification.user_id,
              notification.meta,
              notification.event_type,
              notification.created_at,
              notification.updated_at
)
SELECT * FROM changed
UNION ALL
SELECT id,
       uuid::text AS uuid,
       is_seen,
       project_id,
       user_id,
       meta,
       event_type,
       created_at,
       updated_at
FROM centry.notifications
WHERE id = sqlc.arg(notification_id)
  AND user_id = sqlc.arg(user_id)
  AND NOT EXISTS (SELECT 1 FROM changed)
LIMIT 1;

-- name: DeleteCurrentNotification :execrows
DELETE FROM centry.notifications
WHERE id = sqlc.arg(notification_id)
  AND user_id = sqlc.arg(user_id);

-- name: BulkSetCurrentNotificationsSeen :execrows
UPDATE centry.notifications
SET is_seen = sqlc.arg(is_seen),
    updated_at = NOW()
WHERE user_id = sqlc.arg(user_id)
  AND is_seen <> sqlc.arg(is_seen)
  AND (
      sqlc.arg(all_notifications)::boolean
      OR id = ANY(sqlc.arg(notification_ids)::integer[])
  );

-- name: BulkDeleteCurrentNotifications :execrows
DELETE FROM centry.notifications
WHERE user_id = sqlc.arg(user_id)
  AND id = ANY(sqlc.arg(notification_ids)::integer[]);

-- name: CurrentNotificationSyncClock :one
-- The database clock a `changes_since` page is computed against (ADR-0025
-- WP6): the stamps are written by triggers on this clock, so the settle
-- bound and the expiry check must read it too, not the application's.
SELECT clock_timestamp()::timestamptz AS now;

-- name: ListCurrentNotificationChanges :many
-- The caller's notifications whose sync_at stamp (shared 0144) is after the
-- cursor position, oldest change first.
SELECT id,
       uuid::text AS uuid,
       is_seen,
       project_id,
       user_id,
       meta,
       event_type,
       created_at,
       updated_at,
       sync_at
FROM centry.notifications
WHERE user_id = sqlc.arg(user_id)
  AND (sync_at, id) > (sqlc.arg(after_at)::timestamptz, sqlc.arg(after_id)::bigint)
ORDER BY sync_at, id
LIMIT sqlc.arg(page_limit);

-- name: ListCurrentNotificationTombstones :many
-- The caller's notification deletions after the cursor position.
SELECT id,
       notification_id,
       COALESCE(notification_uuid::text, ''::text)::text AS notification_uuid,
       deleted_at
FROM centry.notification_tombstones
WHERE user_id = sqlc.arg(user_id)
  AND (deleted_at, id) > (sqlc.arg(after_at)::timestamptz, sqlc.arg(after_id)::bigint)
ORDER BY deleted_at, id
LIMIT sqlc.arg(page_limit);
