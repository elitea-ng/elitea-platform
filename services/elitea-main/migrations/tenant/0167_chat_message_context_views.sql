-- 0167_chat_message_context_views.sql — the view each agent answer was given
-- (ADR-0050 decision 9, issue 1669: the "What the agent saw" inspector).
--
-- chat_message_context_views
--   One row per agent answer (the response message group): what Main sent
--   the answering agent as its view of the conversation on that turn. The
--   admission writes it in the transaction that writes the answer, and a
--   regeneration replaces it with the new run's view.
--
--   reader_participant_id  the participant that answered (the reader).
--   history                full | since_joined | addressed_only | none: the
--                          mode the view was built with, after the
--                          participant's own mode.
--   include_side_talk      party | none.
--   participant_history    the participant's entity_settings.context_history
--                          ("share history from: now"), or NULL.
--   window_tokens          the reader's model window.
--   digest                 the ContextViewV1 digest the worker checked.
--   entries                every visible message the resolver projected, in
--                          transcript order, as [{"message_id": uuid,
--                          "reason": "..."}]; no reason is "in the view". Ids
--                          and reasons only: no message body is copied here.
--
-- Deleted with the answer. No backfill: an answer admitted before this table
-- has no row, and the inspector says so.
--
-- Guarded on chat_message_group for the reason 0134 and 0160 are: several
-- integration fixtures apply the tenant chain to a schema without it.
-- Unqualified names: the runner pins search_path to the tenant schema.
DO $$
BEGIN
    IF to_regclass('chat_message_group') IS NULL THEN
        RETURN;
    END IF;
    CREATE TABLE IF NOT EXISTS chat_message_context_views (
        message_group_uuid    uuid        PRIMARY KEY
                              REFERENCES chat_message_group (uuid) ON DELETE CASCADE,
        reader_participant_id integer     NOT NULL CHECK (reader_participant_id > 0),
        history               text        NOT NULL
                              CHECK (history IN ('full', 'since_joined', 'addressed_only', 'none')),
        include_side_talk     text        NOT NULL CHECK (include_side_talk IN ('party', 'none')),
        participant_history   text        CHECK (participant_history IS NULL OR length(participant_history) <= 32),
        window_tokens         bigint      NOT NULL CHECK (window_tokens >= 0),
        digest                text        NOT NULL CHECK (digest ~ '^[0-9a-f]{64}$'),
        entries               jsonb       NOT NULL DEFAULT '[]'::jsonb
                              CHECK (jsonb_typeof(entries) = 'array'),
        created_at            timestamptz NOT NULL DEFAULT clock_timestamp()
    );
END
$$;
