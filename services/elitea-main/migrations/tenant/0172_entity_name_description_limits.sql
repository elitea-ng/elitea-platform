-- 0172_entity_name_description_limits.sql — the entity text limits of issue
-- 1968: a name is at most 128 characters, a description at most 2000.
--
-- The handlers refuse longer input with a 400 (pkg/entitylimits). This
-- migration brings what is already stored under the same limits, and the
-- constraints keep a writer that bypasses the handlers honest. The owner's
-- decision is that stored text over the limit is TRUNCATED (left(col, N), in
-- characters), not kept.
--
-- What is covered, per table:
--   applications.description          2000  truncate + CHECK
--   skills.description                2000  truncate + CHECK
--   elitea_tools.description          2000  truncate + CHECK
--   chat_conversation_folders.name     128  truncate + CHECK
--   chat_conversations.name            128  truncate only
--   configuration.label                128  truncate only
--   configuration.elitea_title         128  truncate only (keeps UNIQUE, below)
-- applications / application_versions / skills / skill_versions / elitea_tools
-- names are already VARCHAR(128) (001_initial.sql), so they need nothing here.
--
-- Why chat_conversations.name and configuration.* have no CHECK: the platform
-- itself writes those columns from several places besides the handlers (a
-- fork's default name, the DeepWiki, heartbeat and support-assistant
-- conversations, credential references), and a CHECK would turn a name one of
-- them produced one character too long into a 500. The handlers are the gate
-- there.
--
-- configuration.elitea_title is UNIQUE, so a plain left() could make two
-- titles equal. A title over the limit is cut to leave room for `~<id>` and
-- gets that suffix, which keeps it unique and 128 characters at most.
--
-- Every table is guarded, for the reason 0134 and 0160 are: several
-- integration fixtures apply the tenant chain to a schema without them.
-- Unqualified names: the runner pins search_path to the tenant schema. No
-- BEGIN/COMMIT. Idempotent: a second run truncates nothing and finds each
-- constraint in place.
DO $$
DECLARE
    spec record;
BEGIN
    FOR spec IN
        SELECT * FROM (VALUES
            ('applications',               'description', 2000, 'applications_description_max_len'),
            ('skills',                     'description', 2000, 'skills_description_max_len'),
            ('elitea_tools',               'description', 2000, 'elitea_tools_description_max_len'),
            ('chat_conversation_folders',  'name',         128, 'chat_conversation_folders_name_max_len')
        ) AS t(tbl, col, max_len, constraint_name)
    LOOP
        IF to_regclass(spec.tbl) IS NULL THEN
            CONTINUE;
        END IF;
        EXECUTE format('UPDATE %I SET %I = left(%I, %s) WHERE char_length(%I) > %s',
                       spec.tbl, spec.col, spec.col, spec.max_len, spec.col, spec.max_len);
        IF NOT EXISTS (
            SELECT 1 FROM pg_constraint
             WHERE conrelid = to_regclass(spec.tbl) AND conname = spec.constraint_name
        ) THEN
            EXECUTE format('ALTER TABLE %I ADD CONSTRAINT %I CHECK (char_length(%I) <= %s)',
                           spec.tbl, spec.constraint_name, spec.col, spec.max_len);
        END IF;
    END LOOP;

    IF to_regclass('chat_conversations') IS NOT NULL THEN
        UPDATE chat_conversations SET name = left(name, 128) WHERE char_length(name) > 128;
    END IF;

    IF to_regclass('configuration') IS NOT NULL THEN
        UPDATE configuration SET label = left(label, 128) WHERE char_length(label) > 128;
        UPDATE configuration
           SET elitea_title = left(elitea_title, 128 - char_length('~' || id::text)) || '~' || id::text
         WHERE char_length(elitea_title) > 128;
    END IF;
END
$$;
