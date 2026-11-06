-- 0162_chat_participant_kinds.sql — typed participants and message authors,
-- addressees, visibility and cause (ADR-0050 decision 7a and 7b, issue 1605).
--
-- chat_participants
--   kind     human | agent | ephemeral_agent | system. NULL for an
--            attachment (toolkit, prompt, pipeline step, skill, mcp): it is
--            in the conversation but never speaks. Backfilled from
--            entity_name: user -> human; application, llm, dummy -> agent;
--            system -> system.
--
-- chat_participant_mapping
--   persona  the agent's persona snapshot in THIS conversation: display_name,
--            pronouns and role from the soul (tenant/0160) of the version the
--            conversation runs, and later avatar_pack (ADR-0050 decision 6).
--            It is on the mapping, not the participant, because one
--            participant row is shared by every conversation the agent is in
--            and each conversation pins its own version
--            (entity_settings.version_id). Written when the agent joins and
--            refreshed on every turn, so a new release's soul shows from its
--            first answer. NULL: no persona.
--
-- chat_message_group
--   author_kind   the author participant's kind, copied at write time. An
--                 attachment that authors a message (DeepWiki answers as its
--                 toolkit participant) is an agent.
--   addressed_to  participant ids the message is for; empty is everyone.
--   visibility    all | side (agent-to-agent talk, shown collapsed).
--   cause         what started the message: human_message | agent_mention |
--                 heartbeat | delegation | system. NULL when unknown.
--
-- sent_to_id STAYS for one release. A writer may set either: a group with a
-- sent_to_id and no addressees gets addressed_to = {sent_to_id}, and a group
-- addressed to exactly one participant with no sent_to_id gets that one as
-- sent_to_id. The next release derives sent_to_id only.
--
-- WHY TRIGGERS. The tables are written by sqlc queries, raw SQL in many repos,
-- the MCP and pipeline-trigger paths and the local (desktop) turn writer, as
-- 0144 records. A BEFORE INSERT trigger fills what a writer left unset, so no
-- writer can store a message without its author kind. A writer that knows
-- better (the turn-taking engine, an agent-to-agent message) sets the columns
-- itself and the trigger keeps them.
--
-- BACKFILL WITHOUT A SYNC STORM. The backfill UPDATE would pass through 0144's
-- chat_sync_stamp trigger and re-deliver every message of the tenant to every
-- synced client. Readers derive the same values from entity_name and
-- sent_to_id when a column is NULL, so a cached row is not wrong; the user
-- triggers are disabled for the backfill only (DISABLE TRIGGER USER keeps the
-- foreign-key triggers).
--
-- GUARDS. Every table is checked with to_regclass, for the reason 0134 gives.
-- IDEMPOTENT. No BEGIN/COMMIT: the ledgered runner wraps the file.
DO $migration$
DECLARE
    tenant text := current_schema();
BEGIN
    IF tenant IS NULL THEN
        RAISE EXCEPTION 'chat participant kinds migration needs a tenant search_path';
    END IF;

    IF to_regclass('chat_participants') IS NOT NULL THEN
        ALTER TABLE chat_participants
            ADD COLUMN IF NOT EXISTS kind text NULL;
        ALTER TABLE chat_participants DROP CONSTRAINT IF EXISTS chat_participants_kind;
        ALTER TABLE chat_participants ADD CONSTRAINT chat_participants_kind
            CHECK (kind IS NULL OR kind IN ('human', 'agent', 'ephemeral_agent', 'system'));

        EXECUTE format($fn$
            CREATE OR REPLACE FUNCTION %1$I.chat_participant_kind_of(entity_name text) RETURNS text
            LANGUAGE sql IMMUTABLE AS $body$
                SELECT CASE entity_name
                    WHEN 'user' THEN 'human'
                    WHEN 'application' THEN 'agent'
                    WHEN 'llm' THEN 'agent'
                    WHEN 'dummy' THEN 'agent'
                    WHEN 'system' THEN 'system'
                END
            $body$ $fn$, tenant);

        EXECUTE format($fn$
            CREATE OR REPLACE FUNCTION %1$I.chat_participant_kind_stamp() RETURNS trigger
            LANGUAGE plpgsql AS $body$
            BEGIN
                IF NEW.kind IS NULL THEN
                    NEW.kind := %1$I.chat_participant_kind_of(NEW.entity_name);
                END IF;
                RETURN NEW;
            END
            $body$ $fn$, tenant);
        DROP TRIGGER IF EXISTS chat_participant_kind_stamp ON chat_participants;
        CREATE TRIGGER chat_participant_kind_stamp BEFORE INSERT ON chat_participants
            FOR EACH ROW EXECUTE FUNCTION chat_participant_kind_stamp();

        UPDATE chat_participants
           SET kind = chat_participant_kind_of(entity_name)
         WHERE kind IS NULL AND chat_participant_kind_of(entity_name) IS NOT NULL;
    END IF;

    IF to_regclass('chat_participant_mapping') IS NOT NULL THEN
        ALTER TABLE chat_participant_mapping ADD COLUMN IF NOT EXISTS persona jsonb NULL;
        ALTER TABLE chat_participant_mapping DROP CONSTRAINT IF EXISTS chat_participant_mapping_persona;
        ALTER TABLE chat_participant_mapping ADD CONSTRAINT chat_participant_mapping_persona
            CHECK (persona IS NULL OR (jsonb_typeof(persona) = 'object' AND octet_length(persona::text) <= 4096));

        -- The persona snapshot of a soul (tenant/0160): the one expression
        -- the backfill below and every application turn
        -- (InsertCurrentApplicationTurn) use. NULL for no persona.
        EXECUTE format($fn$
            CREATE OR REPLACE FUNCTION %1$I.chat_persona_of(soul jsonb) RETURNS jsonb
            LANGUAGE sql IMMUTABLE AS $body$
                SELECT CASE WHEN jsonb_typeof(soul) = 'object' THEN
                    NULLIF(jsonb_strip_nulls(jsonb_build_object(
                        'display_name', soul ->> 'display_name',
                        'pronouns', soul ->> 'pronouns',
                        'role', soul ->> 'role')), '{}'::jsonb)
                END
            $body$ $fn$, tenant);

        -- The souls stored since 0160: the agents that already have one show
        -- it before their next turn.
        IF to_regclass('chat_participants') IS NOT NULL
           AND to_regclass('application_versions') IS NOT NULL
           AND (SELECT count(*) FROM pg_attribute WHERE attrelid = to_regclass('application_versions')
                  AND NOT attisdropped AND attname = ANY (ARRAY['soul', 'application_id'])) = 2
           AND EXISTS (SELECT 1 FROM pg_attribute WHERE attrelid = to_regclass('chat_participant_mapping')
                         AND attname = 'entity_settings' AND NOT attisdropped) THEN
            UPDATE chat_participant_mapping AS mapping
               SET persona = chat_persona_of(version.soul)
              FROM chat_participants AS participant, application_versions AS version
             WHERE participant.id = mapping.participant_id
               AND participant.entity_name = 'application'
               AND mapping.persona IS NULL
               AND mapping.entity_settings ->> 'version_id' ~ '^[0-9]{1,9}$'
               AND version.id = (mapping.entity_settings ->> 'version_id')::integer
               AND version.application_id::text = participant.entity_meta ->> 'id'
               AND jsonb_typeof(version.soul) = 'object';
        END IF;
    END IF;

    IF to_regclass('chat_message_group') IS NOT NULL
       AND to_regclass('chat_participants') IS NOT NULL
       AND (SELECT count(*) FROM pg_attribute WHERE attrelid = to_regclass('chat_message_group')
              AND NOT attisdropped
              AND attname = ANY (ARRAY['author_participant_id', 'sent_to_id', 'reply_to_id'])) = 3 THEN
        ALTER TABLE chat_message_group
            ADD COLUMN IF NOT EXISTS author_kind text NULL,
            ADD COLUMN IF NOT EXISTS addressed_to integer[] NOT NULL DEFAULT '{}',
            ADD COLUMN IF NOT EXISTS visibility text NOT NULL DEFAULT 'all',
            ADD COLUMN IF NOT EXISTS cause text NULL;
        ALTER TABLE chat_message_group DROP CONSTRAINT IF EXISTS chat_message_group_author_kind;
        ALTER TABLE chat_message_group ADD CONSTRAINT chat_message_group_author_kind
            CHECK (author_kind IS NULL OR author_kind IN ('human', 'agent', 'ephemeral_agent', 'system'));
        ALTER TABLE chat_message_group DROP CONSTRAINT IF EXISTS chat_message_group_visibility;
        ALTER TABLE chat_message_group ADD CONSTRAINT chat_message_group_visibility
            CHECK (visibility IN ('all', 'side'));
        ALTER TABLE chat_message_group DROP CONSTRAINT IF EXISTS chat_message_group_cause;
        ALTER TABLE chat_message_group ADD CONSTRAINT chat_message_group_cause
            CHECK (cause IS NULL OR cause IN ('human_message', 'agent_mention', 'heartbeat', 'delegation', 'system'));
        ALTER TABLE chat_message_group DROP CONSTRAINT IF EXISTS chat_message_group_addressed_to;
        ALTER TABLE chat_message_group ADD CONSTRAINT chat_message_group_addressed_to
            CHECK (cardinality(addressed_to) <= 64 AND array_position(addressed_to, NULL) IS NULL);

        -- What started a message, from its author and the message it answers.
        EXECUTE format($fn$
            CREATE OR REPLACE FUNCTION %1$I.chat_message_cause_of(author_kind text, reply_kind text) RETURNS text
            LANGUAGE sql IMMUTABLE AS $body$
                SELECT CASE
                    WHEN author_kind = 'human' THEN 'human_message'
                    WHEN author_kind = 'system' THEN 'system'
                    WHEN reply_kind = 'human' THEN 'human_message'
                    WHEN reply_kind IN ('agent', 'ephemeral_agent') THEN 'agent_mention'
                    WHEN reply_kind = 'system' THEN 'system'
                END
            $body$ $fn$, tenant);

        EXECUTE format($fn$
            CREATE OR REPLACE FUNCTION %1$I.chat_message_author_stamp() RETURNS trigger
            LANGUAGE plpgsql AS $body$
            DECLARE
                reply_kind text;
            BEGIN
                IF NEW.author_kind IS NULL THEN
                    -- An attachment that writes a message (the DeepWiki
                    -- answer is authored by its toolkit participant) speaks
                    -- as an agent.
                    SELECT COALESCE(p.kind, %1$I.chat_participant_kind_of(p.entity_name), 'agent')
                      INTO NEW.author_kind
                      FROM %1$I.chat_participants AS p WHERE p.id = NEW.author_participant_id;
                END IF;
                IF NEW.sent_to_id IS NOT NULL AND cardinality(NEW.addressed_to) = 0 THEN
                    NEW.addressed_to := ARRAY[NEW.sent_to_id];
                ELSIF NEW.sent_to_id IS NULL AND cardinality(NEW.addressed_to) = 1 THEN
                    NEW.sent_to_id := NEW.addressed_to[1];
                END IF;
                IF NEW.cause IS NULL THEN
                    IF NEW.reply_to_id IS NOT NULL THEN
                        SELECT COALESCE(g.author_kind, COALESCE(p.kind, %1$I.chat_participant_kind_of(p.entity_name)))
                          INTO reply_kind
                          FROM %1$I.chat_message_group AS g
                          LEFT JOIN %1$I.chat_participants AS p ON p.id = g.author_participant_id
                         WHERE g.id = NEW.reply_to_id;
                    END IF;
                    NEW.cause := %1$I.chat_message_cause_of(NEW.author_kind, reply_kind);
                END IF;
                RETURN NEW;
            END
            $body$ $fn$, tenant);
        DROP TRIGGER IF EXISTS chat_message_author_stamp ON chat_message_group;
        CREATE TRIGGER chat_message_author_stamp BEFORE INSERT ON chat_message_group
            FOR EACH ROW EXECUTE FUNCTION chat_message_author_stamp();

        ALTER TABLE chat_message_group DISABLE TRIGGER USER;
        UPDATE chat_message_group AS g
           SET author_kind = COALESCE(p.kind, chat_participant_kind_of(p.entity_name), 'agent')
          FROM chat_participants AS p
         WHERE p.id = g.author_participant_id AND g.author_kind IS NULL;
        UPDATE chat_message_group
           SET addressed_to = ARRAY[sent_to_id]
         WHERE sent_to_id IS NOT NULL AND cardinality(addressed_to) = 0;
        UPDATE chat_message_group AS g
           SET cause = chat_message_cause_of(g.author_kind, reply.author_kind)
          FROM chat_message_group AS reply
         WHERE g.cause IS NULL AND g.reply_to_id = reply.id
           AND chat_message_cause_of(g.author_kind, reply.author_kind) IS NOT NULL;
        UPDATE chat_message_group
           SET cause = chat_message_cause_of(author_kind, NULL)
         WHERE cause IS NULL AND reply_to_id IS NULL
           AND chat_message_cause_of(author_kind, NULL) IS NOT NULL;
        ALTER TABLE chat_message_group ENABLE TRIGGER USER;
    END IF;
END
$migration$;
