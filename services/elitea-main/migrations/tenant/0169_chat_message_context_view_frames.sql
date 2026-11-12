-- 0169_chat_message_context_view_frames.sql — store each answer's view as a
-- delta against the reader's previous view (issue 1893).
--
-- 0167 stores every projected message id of every answer's view: one row per
-- answer holding the whole conversation, O(n) per answer and O(n^2) per
-- conversation. From here an answer's entries live in an immutable chain of
-- frames, each frame the delta of one view against the reader's previous one,
-- so a turn costs what changed since that reader's last answer.
--
-- chat_message_context_view_frames
--   One immutable row per recorded view. Never updated: a regeneration writes
--   a new frame and repoints its answer, so a frame other frames were built
--   on keeps the content they were built on.
--
--   id                    identity; a frame's base is always an older frame
--                         (base_frame_id < id), so a chain cannot loop.
--   conversation_id       the conversation; deleting it deletes its frames.
--   reader_participant_id the reader whose views the chain holds.
--   base_frame_id         the frame this one is a delta against; NULL for a
--                         keyframe, which holds the full entry list. The base
--                         is the reader's newest frame in the conversation
--                         (the view of its last recorded answer, a deleted
--                         answer's included: its frame stays) other than the
--                         frame the answer being written points to, so a
--                         regeneration builds on its neighbour, not on the
--                         run it replaces.
--   depth                 0 for a keyframe, else the base's depth + 1. The
--                         writer starts a new keyframe at depth 64, or when
--                         the delta is not smaller than the full list, so a
--                         read applies at most 63 deltas.
--   body                  a keyframe's entries, [{"message_id": uuid,
--                         "reason": "..."}] as 0167 stores them; a delta's
--                         edit script against its base, in base order:
--                         {"keep": n} copies n base entries, {"drop": n}
--                         skips n, {"add": [entries]} emits entries. The
--                         script covers the whole base. Ids and reasons only.
--   entry_count           the length of the entry list the frame decodes to,
--                         checked on read.
--
-- chat_message_context_views.frame_id
--   The answer's frame. NULL on a row written before this migration: such a
--   row holds its full list in `entries` and reads as before. A writer that
--   finds no frame for the reader moves the newest such row of the reader into
--   a keyframe (frame_id set, entries emptied), so it serves as a chain base.
--   When frame_id is set, entries is '[]'.
--
-- An answer deleted since keeps its frame (later frames may be built on it);
-- a frame no answer and no frame names is deleted when the regeneration that
-- left it runs. No backfill.
--
-- Guarded on chat_message_context_views for the reason 0167 is guarded on
-- chat_message_group. Unqualified names: the runner pins search_path to the
-- tenant schema.
DO $$
BEGIN
    IF to_regclass('chat_message_context_views') IS NULL
       OR to_regclass('chat_conversations') IS NULL THEN
        RETURN;
    END IF;
    CREATE TABLE IF NOT EXISTS chat_message_context_view_frames (
        id                    bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
        conversation_id       integer     NOT NULL
                              REFERENCES chat_conversations (id) ON DELETE CASCADE,
        reader_participant_id integer     NOT NULL CHECK (reader_participant_id > 0),
        base_frame_id         bigint
                              REFERENCES chat_message_context_view_frames (id) ON DELETE CASCADE,
        depth                 integer     NOT NULL CHECK (depth >= 0),
        body                  jsonb       NOT NULL CHECK (jsonb_typeof(body) = 'array'),
        entry_count           integer     NOT NULL CHECK (entry_count >= 0),
        created_at            timestamptz NOT NULL DEFAULT clock_timestamp(),
        CONSTRAINT chat_message_context_view_frames_base_older
            CHECK (base_frame_id IS NULL OR base_frame_id < id),
        CONSTRAINT chat_message_context_view_frames_keyframe_depth
            CHECK ((base_frame_id IS NULL) = (depth = 0))
    );
    CREATE INDEX IF NOT EXISTS chat_message_context_view_frames_reader_newest
        ON chat_message_context_view_frames (conversation_id, reader_participant_id, id DESC);
    CREATE INDEX IF NOT EXISTS chat_message_context_view_frames_base
        ON chat_message_context_view_frames (base_frame_id)
        WHERE base_frame_id IS NOT NULL;

    ALTER TABLE chat_message_context_views
        ADD COLUMN IF NOT EXISTS frame_id bigint
            REFERENCES chat_message_context_view_frames (id) ON DELETE CASCADE;
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'chat_message_context_views_frame_entries'
          AND conrelid = to_regclass('chat_message_context_views')
    ) THEN
        ALTER TABLE chat_message_context_views
            ADD CONSTRAINT chat_message_context_views_frame_entries
            CHECK (frame_id IS NULL OR entries = '[]'::jsonb);
    END IF;
    CREATE INDEX IF NOT EXISTS chat_message_context_views_frame
        ON chat_message_context_views (frame_id)
        WHERE frame_id IS NOT NULL;
END
$$;
