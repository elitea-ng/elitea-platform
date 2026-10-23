-- Bring `elitea_tools` up to the shape every toolkit query already assumes.
--
-- internal/db/schema/toolkit_baseline.sql is the sqlc projection of this table
-- and, like agent_chat_baseline.sql before #287, it says the table "already
-- exists in each p_<project_id> schema and is owned by the current
-- tenant-schema lifecycle" — the legacy runtime's. So the Go bootstrap schema
-- (internal/infra/db/migrations/001_initial.sql:435-447) created its own,
-- narrower version, and nothing reconciled the two.
--
-- Three columns are in the projection and not in the bootstrap table:
-- `updated_at`, `shared_owner_id`, `shared_id`. They are not decoration —
-- internal/db/queries/toolkits.sql selects all three by name, so on a
-- legacy-free deployment the very first toolkit read fails with
--
--     ERROR: column "updated_at" does not exist  (SQLSTATE 42703)
--
-- MEASURED on the standalone stack: POST /api/v2/elitea_core/test_toolkit_tool/
-- prompt_lib/1 (the #93 Surface A index-start route) answered
-- `500 {"error":"Failed to start index_data"}` for this reason and no other —
-- the toolkit was never read, so the request could not even reach the
-- `ErrToolkitNotVisible` 404 it deserved for naming a toolkit that did not
-- exist.
--
-- Additive and IF NOT EXISTS for the same reason as 0123: in a mixed
-- deployment the legacy runtime already created these columns and owns their
-- shape, so there this migration records itself and changes nothing.
-- Nullability of the existing columns is deliberately left alone — narrowing
-- `settings`/`meta` to NOT NULL would be a change to a shape this repository
-- does not own.
--
-- (shared_owner_id, shared_id) mirrors 0122's skills columns: it points a
-- public-catalog toolkit at the project + toolkit it was published from.

-- Guarded on the TABLE, not just the columns. `ADD COLUMN IF NOT EXISTS`
-- tolerates a column that is already there; it does NOT tolerate a missing
-- table, which raises 42P01 and fails the whole tenant chain.
--
-- And the table IS missing on a schema this repository builds by itself:
-- `elitea_tools` appears only in `internal/db/schema/toolkit_baseline.sql`, a
-- sqlc COMPILER projection, and in no migration — the same shape/ownership
-- split 0123 was written for, one table further along. Any tenant schema
-- created without the legacy runtime therefore has no `elitea_tools` at all,
-- and this migration has nothing to say to it: it must record itself and move
-- on, not abort. Caught by
-- `configurations_artifacts_postgres_integration_test.go`, which applies the
-- embedded tenant chain to a schema of its own making and runs only where a
-- test database is configured.
--
-- This deliberately does NOT create the table. Claiming a legacy-owned table
-- here has broken tenant seeding with 42P07 before; adopting `elitea_tools`
-- is a separate decision about ownership, not a fix for this ALTER.
DO $$
BEGIN
    IF to_regclass('elitea_tools') IS NULL THEN
        RETURN;
    END IF;

    ALTER TABLE elitea_tools ADD COLUMN IF NOT EXISTS updated_at TIMESTAMP;
    ALTER TABLE elitea_tools ADD COLUMN IF NOT EXISTS shared_owner_id INTEGER;
    ALTER TABLE elitea_tools ADD COLUMN IF NOT EXISTS shared_id INTEGER;
END
$$;
