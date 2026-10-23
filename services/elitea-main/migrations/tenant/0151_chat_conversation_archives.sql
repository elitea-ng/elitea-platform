-- 0151_chat_conversation_archives.sql — the conversation archive
-- (client contract 1.10, archiveConversation / unarchiveConversation).
--
-- WHAT IT HOLDS
--
-- One row per archived conversation. The archive is the PROJECT's, as the
-- conversation pin is: every member's default conversation list leaves the
-- conversation out, and every list row names who archived it and when. A
-- repeated archive keeps the first row; an unarchive removes the row,
-- whoever wrote it. The pin (centry.social_pins) does not change.
--
--   chat_conversation_archives
--     conversation_id  integer    PRIMARY KEY REFERENCES chat_conversations(id) ON DELETE CASCADE
--     archived_by      bigint     NOT NULL   -- the user id of the member who archived it
--     archived_at      timestamp  NOT NULL   -- the UTC wall clock, as chat_conversations
--
-- WHY THE TENANT SCHEMA, NOT centry. The pin is in centry because the legacy
-- runtime's Projects plugin owned that table first (shared/0064). Nothing owns
-- an archive table, and the tenant schema IS the project, so the key is the
-- conversation alone. The foreign key does the delete cleanup: a deleted
-- conversation takes its archive row with it, and a dropped project schema
-- takes all of them. No delete flow and no deprovision step must remember it.
--
-- `archived_by` is not a foreign key, as user_id in chat_message_feedback
-- (0135) is not: user identity is central (auth.User), never a tenant-schema
-- key. It is bigint because the caller's owning user id is an int64.
--
-- GUARDS. Guarded on the PARENT with to_regclass, as 0135: some integration
-- fixtures apply the tenant chain to a schema without chat_conversations,
-- and a REFERENCES clause against a missing relation raises 42P01. IF NOT
-- EXISTS keeps it idempotent. No BEGIN/COMMIT: the ledgered runner wraps
-- each file.
DO $$
BEGIN
    IF to_regclass('chat_conversations') IS NULL THEN
        RETURN;
    END IF;

    CREATE TABLE IF NOT EXISTS chat_conversation_archives (
        conversation_id integer PRIMARY KEY REFERENCES chat_conversations(id) ON DELETE CASCADE,
        archived_by bigint NOT NULL,
        archived_at timestamp NOT NULL DEFAULT (now() AT TIME ZONE 'UTC')
    );
END
$$;
