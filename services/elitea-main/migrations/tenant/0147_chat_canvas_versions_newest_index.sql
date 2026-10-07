-- 0147_chat_canvas_versions_newest_index.sql — index the "newest version of
-- this canvas" lookup.
--
-- chat_canvas_versions (0129) carries only its primary key and pylon's
-- `created_at` index. Every read of a canvas asks for ONE canvas's newest
-- version — `WHERE canvas_item_id = $1 ORDER BY created_at DESC, id DESC
-- LIMIT 1` (ConversationsRepo.readCanvas) — and since a canvas became part of
-- the chat history the model is shown, both turn resolvers in agent_chat.sql
-- ask it several times per canvas on EVERY turn: once to project the canvas,
-- once per newer canvas for the history budget, and once to decide whether a
-- reply carved into a canvas still counts as completed. Without an index on
-- `canvas_item_id` each of those is a scan of every version of every canvas in
-- the project, and the table only grows (an edit appends a version).
--
-- The column order is the query's own: equality on `canvas_item_id`, then the
-- ORDER BY, so the newest row is the first index entry and LIMIT 1 stops there.
--
-- GUARDED. A tenant schema that predates 0129 on a mixed deployment still has
-- to migrate; the index is created only where the table and its columns exist.
-- IDEMPOTENT (IF NOT EXISTS). No BEGIN/COMMIT: the ledgered runner wraps each
-- file. Plain CREATE INDEX rather than CONCURRENTLY, which cannot run inside
-- that transaction; the table holds one row per saved canvas edit, so the
-- build is short.
DO $migration$
BEGIN
    IF to_regclass('chat_canvas_versions') IS NOT NULL
       AND (
           SELECT count(*)
           FROM pg_attribute
           WHERE attrelid = to_regclass('chat_canvas_versions')
             AND attname IN ('canvas_item_id', 'created_at', 'id')
             AND NOT attisdropped
       ) = 3 THEN
        CREATE INDEX IF NOT EXISTS chat_canvas_versions_item_newest_idx
            ON chat_canvas_versions (canvas_item_id, created_at DESC, id DESC);
    END IF;
END
$migration$;
