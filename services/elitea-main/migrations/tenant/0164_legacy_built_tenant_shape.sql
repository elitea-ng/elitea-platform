-- 0164_legacy_built_tenant_shape.sql — bring a tenant schema the legacy
-- runtime built up to the shape create_tenant_schema declares, for the
-- objects Main reads.
--
-- create_tenant_schema (internal/infra/db/migrations/001_initial.sql) uses
-- CREATE TABLE IF NOT EXISTS, so a schema the legacy runtime built keeps its
-- older shape. Measured on the real-model dump (p_1 and p_2, against a fresh
-- p_1 and the native p_118/p_119), Main reads three columns and two tables
-- such a schema lacks:
--
--   elitea_tools.owner_id   written by toolkit create, fork and import and by
--                           an agent or pipeline attached as a tool
--                           (toolkits/handler.go createToolkitInsertSQL and
--                           fork, eliteacore/import_owner.go,
--                           eliteacore/application_relation.go)
--   elitea_tools.env_vars   read by toolkit export and copied by fork
--   elitea_tools.uuid       declared UNIQUE with a generated default
--   index_meta              the toolkit index pages (toolkits/handler.go,
--                           toolkits/index_write.go)
--   social_likes            the agent list's liked and trending filters and
--                           like counts (repos/applications.go,
--                           eliteacore/handler.go, public_applications.go)
--
-- Each is declared exactly as 001_initial.sql declares it (elitea_tools at
-- :536-548, social_likes at :856, index_meta at :922). The tenant social_pins
-- and social_feedbacks are not created: Main keeps those rows in centry.
--
-- owner_id means the PROJECT (0128), so the backfill is this schema's project,
-- taken from the schema name as in 0131. A legacy-built toolkit had no owner
-- and was already visible to the whole project; the backfill names that same
-- project and widens nothing.
--
-- A schema that already has a column or table (every schema Main built)
-- records this migration and changes nothing: each column is added only when
-- it is absent, and each table with IF NOT EXISTS.
DO $$
DECLARE
    v_project_id integer;
BEGIN
    IF to_regclass('elitea_tools') IS NOT NULL THEN
        IF NOT EXISTS (
            SELECT 1 FROM information_schema.columns
             WHERE table_schema = current_schema()
               AND table_name = 'elitea_tools'
               AND column_name = 'owner_id'
        ) THEN
            v_project_id := NULLIF(substring(current_schema() FROM '^p_([0-9]+)$'), '')::integer;
            IF v_project_id IS NULL THEN
                RAISE EXCEPTION
                    '0164: the effective schema % is not a tenant schema, so the project of this schema is unknown',
                    current_schema();
            END IF;
            ALTER TABLE elitea_tools ADD COLUMN owner_id INTEGER;
            UPDATE elitea_tools SET owner_id = v_project_id;
            ALTER TABLE elitea_tools ALTER COLUMN owner_id SET NOT NULL;
        END IF;

        IF NOT EXISTS (
            SELECT 1 FROM information_schema.columns
             WHERE table_schema = current_schema()
               AND table_name = 'elitea_tools'
               AND column_name = 'env_vars'
        ) THEN
            ALTER TABLE elitea_tools ADD COLUMN env_vars JSONB DEFAULT '{}'::jsonb;
        END IF;

        IF NOT EXISTS (
            SELECT 1 FROM information_schema.columns
             WHERE table_schema = current_schema()
               AND table_name = 'elitea_tools'
               AND column_name = 'uuid'
        ) THEN
            ALTER TABLE elitea_tools ADD COLUMN uuid UUID UNIQUE DEFAULT gen_random_uuid();
        END IF;

        CREATE TABLE IF NOT EXISTS index_meta (
            id SERIAL PRIMARY KEY,
            toolkit_id INTEGER NOT NULL REFERENCES elitea_tools(id) ON DELETE CASCADE,
            name VARCHAR(128) NOT NULL,
            status VARCHAR(64) NOT NULL DEFAULT 'created',
            progress DOUBLE PRECISION NOT NULL DEFAULT 0,
            meta JSONB DEFAULT '{}'::jsonb,
            created_at TIMESTAMP NOT NULL DEFAULT now()
        );
    END IF;

    CREATE TABLE IF NOT EXISTS social_likes (
        id SERIAL PRIMARY KEY,
        entity_name VARCHAR NOT NULL,
        entity_id INTEGER NOT NULL,
        user_id INTEGER NOT NULL,
        created_at TIMESTAMP NOT NULL DEFAULT now(),
        CONSTRAINT _like_unique UNIQUE (entity_name, entity_id, user_id)
    );
END
$$;
