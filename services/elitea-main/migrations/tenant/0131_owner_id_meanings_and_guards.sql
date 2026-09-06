-- Settle `owner_id`, repair the rows that hold the wrong kind of number, and
-- make the database refuse the next one.
--
-- Issue #533. tenant/0128 wrote the table of meanings onto the columns with
-- COMMENT ON COLUMN. It added no constraint, it stated no meaning for
-- `prompt_collections`, and it left `applications.owner_id` DISPUTED: the
-- legacy runtime and this service's fork path read the column as a PROJECT,
-- and every writer in this service stored the caller USER id. A comment cannot
-- settle a dispute, and a reader still had to guess.
--
-- This file settles it. It does three things, in this order, per tenant schema:
--
--   1. REPAIR. Each PROJECT-kind `owner_id` gets the project of the schema it
--      lives in. This is a data change, and it is the plan the issue asks for.
--   2. CONSTRAIN. Each repaired column gets a FOREIGN KEY to centry.project(id).
--      A number that names no project is now refused by the database.
--   3. RECORD. Each column, and both `prompt_collections` columns, gets the
--      meaning written on it with COMMENT ON COLUMN.
--
--
-- THE TABLE OF MEANINGS, MEASURED FROM THE WRITERS
--
-- 0128 carries the full table. This file changes two rows of it and adds one
-- table. The other rows are unchanged.
--
--   applications.owner_id              PROJECT  (was DISPUTED)
--     The legacy runtime owns the table and overwrites the payload before the
--     model validates: `raw["owner_id"] = project_id`
--     (legacy/plugins/elitea_core/api/v2/applications.py:163), with the prose
--     one line above: "owner_id is current project ID". Every legacy read
--     agrees: `Application.owner_id == project_id`
--     (api/v2/recommendations.py:160,201) and
--     `Application.owner_id == public_project_id`
--     (methods/admin_tasks.py:1746). This service agrees on the read side too:
--     Fork copies an exported `owner_id` into `forkMeta["parent_project_id"]`
--     (internal/api/v2/eliteacore/handler.go).
--     The writers in this service were wrong, not the meaning. A user id here
--     makes the agent invisible to every legacy read, because the legacy filter
--     compares the column to the project. This file repairs the rows and the
--     same change corrects the writers:
--     internal/infra/db/repos/applications.go Create, and the import and fork
--     paths in internal/api/v2/eliteacore/handler.go.
--
--   prompt_collections.owner_id        PROJECT  (0128 stated no meaning)
--     `data["owner_id"], data["author_id"] = project_id, author_id`
--     (legacy/plugins/elitea_core/api/v2/collections.py:105, removed from the
--     legacy runtime by commit f81cdad). The create helper agrees:
--     `"owner_id": project_id` (utils/collections.py:220), and the read takes
--     the project back out of it: `project_id = data['owner_id']`
--     (utils/collections.py:421).
--
--   prompt_collections.author_id       USER
--     The same line binds `author_id` to `auth.current_user()["id"]`
--     (api/v2/collections.py:104). The listing resolves the column through
--     `get_authors_data([i.author_id for i in collections])`
--     (api/v2/collections.py:54), which reads users.
--
--   skills.owner_id                    PROJECT  (unchanged, now constrained)
--     `raw["owner_id"] = project_id` (elitea_core/api/v2/skills.py:95), and
--     reads filter with `Skill.owner_id == project_id`
--     (utils/skill_utils.py:1066). Rows written before #533 can hold the
--     literal 1, so this file repairs them as well.
--
-- USER-kind columns keep their 0128 comments and get no constraint here. The
-- section "WHY THE USER COLUMNS GET NO FOREIGN KEY" below states why.
--
--
-- WHY prompt_collections IS HERE AT ALL
--
-- No migration in this repository creates the table, and the legacy runtime
-- carries an admin task that DROPS it (methods/admin_tasks.py:2481-2566). A
-- deployment that ran that task has no such table, and every statement below
-- is behind a `to_regclass` probe for that reason.
--
-- The table still needs the record, because this service WRITES to it:
-- internal/api/v2/eliteacore/handler.go CreateCollection inserts a row on a
-- registered route. That writer put the caller user id into BOTH columns with
-- `VALUES ($1, $2, $3, $3, ...)`. The same change corrects it.
--
--
-- WHY A FOREIGN KEY IS POSSIBLE NOW, WHEN 0128 REFUSED ONE
--
-- 0128 refused the foreign key because `centry.project` is a pylon-owned table.
-- The refusal was measured against the wrong risk. The table is present in
-- every database this repository can migrate: the tenant runner refuses to
-- apply anything until it reads the project row
-- (internal/infra/db/migrate/runner.go ApplyTenant), and the bootstrap schema
-- creates the table for a standalone deployment
-- (internal/infra/db/migrations/001_initial.sql). A corpus-only database
-- therefore has both the parent table and the parent row, so the constraint is
-- exercised where it is declared. It is not a constraint that only exists in
-- production.
--
-- The constraint states the KIND of number, not which one. A project id that
-- names a project is accepted; a number that names no project is refused with
-- SQLSTATE 23503. The one correct number per schema is a stricter rule, and it
-- stays where it can be reported to a caller as a 400 rather than as a
-- constraint violation: internal/infra/db/tenantschema/owner.go OwnerID returns
-- the project of the schema, and internal/domain/ownership types it as
-- ProjectID so that a UserID cannot be passed in its place.
--
-- ON DELETE CASCADE, and why it is not the loss of a record. Project delete
-- removes the project row FIRST and drops the tenant schema after
-- (internal/application/projectprovisioning/deprovision.go, issue #374). The
-- order is deliberate and must not change. A foreign key with NO ACTION would
-- make that delete fail for every project that holds one agent. A cascade
-- deletes rows that the next statement drops with the schema, so it destroys
-- nothing that would otherwise survive. This is the opposite of the runtime
-- attestation tables, which a cascade would silently destroy; those keep their
-- explicit deletes in projectprovisioning.referencingDeletes.
--
-- eval_dimensions. tenant/0130 gave `eval_dimensions.application_id` a foreign
-- key to `applications(id)` with NO ACTION. Two defects follow from it once
-- applications carries a cascade: an agent with a dimension cannot be deleted
-- at all (SQLSTATE 23503 on DELETE FROM applications), and a project delete
-- stops on the same row. This file replaces that constraint with the same
-- foreign key ON DELETE CASCADE. A dimension describes one agent, so it goes
-- with the agent.
--
--
-- WHY THE USER COLUMNS GET NO FOREIGN KEY
--
-- A USER-kind column would reference `public.auth_core__user`. That reference
-- would break two shipped behaviours:
--
--   * The admin panel deletes users (`DELETE FROM public.auth_core__user WHERE
--     id = ANY($1)`, internal/api/v2/admin/users.go). With NO ACTION the delete
--     fails for every user who owns a chat folder or authored a version. With
--     ON DELETE CASCADE it destroys that user's agents and versions, which is
--     a data loss the panel does not offer.
--   * Writers still store the literal 1 where no principal is resolved
--     (internal/infra/db/repos/folders.go Create). Issue #505 owns that
--     fallback, and this file does not change it. A foreign key would turn
--     that row into a 500 on a deployment whose user 1 was removed.
--
-- So the USER-kind columns keep the 0128 record and the write-side rule. The
-- refusal proved here is on the PROJECT-kind columns.
--
--
-- MECHANICS
--
-- All tenant table names are UNQUALIFIED. The tenant runner pins a
-- transaction-local search_path to the tenant schema and verifies it before any
-- file runs. `centry.project` is qualified, because that search_path holds the
-- tenant schema alone.
--
-- The project of the schema comes from the schema NAME. The runner builds the
-- name from the project id it verified, so the two cannot disagree. A schema
-- that is not `p_<digits>` is a failure and not a skip.
--
-- Each table is probed for the TABLE and for the COLUMN, as in 0128: a legacy
-- schema and a corpus-only schema hold different subsets of these tables.
--
-- IDEMPOTENT: the repair is an UPDATE with a predicate, every ADD CONSTRAINT is
-- behind an existence probe, and COMMENT ON replaces the comment it finds.
--
-- No BEGIN/COMMIT: the ledgered runner executes each file inside one
-- transaction with its ledger row (migrate/runner.go apply).
DO $$
DECLARE
    project_id integer;
    entry      record;
    repaired   bigint;
BEGIN
    project_id := NULLIF(substring(current_schema() FROM '^p_([0-9]+)$'), '')::integer;
    IF project_id IS NULL THEN
        RAISE EXCEPTION
            '0131: the effective schema % is not a tenant schema, so the project of this schema is unknown',
            current_schema();
    END IF;

    FOR entry IN
        SELECT *
          FROM (VALUES
            ('applications',
             'applications_owner_project_fkey',
             'PROJECT id, not user id. It holds the project of this schema. The legacy runtime sets it to the route project (elitea_core/api/v2/applications.py:163) and Fork reads it as parent_project_id. Settled by migrations/tenant/0131 (issue #533): the rows were repaired, the writers now use tenantschema.OwnerID, and a number that names no project is refused. Do not join this column to a user table; the version author is application_versions.author_id.'),
            ('skills',
             'skills_owner_project_fkey',
             'PROJECT id, not user id. It holds the project of this schema. The legacy runtime sets it to the route project (elitea_core/api/v2/skills.py:95) and reads filter Skill.owner_id = project_id. Rows that held the literal 1 were repaired by migrations/tenant/0131. The user is skills.author_id.'),
            ('prompt_collections',
             'prompt_collections_owner_project_fkey',
             'PROJECT id, not user id. The legacy runtime set both columns in one statement: data["owner_id"], data["author_id"] = project_id, author_id (elitea_core/api/v2/collections.py:105). Settled by migrations/tenant/0131 (issue #533). The user is prompt_collections.author_id.')
          ) AS t(table_name, constraint_name, meaning)
    LOOP
        IF to_regclass(entry.table_name) IS NULL THEN
            CONTINUE;
        END IF;
        IF NOT EXISTS (
            SELECT 1
              FROM pg_attribute
             WHERE attrelid = to_regclass(entry.table_name)
               AND attname = 'owner_id'
               AND attnum > 0
               AND NOT attisdropped
        ) THEN
            CONTINUE;
        END IF;

        -- 1. REPAIR. A PROJECT-kind owner_id has exactly one correct value in
        -- schema p_<id>, because "the project that owns the row" and "the
        -- project whose schema holds the row" are the same project. Any other
        -- number is a user id, a stale literal 1, or a project that does not
        -- own the row. All three are repaired to the same value, and the row
        -- itself is kept.
        EXECUTE format(
            'UPDATE %I SET owner_id = $1 WHERE owner_id IS DISTINCT FROM $1',
            entry.table_name) USING project_id;
        GET DIAGNOSTICS repaired = ROW_COUNT;
        IF repaired > 0 THEN
            RAISE NOTICE '0131: % rows repaired in %.%; owner_id now names project %',
                repaired, current_schema(), entry.table_name, project_id;
        END IF;

        -- The index that the cascade reads. A delete of the project row scans
        -- this column in every tenant schema, so the column needs its own
        -- index; the primary key does not answer that question.
        EXECUTE format(
            'CREATE INDEX IF NOT EXISTS %I ON %I (owner_id)',
            entry.table_name || '_owner_id_idx', entry.table_name);

        -- 2. CONSTRAIN.
        IF to_regclass('centry.project') IS NULL THEN
            RAISE NOTICE '0131: centry.project is absent, so %.owner_id keeps the comment as its only record',
                entry.table_name;
        -- The probe reads the COLUMN and not only the constraint name, as 0130
        -- does. A deployment whose runtime already put a foreign key on this
        -- column must keep one constraint and not collect a second one under a
        -- different name.
        ELSIF NOT EXISTS (
            SELECT 1
              FROM pg_constraint AS constraint_
              JOIN pg_attribute AS attribute
                ON attribute.attrelid = constraint_.conrelid
               AND attribute.attnum = ANY (constraint_.conkey)
             WHERE constraint_.conrelid = to_regclass(entry.table_name)
               AND constraint_.contype = 'f'
               AND attribute.attname = 'owner_id'
        ) THEN
            BEGIN
                EXECUTE format(
                    'ALTER TABLE %I ADD CONSTRAINT %I FOREIGN KEY (owner_id) REFERENCES centry.project(id) ON DELETE CASCADE',
                    entry.table_name, entry.constraint_name);
            EXCEPTION
                WHEN insufficient_privilege THEN
                    RAISE NOTICE '0131: centry.project belongs to another role, so %.owner_id keeps the comment as its only record',
                        entry.table_name;
            END;
        END IF;

        -- 3. RECORD.
        BEGIN
            EXECUTE format('COMMENT ON COLUMN %I.owner_id IS %L',
                           entry.table_name, entry.meaning);
        EXCEPTION
            WHEN insufficient_privilege THEN
                RAISE NOTICE '0131: %.owner_id belongs to another role, comment skipped; this file stays the record',
                    entry.table_name;
        END;
    END LOOP;

    -- prompt_collections.author_id is the USER half of the same statement. It
    -- gets the record, and no constraint, for the reason above.
    IF to_regclass('prompt_collections') IS NOT NULL AND EXISTS (
        SELECT 1
          FROM pg_attribute
         WHERE attrelid = to_regclass('prompt_collections')
           AND attname = 'author_id'
           AND attnum > 0
           AND NOT attisdropped
    ) THEN
        BEGIN
            COMMENT ON COLUMN prompt_collections.author_id IS
                'USER id. The principal that created the collection. The legacy runtime read it back through get_authors_data (elitea_core/api/v2/collections.py:54). owner_id on the same table is a PROJECT: see migrations/tenant/0131 and issue #533.';
        EXCEPTION
            WHEN insufficient_privilege THEN
                RAISE NOTICE '0131: prompt_collections.author_id belongs to another role, comment skipped';
        END;
    END IF;

    -- eval_dimensions keeps its foreign key to applications and loses NO
    -- ACTION. See "ON DELETE CASCADE" above for both defects this removes.
    IF to_regclass('eval_dimensions') IS NOT NULL AND EXISTS (
        SELECT 1
          FROM pg_constraint
         WHERE conrelid = to_regclass('eval_dimensions')
           AND contype = 'f'
           AND conname = 'eval_dimensions_application_id_fkey'
           AND confdeltype <> 'c'
    ) THEN
        ALTER TABLE eval_dimensions DROP CONSTRAINT eval_dimensions_application_id_fkey;
        ALTER TABLE eval_dimensions
            ADD CONSTRAINT eval_dimensions_application_id_fkey
            FOREIGN KEY (application_id) REFERENCES applications(id) ON DELETE CASCADE;
    END IF;
END $$;
