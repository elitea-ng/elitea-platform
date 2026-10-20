-- 0153_chat_conversation_reads.sql — per-user read markers and turn-only
-- sync bumps (client contract 1.12, conversation activity status).
--
-- WHAT IT ADDS
--
--   chat_conversation_reads
--     conversation_id       integer   REFERENCES chat_conversations(id) ON DELETE CASCADE
--     user_id               bigint    -- the reader's user id
--     read_at               timestamp -- the UTC wall clock, as chat_message_group
--     last_read_message_id  integer   -- the message group the reader read up to, or NULL
--     PRIMARY KEY (conversation_id, user_id)
--
-- One row per reader of a conversation. markConversationRead writes it. A
-- list row is `unread` for its caller when an answer (a message group that
-- no user participant wrote and that does not stream) changed after the
-- caller's read_at. The marker is personal state: it never stamps sync_at,
-- because a read by one member is not a change for the others.
--
-- `user_id` is not a foreign key, as archived_by in 0151 and user_id in
-- chat_message_feedback (0135) are not: user identity is central.
--
-- THE BASELINE. A reader without a row reads everything before
-- chat_conversation_read_baseline(), the wall clock at which this migration
-- ran in the tenant. Without it every existing conversation would be unread
-- for every member on the first list after the upgrade. The value is one row
-- of chat_conversation_read_settings, written once (a re-run keeps the first
-- baseline). It is data, not part of the function body, so two databases
-- built from the same migrations have the same schema.
--
--   chat_conversation_read_settings
--     singleton  boolean   PRIMARY KEY, always TRUE (one row)
--     baseline   timestamp -- the UTC wall clock of the first run
--
-- TURN-ONLY CONVERSATION BUMPS
--
-- 0144 bumps a conversation's sync_at (throttled to one per second) on every
-- write to one of its message groups. A streamed token rewrites the text of
-- the running answer, which bumps the answer group, which bumped the
-- conversation once a second for the whole turn: the conversation row came
-- back in every `changes_since` delta while a turn ran, with nothing new in
-- it. The list row now carries the turn's activity, so the row must come
-- back on each TRANSITION and only then:
--
--   * a group INSERT (a turn starts, or a settled message is added);
--   * an UPDATE that changes `is_streaming` (the turn finishes, fails, is
--     cancelled, pauses, or resumes);
--   * an UPDATE that adds or removes a pause key (hitl_interrupt,
--     hitl_interrupts, authorization_requests, pipeline_static_v1,
--     node_recovery_required_v1, output_limit_reached).
--
-- These bumps keep 0144's throttle (one per second per conversation). The
-- throttle is sound for the same reason as in 0144: it is shorter than the
-- read side's settle window, so a transition that the throttle skips is
-- still behind the client's next cursor, and the row comes back in its
-- current state. An unthrottled bump would also hold the conversation row
-- lock for every concurrent writer of the same conversation. An UPDATE of a
-- group that streams before and after it (a token, a trace step, a
-- provisional text reset) does not bump the conversation at all: the
-- message delta still sees the group, through the group's own sync_at. Any
-- other UPDATE (an edit of a settled message, a canvas edit) keeps 0144's
-- bump.
--
-- INDEXES. The list reads each row's newest message group, and the folder
-- sidebar counts the conversations whose newest group is paused, so this
-- adds (conversation_id, created_at DESC, id DESC) and a partial index of
-- the paused groups. The pause predicate is the editor Test run's PAUSED
-- rule (internal/infra/db/repos/editor_test_runs.go); the Go side keeps the
-- same text in internal/infra/db/chatactivity.
--
-- GUARDS. As 0144 and 0151: every relation and column is checked, because
-- integration fixtures apply the tenant chain to minimal hand-built tables.
-- Function bodies are schema-qualified, because a trigger runs under the
-- WRITER's search_path. IDEMPOTENT. No BEGIN/COMMIT: the ledgered runner
-- wraps each file.
DO $migration$
DECLARE
    tenant text := current_schema();
BEGIN
    IF tenant IS NULL THEN
        RAISE EXCEPTION 'chat read migration needs a tenant search_path';
    END IF;

    CREATE TABLE IF NOT EXISTS chat_conversation_read_settings (
        singleton boolean PRIMARY KEY DEFAULT TRUE CHECK (singleton),
        baseline timestamp NOT NULL
    );
    INSERT INTO chat_conversation_read_settings (singleton, baseline)
    VALUES (TRUE, clock_timestamp() AT TIME ZONE 'UTC')
    ON CONFLICT (singleton) DO NOTHING;

    -- No row (a hand-built fixture): nothing is read before the first marker.
    EXECUTE format($fn$
        CREATE OR REPLACE FUNCTION %1$I.chat_conversation_read_baseline() RETURNS timestamp
        LANGUAGE sql STABLE AS $body$
            SELECT COALESCE((SELECT baseline FROM %1$I.chat_conversation_read_settings), '-infinity'::timestamp)
        $body$ $fn$, tenant);

    IF to_regclass('chat_conversations') IS NOT NULL THEN
        CREATE TABLE IF NOT EXISTS chat_conversation_reads (
            conversation_id integer NOT NULL REFERENCES chat_conversations(id) ON DELETE CASCADE,
            user_id bigint NOT NULL,
            read_at timestamp NOT NULL DEFAULT (now() AT TIME ZONE 'UTC'),
            last_read_message_id integer,
            PRIMARY KEY (conversation_id, user_id)
        );
    END IF;

    IF to_regclass('chat_message_group') IS NOT NULL
       AND (SELECT count(*) FROM pg_attribute WHERE attrelid = to_regclass('chat_message_group')
              AND NOT attisdropped
              AND attname = ANY (ARRAY['conversation_id', 'created_at', 'is_streaming', 'meta'])) = 4 THEN
        CREATE INDEX IF NOT EXISTS chat_message_group_conversation_newest_idx
            ON chat_message_group (conversation_id, created_at DESC, id DESC);
        CREATE INDEX IF NOT EXISTS chat_message_group_paused_idx
            ON chat_message_group (conversation_id)
            WHERE NOT is_streaming AND (meta ? 'hitl_interrupt' OR meta ? 'hitl_interrupts'
                OR meta ? 'authorization_requests' OR meta->'output_limit_reached' = 'true'::jsonb
                OR meta ? 'pipeline_static_v1' OR meta ? 'node_recovery_required_v1');
    END IF;

    -- Whether a group's meta holds a pause key.
    EXECUTE format($fn$
        CREATE OR REPLACE FUNCTION %1$I.chat_group_meta_paused(meta jsonb) RETURNS boolean
        LANGUAGE sql IMMUTABLE AS $body$
            SELECT COALESCE(meta ? 'hitl_interrupt' OR meta ? 'hitl_interrupts'
                OR meta ? 'authorization_requests' OR meta->'output_limit_reached' = 'true'::jsonb
                OR meta ? 'pipeline_static_v1' OR meta ? 'node_recovery_required_v1', false)
        $body$ $fn$, tenant);

    -- 0144's chat_message_group trigger function, with the turn-only bump.
    -- The trigger (0144) keeps pointing at this function.
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
            ELSIF TG_OP = 'INSERT' THEN
                PERFORM %1$I.chat_sync_bump_conversation(NEW.conversation_id);
            ELSIF OLD.is_streaming IS DISTINCT FROM NEW.is_streaming
                  OR %1$I.chat_group_meta_paused(OLD.meta) IS DISTINCT FROM %1$I.chat_group_meta_paused(NEW.meta) THEN
                PERFORM %1$I.chat_sync_bump_conversation(NEW.conversation_id);
            ELSIF OLD.conversation_id IS DISTINCT FROM NEW.conversation_id THEN
                PERFORM %1$I.chat_sync_bump_conversation(OLD.conversation_id);
                PERFORM %1$I.chat_sync_bump_conversation(NEW.conversation_id);
            ELSIF OLD.is_streaming AND NEW.is_streaming THEN
                -- A write inside a running turn: no conversation bump.
                NULL;
            ELSE
                PERFORM %1$I.chat_sync_bump_conversation(NEW.conversation_id);
            END IF;
            RETURN NULL;
        END
        $body$ $fn$, tenant);
END
$migration$;
