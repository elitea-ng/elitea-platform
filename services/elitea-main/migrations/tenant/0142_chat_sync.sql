-- 0142_chat_sync.sql — incremental sync for conversations and messages
-- (ADR-0025 WP6).
--
-- A native client keeps conversations and their messages offline and asks
-- `changes_since=<cursor>` for what moved. That needs three things this schema
-- did not have: a change stamp every writer maintains, a record of deletions,
-- and a record of a conversation leaving a caller's view.
--
-- WHY A NEW `sync_at` COLUMN AND NOT `updated_at`
--
-- `updated_at` on chat_conversations and chat_message_group is nullable and is
-- stamped by some writers only. Its wire meaning is fixed: a message's
-- `updated_at` is null when the message was never rewritten, and the
-- conversation list answers COALESCE(updated_at, created_at) as "last
-- modified". Stamping it on every write would change both. `sync_at` is
-- internal: no response carries it, and the cursor is opaque.
--
-- WHY TRIGGERS AND NOT CODE AT EACH WRITER
--
-- The chat tables are written by sqlc queries, raw SQL in half a dozen repos,
-- the pipeline-trigger and MCP paths, and the Python worker. A stamp that one
-- writer forgets is a change a client never sees. Triggers are the one place
-- every writer passes through.
--
--   * BEFORE INSERT/UPDATE on chat_conversations and chat_message_group sets
--     sync_at = clock_timestamp().
--   * Child -> parent bumps, throttled to one per second per parent row:
--     a message group insert/update/delete bumps its conversation, and a
--     chat_message_items / chat_messages_text write bumps its group, as does
--     a write to any other payload the message delta projects:
--     chat_messages_canvas and chat_messages_attachment (keyed by the item
--     id) and chat_canvas_versions (keyed by canvas_item_id). The canvas
--     editor's save writes ONLY the last two, so without them a synced
--     client would never see a canvas edit. The
--     throttle is sound only while it is shorter than the read side's settle
--     window (internal/application/changesync, 5 s): a client whose cursor
--     stops at S - settle is behind the last bump, so the parent comes back
--     on the next delta and is read in its current state.
--   * AFTER DELETE writes a tombstone. A conversation delete that runs
--     `SET LOCAL elitea.sync_cascade = 'conversation'` (ConversationsRepo.Delete)
--     suppresses the per-message tombstones and bumps: the conversation
--     tombstone already means "drop every message". Other deleters that do
--     not set it write message tombstones as well, which is only more rows.
--
-- LOST ACCESS
--
-- A conversation can leave a caller's list without being deleted. Those
-- moments are recorded in the same tombstone table under three kinds, and the
-- READ side decides per caller whether one becomes an `access_lost` tombstone
-- (internal/infra/db/repos/conversation_changes.go):
--
--   access_private      is_private went false -> true. Everyone could see it
--                       before; a caller who cannot see it now lost it.
--   access_filter       meta.is_hidden, meta.single_participant or source
--                       changed. A caller who can still see the conversation
--                       but whose list filter no longer matches lost it from
--                       that list.
--   access_participant  a user participant was removed (user_id = that user).
--                       Only that user is told, and only if they can no
--                       longer see it.
--   access_orphaned     a PRIVATE conversation lost its last user
--                       participant. A project admin lists a private
--                       conversation only through some user participant, so
--                       admins who can no longer see it are told; nobody else
--                       could see it before.
--
-- A caller's project ROLE changing (admin granted or withdrawn) changes what
-- the list shows without touching these tables. That is not a tombstone: the
-- conversation cursor is bound to the role, and a cursor issued under the
-- other one answers 410 and the client resyncs.
--
-- `access_participant` rows are also what a PRIVATE conversation's delete
-- tombstone is shown against: the participant mappings are deleted before the
-- conversation (ConversationsRepo.Delete), so at read time they are the only
-- record of who could see it. A private conversation's deletion is therefore
-- announced to its author, its former user participants and project admins,
-- and not to everyone in the project.
--
-- Project-membership loss is NOT a conversation tombstone: the client gets 403
-- on the project and drops that project's cache.
--
-- RETENTION. Rows older than the sync tombstone retention (97 days by default,
-- never less) are removed by elitea-scheduler's syncretention sweeper; a cursor
-- older than that answers 410 and the client resyncs from scratch.
--
-- GUARDS. Every table is checked with to_regclass, and the backfill reads only
-- the columns that exist, for the reason 0134 gives: integration fixtures
-- apply the tenant chain to minimal hand-built tables. Functions are created
-- in this tenant schema with schema-qualified bodies, because a trigger runs
-- under the WRITER's search_path, not the migration's.
--
-- IDEMPOTENT. No BEGIN/COMMIT: the ledgered runner wraps each file.
DO $migration$
DECLARE
    tenant text := current_schema();
    table_name text;
    has_updated boolean;
    has_created boolean;
BEGIN
    IF tenant IS NULL THEN
        RAISE EXCEPTION 'chat sync migration needs a tenant search_path';
    END IF;

    -- The tombstone table exists in every tenant, chat tables or not, so the
    -- read side and the sweeper can rely on it.
    CREATE TABLE IF NOT EXISTS chat_sync_tombstones (
        id bigserial PRIMARY KEY,
        kind text NOT NULL CHECK (kind IN (
            'conversation', 'message_group',
            'access_private', 'access_filter', 'access_participant',
            'access_orphaned')),
        -- The conversation id for every kind but message_group, whose
        -- entity_id is the group id.
        entity_id integer NOT NULL,
        entity_uuid uuid,
        -- The owning conversation of a message_group tombstone.
        conversation_id integer,
        -- access_participant: the removed user's id, as the participant row
        -- spells it (`entity_meta->>'id'`, text).
        user_id text,
        -- conversation: the deleted row's privacy and author, which decide
        -- who is told.
        is_private boolean,
        author_id integer,
        deleted_at timestamptz NOT NULL DEFAULT clock_timestamp()
    );
    CREATE INDEX IF NOT EXISTS chat_sync_tombstones_deleted_at_idx
        ON chat_sync_tombstones (deleted_at, id);
    CREATE INDEX IF NOT EXISTS chat_sync_tombstones_message_idx
        ON chat_sync_tombstones (conversation_id, deleted_at, id)
        WHERE kind = 'message_group';
    CREATE INDEX IF NOT EXISTS chat_sync_tombstones_participant_idx
        ON chat_sync_tombstones (entity_id, user_id)
        WHERE kind = 'access_participant';

    -- The stamp column, backfilled before NOT NULL.
    FOREACH table_name IN ARRAY ARRAY['chat_conversations', 'chat_message_group'] LOOP
        CONTINUE WHEN to_regclass(table_name) IS NULL;
        EXECUTE format('ALTER TABLE %I ADD COLUMN IF NOT EXISTS sync_at timestamptz', table_name);
        SELECT EXISTS (SELECT 1 FROM pg_attribute WHERE attrelid = to_regclass(table_name)
                         AND attname = 'updated_at' AND NOT attisdropped),
               EXISTS (SELECT 1 FROM pg_attribute WHERE attrelid = to_regclass(table_name)
                         AND attname = 'created_at' AND NOT attisdropped)
          INTO has_updated, has_created;
        IF has_updated AND has_created THEN
            EXECUTE format('UPDATE %I SET sync_at = COALESCE(updated_at, created_at) WHERE sync_at IS NULL', table_name);
        ELSIF has_created THEN
            EXECUTE format('UPDATE %I SET sync_at = created_at WHERE sync_at IS NULL', table_name);
        END IF;
        EXECUTE format('UPDATE %I SET sync_at = clock_timestamp() WHERE sync_at IS NULL', table_name);
        EXECUTE format('ALTER TABLE %I ALTER COLUMN sync_at SET DEFAULT clock_timestamp()', table_name);
        EXECUTE format('ALTER TABLE %I ALTER COLUMN sync_at SET NOT NULL', table_name);
        EXECUTE format('CREATE INDEX IF NOT EXISTS %I ON %I (sync_at, id)', table_name || '_sync_idx', table_name);
    END LOOP;

    -- Stamp on every insert and update. An explicit sync_at in an INSERT is
    -- overwritten too: the stamp is the server's clock, never a writer's.
    EXECUTE format($fn$
        CREATE OR REPLACE FUNCTION %1$I.chat_sync_stamp() RETURNS trigger
        LANGUAGE plpgsql AS $body$
        BEGIN
            NEW.sync_at := clock_timestamp();
            RETURN NEW;
        END
        $body$ $fn$, tenant);

    -- Throttled bump of one conversation.
    EXECUTE format($fn$
        CREATE OR REPLACE FUNCTION %1$I.chat_sync_bump_conversation(target integer) RETURNS void
        LANGUAGE plpgsql AS $body$
        BEGIN
            IF target IS NULL OR current_setting('elitea.sync_cascade', true) = 'conversation' THEN
                RETURN;
            END IF;
            UPDATE %1$I.chat_conversations SET sync_at = clock_timestamp()
             WHERE id = target AND sync_at < clock_timestamp() - interval '1 second';
        END
        $body$ $fn$, tenant);

    -- Throttled bump of one message group.
    EXECUTE format($fn$
        CREATE OR REPLACE FUNCTION %1$I.chat_sync_bump_group(target integer) RETURNS void
        LANGUAGE plpgsql AS $body$
        BEGIN
            IF target IS NULL OR current_setting('elitea.sync_cascade', true) = 'conversation' THEN
                RETURN;
            END IF;
            UPDATE %1$I.chat_message_group SET sync_at = clock_timestamp()
             WHERE id = target AND sync_at < clock_timestamp() - interval '1 second';
        END
        $body$ $fn$, tenant);

    -- chat_conversations: deletion tombstone and lost-access markers.
    EXECUTE format($fn$
        CREATE OR REPLACE FUNCTION %1$I.chat_sync_conversation_changed() RETURNS trigger
        LANGUAGE plpgsql AS $body$
        BEGIN
            IF TG_OP = 'DELETE' THEN
                INSERT INTO %1$I.chat_sync_tombstones (kind, entity_id, entity_uuid, is_private, author_id)
                VALUES ('conversation', OLD.id, OLD.uuid, OLD.is_private, OLD.author_id);
                RETURN OLD;
            END IF;
            IF OLD.is_private = false AND NEW.is_private = true THEN
                INSERT INTO %1$I.chat_sync_tombstones (kind, entity_id, entity_uuid)
                VALUES ('access_private', NEW.id, NEW.uuid);
            END IF;
            IF OLD.source IS DISTINCT FROM NEW.source
               OR OLD.meta->'is_hidden' IS DISTINCT FROM NEW.meta->'is_hidden'
               OR OLD.meta->'single_participant' IS DISTINCT FROM NEW.meta->'single_participant' THEN
                INSERT INTO %1$I.chat_sync_tombstones (kind, entity_id, entity_uuid)
                VALUES ('access_filter', NEW.id, NEW.uuid);
            END IF;
            RETURN NEW;
        END
        $body$ $fn$, tenant);

    -- chat_message_group: tombstone on delete, parent bump on every write.
    EXECUTE format($fn$
        CREATE OR REPLACE FUNCTION %1$I.chat_sync_group_changed() RETURNS trigger
        LANGUAGE plpgsql AS $body$
        BEGIN
            IF current_setting('elitea.sync_cascade', true) = 'conversation' THEN
                RETURN NULL;
            END IF;
            IF TG_OP = 'DELETE' THEN
                INSERT INTO %1$I.chat_sync_tombstones (kind, entity_id, entity_uuid, conversation_id)
                VALUES ('message_group', OLD.id, OLD.uuid, OLD.conversation_id);
                PERFORM %1$I.chat_sync_bump_conversation(OLD.conversation_id);
            ELSE
                PERFORM %1$I.chat_sync_bump_conversation(NEW.conversation_id);
            END IF;
            RETURN NULL;
        END
        $body$ $fn$, tenant);

    -- chat_message_items: a content change bumps the owning group.
    EXECUTE format($fn$
        CREATE OR REPLACE FUNCTION %1$I.chat_sync_item_changed() RETURNS trigger
        LANGUAGE plpgsql AS $body$
        BEGIN
            IF TG_OP = 'DELETE' THEN
                PERFORM %1$I.chat_sync_bump_group(OLD.message_group_id);
            ELSE
                PERFORM %1$I.chat_sync_bump_group(NEW.message_group_id);
            END IF;
            RETURN NULL;
        END
        $body$ $fn$, tenant);

    -- chat_messages_text: the text payload hangs off an item by id.
    EXECUTE format($fn$
        CREATE OR REPLACE FUNCTION %1$I.chat_sync_text_changed() RETURNS trigger
        LANGUAGE plpgsql AS $body$
        BEGIN
            IF current_setting('elitea.sync_cascade', true) = 'conversation' THEN
                RETURN NULL;
            END IF;
            PERFORM %1$I.chat_sync_bump_group(
                (SELECT message_group_id FROM %1$I.chat_message_items WHERE id = NEW.id));
            RETURN NULL;
        END
        $body$ $fn$, tenant);

    -- chat_messages_canvas / chat_messages_attachment: payloads keyed by the
    -- item id, like chat_messages_text. On a cascade from the item's own
    -- delete the item row is already gone; the item trigger bumped the group.
    EXECUTE format($fn$
        CREATE OR REPLACE FUNCTION %1$I.chat_sync_payload_changed() RETURNS trigger
        LANGUAGE plpgsql AS $body$
        BEGIN
            IF current_setting('elitea.sync_cascade', true) = 'conversation' THEN
                RETURN NULL;
            END IF;
            PERFORM %1$I.chat_sync_bump_group(
                (SELECT message_group_id FROM %1$I.chat_message_items
                  WHERE id = CASE WHEN TG_OP = 'DELETE' THEN OLD.id ELSE NEW.id END));
            RETURN NULL;
        END
        $body$ $fn$, tenant);

    -- chat_canvas_versions: a canvas edit is a new version row, keyed by the
    -- canvas item.
    EXECUTE format($fn$
        CREATE OR REPLACE FUNCTION %1$I.chat_sync_canvas_version_changed() RETURNS trigger
        LANGUAGE plpgsql AS $body$
        BEGIN
            IF current_setting('elitea.sync_cascade', true) = 'conversation' THEN
                RETURN NULL;
            END IF;
            PERFORM %1$I.chat_sync_bump_group(
                (SELECT message_group_id FROM %1$I.chat_message_items
                  WHERE id = CASE WHEN TG_OP = 'DELETE' THEN OLD.canvas_item_id ELSE NEW.canvas_item_id END));
            RETURN NULL;
        END
        $body$ $fn$, tenant);

    -- chat_participant_mapping: gaining a participant bumps the conversation
    -- (a new participant must see a private conversation it now belongs to);
    -- losing a user participant records who lost it.
    EXECUTE format($fn$
        CREATE OR REPLACE FUNCTION %1$I.chat_sync_mapping_changed() RETURNS trigger
        LANGUAGE plpgsql AS $body$
        DECLARE
            removed_user text;
        BEGIN
            IF TG_OP = 'DELETE' THEN
                SELECT participant.entity_meta->>'id' INTO removed_user
                  FROM %1$I.chat_participants AS participant
                 WHERE participant.id = OLD.participant_id AND participant.entity_name = 'user';
                IF removed_user IS NOT NULL THEN
                    INSERT INTO %1$I.chat_sync_tombstones (kind, entity_id, user_id)
                    VALUES ('access_participant', OLD.conversation_id, removed_user);
                    -- The last user participant of a private conversation:
                    -- admins lose it too. Not during a conversation delete,
                    -- whose own tombstone already reaches admins.
                    IF current_setting('elitea.sync_cascade', true) IS DISTINCT FROM 'conversation' THEN
                        INSERT INTO %1$I.chat_sync_tombstones (kind, entity_id, entity_uuid)
                        SELECT 'access_orphaned', c.id, c.uuid
                          FROM %1$I.chat_conversations c
                         WHERE c.id = OLD.conversation_id AND c.is_private
                           AND NOT EXISTS (
                               SELECT 1 FROM %1$I.chat_participant_mapping m
                                 JOIN %1$I.chat_participants p ON p.id = m.participant_id
                                WHERE m.conversation_id = c.id AND p.entity_name = 'user');
                    END IF;
                END IF;
                PERFORM %1$I.chat_sync_bump_conversation(OLD.conversation_id);
            ELSE
                PERFORM %1$I.chat_sync_bump_conversation(NEW.conversation_id);
            END IF;
            RETURN NULL;
        END
        $body$ $fn$, tenant);

    -- A trigger is attached only where the columns its function reads exist:
    -- a fixture's minimal table would otherwise fail every write to it with
    -- "record has no field".
    IF to_regclass('chat_conversations') IS NOT NULL
       AND (SELECT count(*) FROM pg_attribute WHERE attrelid = to_regclass('chat_conversations')
              AND NOT attisdropped
              AND attname = ANY (ARRAY['uuid', 'is_private', 'author_id', 'meta', 'source'])) = 5 THEN
        DROP TRIGGER IF EXISTS chat_sync_stamp ON chat_conversations;
        CREATE TRIGGER chat_sync_stamp BEFORE INSERT OR UPDATE ON chat_conversations
            FOR EACH ROW EXECUTE FUNCTION chat_sync_stamp();
        DROP TRIGGER IF EXISTS chat_sync_changed ON chat_conversations;
        CREATE TRIGGER chat_sync_changed AFTER UPDATE OR DELETE ON chat_conversations
            FOR EACH ROW EXECUTE FUNCTION chat_sync_conversation_changed();
    END IF;
    IF to_regclass('chat_message_group') IS NOT NULL
       AND to_regclass('chat_conversations') IS NOT NULL
       AND (SELECT count(*) FROM pg_attribute WHERE attrelid = to_regclass('chat_message_group')
              AND NOT attisdropped AND attname = ANY (ARRAY['uuid', 'conversation_id'])) = 2 THEN
        DROP TRIGGER IF EXISTS chat_sync_stamp ON chat_message_group;
        CREATE TRIGGER chat_sync_stamp BEFORE INSERT OR UPDATE ON chat_message_group
            FOR EACH ROW EXECUTE FUNCTION chat_sync_stamp();
        DROP TRIGGER IF EXISTS chat_sync_changed ON chat_message_group;
        CREATE TRIGGER chat_sync_changed AFTER INSERT OR UPDATE OR DELETE ON chat_message_group
            FOR EACH ROW EXECUTE FUNCTION chat_sync_group_changed();
        CREATE INDEX IF NOT EXISTS chat_message_group_conversation_sync_idx
            ON chat_message_group (conversation_id, sync_at, id);
    END IF;
    IF to_regclass('chat_message_items') IS NOT NULL
       AND to_regclass('chat_message_group') IS NOT NULL
       AND (SELECT count(*) FROM pg_attribute WHERE attrelid = to_regclass('chat_message_items')
              AND NOT attisdropped AND attname = 'message_group_id') = 1 THEN
        DROP TRIGGER IF EXISTS chat_sync_changed ON chat_message_items;
        CREATE TRIGGER chat_sync_changed AFTER INSERT OR UPDATE OR DELETE ON chat_message_items
            FOR EACH ROW EXECUTE FUNCTION chat_sync_item_changed();
    END IF;
    IF to_regclass('chat_messages_text') IS NOT NULL
       AND to_regclass('chat_message_items') IS NOT NULL
       AND to_regclass('chat_message_group') IS NOT NULL THEN
        DROP TRIGGER IF EXISTS chat_sync_changed ON chat_messages_text;
        CREATE TRIGGER chat_sync_changed AFTER INSERT OR UPDATE ON chat_messages_text
            FOR EACH ROW EXECUTE FUNCTION chat_sync_text_changed();
    END IF;
    FOREACH table_name IN ARRAY ARRAY['chat_messages_canvas', 'chat_messages_attachment'] LOOP
        CONTINUE WHEN to_regclass(table_name) IS NULL
                   OR to_regclass('chat_message_items') IS NULL
                   OR to_regclass('chat_message_group') IS NULL;
        EXECUTE format('DROP TRIGGER IF EXISTS chat_sync_changed ON %I', table_name);
        EXECUTE format('CREATE TRIGGER chat_sync_changed AFTER INSERT OR UPDATE OR DELETE ON %I
            FOR EACH ROW EXECUTE FUNCTION chat_sync_payload_changed()', table_name);
    END LOOP;
    IF to_regclass('chat_canvas_versions') IS NOT NULL
       AND to_regclass('chat_message_items') IS NOT NULL
       AND to_regclass('chat_message_group') IS NOT NULL
       AND (SELECT count(*) FROM pg_attribute WHERE attrelid = to_regclass('chat_canvas_versions')
              AND NOT attisdropped AND attname = 'canvas_item_id') = 1 THEN
        DROP TRIGGER IF EXISTS chat_sync_changed ON chat_canvas_versions;
        CREATE TRIGGER chat_sync_changed AFTER INSERT OR UPDATE OR DELETE ON chat_canvas_versions
            FOR EACH ROW EXECUTE FUNCTION chat_sync_canvas_version_changed();
    END IF;
    IF to_regclass('chat_participant_mapping') IS NOT NULL
       AND to_regclass('chat_participants') IS NOT NULL
       AND to_regclass('chat_conversations') IS NOT NULL
       AND (SELECT count(*) FROM pg_attribute WHERE attrelid = to_regclass('chat_participants')
              AND NOT attisdropped AND attname = ANY (ARRAY['entity_meta', 'entity_name'])) = 2
       AND (SELECT count(*) FROM pg_attribute WHERE attrelid = to_regclass('chat_conversations')
              AND NOT attisdropped AND attname = ANY (ARRAY['uuid', 'is_private'])) = 2 THEN
        DROP TRIGGER IF EXISTS chat_sync_changed ON chat_participant_mapping;
        CREATE TRIGGER chat_sync_changed AFTER INSERT OR DELETE ON chat_participant_mapping
            FOR EACH ROW EXECUTE FUNCTION chat_sync_mapping_changed();
    END IF;
END
$migration$;
