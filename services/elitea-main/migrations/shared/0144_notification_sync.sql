-- 0144_notification_sync.sql — incremental sync for notifications
-- (ADR-0025 WP6).
--
-- The notification list is user-scoped across projects, so its delta is
-- per user: `changes_since=<cursor>` answers the caller's notifications that
-- changed after the cursor plus tombstones for the ones deleted since.
--
-- WHY A `sync_at` COLUMN AND NOT `updated_at`. On a schema 001_initial.sql
-- built, `updated_at` is NOT NULL DEFAULT now() and the sqlc queries stamp it.
-- On a pylon-built schema it is nullable (internal/db/schema/
-- notifications_baseline.sql was verified against one), and pylon, the
-- scheduler's producers and the moderation and eliteacore writers do not all
-- stamp it. A BEFORE INSERT/UPDATE trigger stamps `sync_at` for every writer,
-- the same rule the tenant chat tables follow (tenant 0144). No response
-- carries the column.
--
-- TOMBSTONES. An AFTER DELETE trigger writes one row per deleted notification
-- to centry.notification_tombstones, keyed by user. elitea-scheduler's
-- syncretention sweeper removes rows past the sync tombstone retention; a
-- cursor older than that answers 410.
--
-- Guarded on centry.notifications: several suites apply the shared corpus to a
-- bare database that has no pylon-era schema (memory admin-rbac-seeding-trap).
--
-- IDEMPOTENT. No BEGIN/COMMIT: the ledgered runner wraps each file.
DO $$
BEGIN
    IF to_regclass('centry.notifications') IS NULL THEN
        RAISE NOTICE 'centry.notifications is absent; notification sync skipped';
        RETURN;
    END IF;

    ALTER TABLE centry.notifications ADD COLUMN IF NOT EXISTS sync_at timestamptz;
    UPDATE centry.notifications
       SET sync_at = COALESCE(updated_at, created_at, clock_timestamp())
     WHERE sync_at IS NULL;
    ALTER TABLE centry.notifications ALTER COLUMN sync_at SET DEFAULT clock_timestamp();
    ALTER TABLE centry.notifications ALTER COLUMN sync_at SET NOT NULL;
    CREATE INDEX IF NOT EXISTS notifications_user_sync_idx
        ON centry.notifications (user_id, sync_at, id);

    CREATE TABLE IF NOT EXISTS centry.notification_tombstones (
        id bigserial PRIMARY KEY,
        notification_id integer NOT NULL,
        notification_uuid uuid,
        user_id integer NOT NULL,
        deleted_at timestamptz NOT NULL DEFAULT clock_timestamp()
    );
    CREATE INDEX IF NOT EXISTS notification_tombstones_user_idx
        ON centry.notification_tombstones (user_id, deleted_at, id);
    CREATE INDEX IF NOT EXISTS notification_tombstones_deleted_at_idx
        ON centry.notification_tombstones (deleted_at);

    CREATE OR REPLACE FUNCTION centry.notification_sync_stamp() RETURNS trigger
    LANGUAGE plpgsql AS $body$
    BEGIN
        NEW.sync_at := clock_timestamp();
        RETURN NEW;
    END
    $body$;

    CREATE OR REPLACE FUNCTION centry.notification_sync_deleted() RETURNS trigger
    LANGUAGE plpgsql AS $body$
    BEGIN
        INSERT INTO centry.notification_tombstones (notification_id, notification_uuid, user_id)
        VALUES (OLD.id, OLD.uuid, OLD.user_id);
        RETURN NULL;
    END
    $body$;

    DROP TRIGGER IF EXISTS notification_sync_stamp ON centry.notifications;
    CREATE TRIGGER notification_sync_stamp BEFORE INSERT OR UPDATE ON centry.notifications
        FOR EACH ROW EXECUTE FUNCTION centry.notification_sync_stamp();
    DROP TRIGGER IF EXISTS notification_sync_deleted ON centry.notifications;
    CREATE TRIGGER notification_sync_deleted AFTER DELETE ON centry.notifications
        FOR EACH ROW EXECUTE FUNCTION centry.notification_sync_deleted();
END
$$;
