-- Record what `owner_id` and `author_id` mean, on the columns themselves.
--
-- Issue #533. The name `owner_id` holds a PROJECT in one tenant table and a
-- USER in another, and no foreign key catches either. A person who reads the
-- name and assumes one meaning is correct for one table and wrong for the next.
-- A join written on that assumption returns rows that look valid.
--
-- This file writes the meaning into pg_description with COMMENT ON COLUMN, so
-- `\d+ p_1.applications` in psql, every schema browser and every introspection
-- tool answer the question at the point where the reader asks it. It changes no
-- data and adds no constraint.
--
--
-- THE TABLE OF MEANINGS, MEASURED FROM THE WRITERS
--
-- Each row names the writer that proves the meaning. The name of the column
-- proves nothing and is not used as evidence.
--
--   applications.owner_id              PROJECT
--     legacy/plugins/elitea_core/api/v2/applications.py:163 overwrites the
--     payload with `raw["owner_id"] = project_id` before the model validates,
--     and line 138 states it: "owner_id is current project ID". Reads agree:
--     `Application.owner_id == project_id` (api/v2/recommendations.py:160,201)
--     and `Application.owner_id == public_project_id`
--     (methods/admin_tasks.py:1746). This service agrees on the READ side too:
--     Fork copies an exported `owner_id` straight into
--     `forkMeta["parent_project_id"]`
--     (internal/api/v2/eliteacore/handler.go).
--     DISPUTED BY THIS SERVICE'S WRITERS. See the deferred item below.
--
--   applications.shared_owner_id       PROJECT
--     The project an agent was published FROM. Not touched here; it already
--     carries "shared" in its name and no writer disagrees.
--
--   application_versions.author_id     USER
--     internal/api/v2/eliteacore/handler.go writes the caller's user id.
--
--   skills.owner_id                    PROJECT
--     legacy/plugins/elitea_core/api/v2/skills.py:95 overwrites the payload
--     with `raw["owner_id"] = project_id`; reads filter with
--     `Skill.owner_id == project_id` (utils/skill_utils.py:1066); publishing
--     writes `owner_id=public_project_id` into the public project's schema
--     (utils/skill_publish_utils.py:786).
--     internal/api/v2/skillpublish/attach.go:193 and publish.go:311 both write
--     a project. internal/infra/db/repos/skills.go wrote the literal 1 and now
--     writes the project (this change).
--
--   skills.author_id                   USER
--     internal/api/v2/skillpublish/attach.go:193 writes the caller's user id.
--
--   skill_versions.author_id           USER
--     internal/api/v2/skillpublish insertVersion writes the caller's user id.
--
--   elitea_tools.owner_id              PROJECT
--     internal/api/v2/toolkits/handler.go createToolkitInsertSQL, asserted
--     against this corpus by create_toolkit_owner_id_test.go, and
--     internal/api/v2/eliteacore/import_owner.go. Pull request #522 owns this
--     column and settled it. NOTE: a schema built by the legacy runtime has no
--     `owner_id` on this table at all, which is why the loop below probes for
--     the column and not only for the table.
--
--   elitea_tools.author_id             USER
--     The same two writers bind it to the caller's user id, on its own
--     placeholder.
--
--   chat_conversation_folders.owner_id USER
--     legacy/plugins/elitea_core/models/folder.py:21 says so in the model, and
--     every read filters `ConversationFolder.owner_id == user_id`
--     (api/v2/folder.py:52,306,584 and others).
--
--   chat_conversations.author_id       USER
--     internal/infra/db/repos/conversations.go writes the conversation author.
--
--   configuration.author_id            USER
--     internal/api/v2/configurations/mutation.go refuses the write with
--     "authentication required" when there is no principal.
--
-- One table is deliberately absent: `prompt_collections`. No migration in this
-- repository creates it, and the legacy runtime carries an admin task that
-- DROPS it (methods/admin_tasks.py:2481-2566). Its `owner_id` has no proven
-- meaning, so this file states none. Do not build on that column.
--
--
-- WHY NO FOREIGN KEY
--
-- A PROJECT-kind column would reference `centry.project`. This corpus already
-- records that decision three times, for the same reason each time:
-- `centry.project` is a legacy-owned table, so a Go-owned column that names a
-- project carries no foreign key to it and is validated at the write instead
-- (shared/0071 token_project_binding, shared/0073 mcp_tool_registry,
-- shared/0098 scim_group_bindings). This file follows that decision rather
-- than reversing it in a tenant migration.
--
-- A USER-kind column would reference `public.auth_core__user`. That table is
-- legacy-owned as well, and it is ABSENT from a corpus-only database — the
-- repository test template is built from this corpus alone (see the header of
-- tenant/0126). A guarded `REFERENCES` would therefore exist on a deployment
-- and not exist under test, which is a constraint that only appears where it
-- cannot be exercised.
--
-- The validation lives at the write instead:
-- internal/infra/db/tenantschema/owner.go OwnerID returns the one number a
-- PROJECT-kind owner_id may hold in schema p_<id>, and refuses anything that
-- is not a project id.
--
--
-- DEFERRED, DELIBERATELY: applications.owner_id
--
-- Every writer in this service puts the CALLER'S USER ID into
-- `applications.owner_id`: internal/infra/db/repos/applications.go Create, and
-- the import, fork and sub-agent-embed paths in
-- internal/api/v2/eliteacore/handler.go. One reader depends on it being a user
-- (`LEFT JOIN public.auth_core__user u ON u.id = a.owner_id` in
-- repos/applications.go) and one reader depends on it being a project (Fork,
-- above). Both cannot be right.
--
-- Correcting the writers needs a data migration for the rows that exist, a
-- replacement source for the author name the list join reads today, and a
-- decision about the `owner_id` field in the applications API response. None of
-- that fits in a comment, and a constraint added before it would refuse rows
-- this service is still writing. So this file records the disagreement and
-- adds no constraint. That is the whole point of the record: the next reader
-- sees the dispute instead of discovering it from a wrong join.
--
--
-- MECHANICS
--
-- All table names are UNQUALIFIED. Tenant migrations run with a
-- transaction-local search_path pinned to the tenant schema
-- (internal/infra/db/migrate/runner.go), which is verified to be the effective
-- schema before any file runs.
--
-- Each entry is probed twice, for the TABLE and for the COLUMN. A schema built
-- by the legacy runtime carries `elitea_tools` without `owner_id`, and a
-- corpus-only schema carries neither `applications` nor `configuration`.
--
-- COMMENT ON requires ownership of the table. Every deployment this repository
-- builds runs one database role, and 0122, 0124 and 0126 already ALTER
-- legacy-created tables on that assumption. An installation that splits the
-- roles raises a NOTICE per column and keeps this file as the record, rather
-- than failing a migration chain over a comment.
--
-- IDEMPOTENT: COMMENT ON replaces the comment it finds.
--
-- No BEGIN/COMMIT: the ledgered runner executes each file inside one
-- transaction with its ledger row (migrate/runner.go apply).
DO $$
DECLARE
    entry record;
BEGIN
    FOR entry IN
        SELECT *
          FROM (VALUES
            ('applications', 'owner_id',
             'PROJECT id, not user id. The legacy runtime sets it to the route project (elitea_core/api/v2/applications.py:163) and Fork reads it as parent_project_id. DISPUTED: every writer in elitea-main stores the caller user id. See migrations/tenant/0128 and issue #533. Do not join this column to a user table.'),
            ('application_versions', 'author_id',
             'USER id. The principal that created the version.'),
            ('skills', 'owner_id',
             'PROJECT id, not user id. The legacy runtime sets it to the route project (elitea_core/api/v2/skills.py:95) and reads filter Skill.owner_id = project_id. Rows created before issue #533 may hold the literal 1.'),
            ('skills', 'author_id',
             'USER id. The principal that created the skill.'),
            ('skill_versions', 'author_id',
             'USER id. The principal that created the version.'),
            ('elitea_tools', 'owner_id',
             'PROJECT id, not user id. Pull request #522 settled it; author_id holds the user. A schema built by the legacy runtime has no such column.'),
            ('elitea_tools', 'author_id',
             'USER id. The principal that created the toolkit.'),
            ('chat_conversation_folders', 'owner_id',
             'USER id, not project id. The legacy runtime filters every folder read with owner_id = user_id (elitea_core/api/v2/folder.py). elitea-main writes the literal 1 today.'),
            ('chat_conversations', 'author_id',
             'USER id. The principal that started the conversation.'),
            ('configuration', 'author_id',
             'USER id. The principal that wrote the configuration.')
          ) AS t(table_name, column_name, meaning)
    LOOP
        IF to_regclass(entry.table_name) IS NULL THEN
            CONTINUE;
        END IF;
        IF NOT EXISTS (
            SELECT 1
              FROM pg_attribute
             WHERE attrelid = to_regclass(entry.table_name)
               AND attname = entry.column_name
               AND attnum > 0
               AND NOT attisdropped
        ) THEN
            CONTINUE;
        END IF;
        BEGIN
            EXECUTE format('COMMENT ON COLUMN %I.%I IS %L',
                           entry.table_name, entry.column_name, entry.meaning);
        EXCEPTION
            WHEN insufficient_privilege THEN
                RAISE NOTICE '0128: %.% belongs to another role, comment skipped; this file stays the record',
                    entry.table_name, entry.column_name;
        END;
    END LOOP;
END $$;
