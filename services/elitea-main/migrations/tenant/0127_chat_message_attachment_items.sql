-- Give `chat_messages_attachment` an owner in this repository.
--
-- This is option 1 of #606, chosen deliberately over the alternatives: Go takes
-- migration ownership of the table, and an uploaded chat attachment becomes a
-- message item hanging off a message group, the way the legacy runtime models
-- it.
--
-- WHY THE QUESTION WAS OPEN. internal/api/v2/conversations/attachments.go:34-55
-- records that the S20a attachment port stored bytes and
-- `elitea_storage.objects` metadata but deliberately did NOT write the
-- `chat_messages_attachment` row the legacy runtime writes, because nothing
-- established who owns that table's DDL: inventing a migration risked colliding
-- with the legacy runtime's own history in a shared database. The consequences
-- accumulated. An attachment was conversation-scoped only — bytes keyed
-- `{conversationUUID}/{filename}`, a listing in
-- `chat_conversations.meta.attachments` — with NO association to the message it
-- was sent with. So it never appeared inline in the transcript, and the legacy
-- runtime's per-message attachment cleanup (api/v2/message.py:103-107) had
-- nothing to iterate and could not be ported at all.
--
-- WHY THE COLLISION RISK IS ADDRESSABLE, AND ADDRESSED. `CREATE TABLE IF NOT
-- EXISTS` makes this a no-op wherever the legacy runtime already created the
-- table, which is every deployment restored from the shared database. It
-- becomes the definition only where nothing else provides one — a legacy-free
-- deployment, the compose/standalone stack, a freshly created tenant. That is
-- the same bargain 0123, 0124, 0125 and 0126 already struck for the rest of
-- this graph.
--
-- The shape is not transcribed from the SQLAlchemy model. It is MEASURED from
-- `testdata/postgres/legacy-centry-catalog.json`, the pg_catalog dump of the
-- live legacy database, schema p_1:
--
--     name            character varying(256)  NOT NULL
--     bucket          character varying(256)  NOT NULL
--     attachment_type character varying(256)  NOT NULL
--     content         json                    NULL
--     id              integer                 NOT NULL
--     PRIMARY KEY (id)
--     FOREIGN KEY (id) REFERENCES p_1.chat_message_items(id) ON DELETE CASCADE
--
-- and it agrees with legacy/plugins/elitea_core/models/message_items/attachment.py:19-27
-- column for column. Taking the dump as the authority rather than the model is
-- the point: a model can drift from what was actually applied, and it is the
-- applied shape a mixed deployment has to keep matching.
--
-- `content` IS `json`, NOT `jsonb`, AND THAT IS NOT AN OVERSIGHT. The deployed
-- column is `json`. The two types are not interchangeable here: `jsonb`
-- normalises whitespace, reorders object keys and silently drops duplicate
-- keys, so a value written through this migration's table and read back by
-- the legacy runtime — or the reverse, in a deployment running both — would not
-- be the bytes that were stored. Every other jsonb column in 0123 is one this
-- repository owns end to end; this one is shared, so it matches.
--
-- SHARING chat_message_items' PRIMARY KEY, rather than carrying its own, is
-- what makes the item's `item_type` discriminator and its payload impossible to
-- disagree — the same reasoning 0123 records for chat_messages_text and
-- chat_messages_context, which have the identical 1:1 shape. The discriminator
-- value for this table is `attachment_message`
-- (attachment.py:15-17 `polymorphic_identity`), NOT `attachment`.
--
-- ON DELETE CASCADE FROM THE ITEM MATTERS TO CODE ALREADY WRITTEN. Every delete
-- path in ConversationsRepo — DeleteMessage, DeleteMessages, Delete — removes
-- chat_message_items rows for the groups it is clearing. With this cascade in
-- place those deletes carry the attachment rows with them automatically, so
-- none of them needs a new statement. What they still do NOT do is remove the
-- stored BYTES; that is the per-message cleanup this migration unblocks, not
-- something the FK can do.
--
-- No index beyond the primary key, matching the deployed table. The only access
-- path is a join from chat_message_items on `id`, which the primary key already
-- serves; an index this repository added and the legacy runtime's table lacked
-- would be a shape difference for no gain.
--
-- Guarded on the PARENT with to_regclass, exactly as 0125 and 0126 are: several
-- integration fixtures apply the tenant chain to a schema of their own making
-- that has no chat tables at all, and a REFERENCES clause pointing at a missing
-- relation raises 42P01 and fails the whole chain. Where the parent is absent
-- this migration must record itself and change nothing.
DO $$
BEGIN
    IF to_regclass('chat_message_items') IS NULL THEN
        RETURN;
    END IF;

    CREATE TABLE IF NOT EXISTS chat_messages_attachment (
        id integer PRIMARY KEY REFERENCES chat_message_items(id) ON DELETE CASCADE,
        name varchar(256) NOT NULL,
        bucket varchar(256) NOT NULL,
        attachment_type varchar(256) NOT NULL,
        content json
    );
END
$$;
