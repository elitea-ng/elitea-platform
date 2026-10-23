-- Give `entity_tool_mapping` the `entity_id` column every writer and the chat
-- read already name.
--
-- MEASURED, by reading information_schema on a database freshly built from
-- internal/infra/db/migrations/001_initial.sql on pgvector/pgvector:0.8.1-pg18.
-- The bootstrap table (001_initial.sql:451-460) has exactly seven columns:
--
--     id, entity_version_id, entity_type, tool_id, selected_tools,
--     created_at, updated_at
--
-- The production legacy runtime schema this service inherits has an eighth,
-- `entity_id integer NOT NULL` — see internal/db/schema/agent_chat_baseline.sql:104-113,
-- which is the sqlc COMPILER projection of the deployed shape and states in its
-- own header that "the tenant schema lifecycle continues to own this
-- already-existing table". That lifecycle is the legacy runtime's, and nothing
-- in this repository ever reconciled the two.
--
-- Every writer in this repository names the column:
--
--   * internal/api/v2/toolkits/handler.go:818,820 — `updateToolRelation`'s
--     attach/selection-edit INSERT, the route the agent editor's ToolMenu
--     drives.
--   * internal/api/v2/eliteacore/handler.go:712, :858, :2318 — the publish-clone,
--     embedded-version clone and import INSERTs.
--
-- and the chat read joins on it:
--
--   * internal/db/queries/agent_chat.sql:107 (and its generated twin
--     sqlcgen/agent_chat.sql.go:1234)
--         AND application_tool_mapping.entity_id = application_version.application_id
--
--     `application_tool_mapping` is NOT a second table and not a view: line 88
--     of the same query reads `FROM entity_tool_mapping AS application_tool_mapping`.
--     It is an alias, and that alias is the clearest statement of what the
--     column MEANS — `entity_version_id` identifies the application VERSION the
--     toolkit is attached to, `entity_id` identifies the application (the agent)
--     that version belongs to, and `entity_type` says which kind of entity that
--     is ('agent' from the editor, 'application' from import). The attach INSERT
--     agrees: it stores the request's `entity_id` alongside `entity_version_id`,
--     and eliteacore's import passes `info.appID` with the version's `vID`.
--
-- So on any database provisioned by these migrations rather than restored from
-- a legacy runtime dump — the compose/standalone stack, a freshly created
-- tenant — attaching a toolkit to an agent answers
--
--     500 {"error":"ERROR: column \"entity_id\" of relation
--          \"entity_tool_mapping\" does not exist (SQLSTATE 42703)"}
--
-- and the chat turn's tool-resolution subquery fails on the same column.
-- Dump-loaded stacks hide it because db.RunMigrations skips the whole baseline
-- set once tenant schemas already exist (internal/infra/db/migrate.go:38-47),
-- so the only schema anyone had actually looked at was the legacy runtime's.
--
-- This cannot be an edit to 001_initial.sql: it is already applied and the
-- ledger pins its checksum.
--
-- WHY `NOT NULL`, MATCHING THE DEPLOYED SHAPE, RATHER THAN NULLABLE-FOR-NOW
--
-- Adding a NOT NULL column with no default requires the table to be empty or
-- every existing row to be given a value. On a database that is missing this
-- column the table is necessarily EMPTY, because there is no statement in this
-- repository that can insert into it:
--
--   * the five INSERTs listed above all name `entity_id`, so they raise 42703;
--   * the only two that do not — internal/api/oapiserver/publishing.go:107 and
--     versions.go:202, both `INSERT ... (entity_version_id, tool_id)` — omit
--     `entity_type` as well, which is NOT NULL with no default in the bootstrap
--     table AND in the legacy runtime shape, so they raise 23502 instead. (They
--     are written `_, _ = pool.Exec(...)`, so they have been silently writing
--     nothing on every deployment; that is a separate defect, not this one, and
--     this migration does not change their behaviour.)
--
-- The backfill below therefore has nothing to do in practice. It is here so
-- that the statement after it is safe rather than merely lucky, and it uses the
-- exact relationship the writers and the chat join use: an entity's id is the
-- `application_id` of the version the mapping points at. Guessing anything else
-- — `entity_version_id` itself, say — would write plausible-looking rows that
-- satisfy NOT NULL and then join to nothing, which is a worse outcome than the
-- 42703 this file removes.
--
-- Landing it nullable "for now" was the alternative and was rejected: it would
-- leave a Go-migrated database with a different shape from a legacy-migrated
-- one indefinitely, which is precisely the divergence that produced this defect
-- and the five before it in this branch.
--
-- Guarded on the TABLE with to_regclass for the same reason as 0124: several
-- integration fixtures apply the tenant chain to a schema of their own making
-- that has no `entity_tool_mapping` at all, and `ALTER TABLE` on a missing
-- relation raises 42P01 and fails the whole chain. Where the table is absent
-- this migration must record itself and move on. Where the legacy runtime
-- already created the column, `ADD COLUMN IF NOT EXISTS` is a no-op and `SET
-- NOT NULL` on an already-NOT NULL column is likewise a no-op, so the mixed
-- deployment is untouched.
DO $$
BEGIN
    IF to_regclass('entity_tool_mapping') IS NULL THEN
        RETURN;
    END IF;

    ALTER TABLE entity_tool_mapping ADD COLUMN IF NOT EXISTS entity_id INTEGER;

    -- Inert on every database this has been measured against (see above); it
    -- exists so that the SET NOT NULL below cannot be the statement that breaks
    -- a tenant chain. `application_versions` is the parent for both entity
    -- types this table carries — the editor's 'agent' rows and eliteacore's
    -- 'application' rows both point `entity_version_id` at it.
    IF to_regclass('application_versions') IS NOT NULL THEN
        UPDATE entity_tool_mapping AS mapping
           SET entity_id = version.application_id
          FROM application_versions AS version
         WHERE mapping.entity_id IS NULL
           AND version.id = mapping.entity_version_id;
    END IF;

    ALTER TABLE entity_tool_mapping ALTER COLUMN entity_id SET NOT NULL;
END
$$;
