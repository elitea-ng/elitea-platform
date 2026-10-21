-- 0152_chat_messages_attachment_object_index.sql — index the attachment rows
-- by the object they name.
--
-- A conversation fork (client contract 1.11) can leave two conversations
-- naming one stored object: an attachment whose name lacks the source
-- conversation's UUID prefix is copied as it is. The message and conversation
-- deletes therefore keep an object that another row still names
-- (collectGroupAttachments, collectConversationAttachments): a NOT EXISTS
-- anti-join on chat_messages_attachment (bucket, name), once per deleted
-- attachment.
--
-- chat_messages_attachment (0123) is keyed by the item id only, so each probe
-- would scan every attachment row of the project. Not unique: a shared object
-- is two or more rows with one (bucket, name).
--
-- GUARDED. A tenant schema that predates 0123 on a mixed deployment still has
-- to migrate; the index is created only where the table and its columns exist.
-- IDEMPOTENT (IF NOT EXISTS). No BEGIN/COMMIT: the ledgered runner wraps each
-- file. Plain CREATE INDEX rather than CONCURRENTLY, which cannot run inside
-- that transaction.
DO $migration$
BEGIN
    IF to_regclass('chat_messages_attachment') IS NOT NULL
       AND (
           SELECT count(*)
           FROM pg_attribute
           WHERE attrelid = to_regclass('chat_messages_attachment')
             AND attname IN ('bucket', 'name')
             AND NOT attisdropped
       ) = 2 THEN
        CREATE INDEX IF NOT EXISTS chat_messages_attachment_object_idx
            ON chat_messages_attachment (bucket, name);
    END IF;
END
$migration$;
