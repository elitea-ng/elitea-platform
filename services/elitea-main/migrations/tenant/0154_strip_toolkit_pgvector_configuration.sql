-- Remove the retired `pgvector_configuration` from every toolkit's stored
-- `settings` (#1359).
--
-- WHAT IS THERE. A toolkit with an index named the PgVector connection its
-- vectors lived in, as `settings.pgvector_configuration` (a reference to a
-- `pgvector` configuration, sometimes with a private inline value). The
-- per-project PgVector store is gone: toolkit indexes are kept by
-- elitea-vector (ADR-0031) and their metadata by the index registry
-- (ADR-0030), so no runtime path reads the key. S4a (configurations/retired.go)
-- already drops it on every toolkit read and write; this file removes it from
-- the rows too, so an export, a backup or a direct SQL reader sees what the API
-- returns, and the read-side strip stops being the only thing keeping it out.
--
-- WHAT THIS DOES. Deletes the key, and nothing else. A row whose `settings` is
-- not a JSON object is left alone: the `-` operator means something else on an
-- array, and raises on a scalar.
--
-- WHAT IT DOES NOT DO. It does not delete the `pgvector` configuration rows
-- the key pointed at, nor their vault secrets: retired.go keeps those on
-- purpose (an operator removes them with DELETE by id), and they live in the
-- configurations tables, not here.
--
-- The column is `jsonb` in the sqlc projection (internal/db/schema/
-- toolkit_baseline.sql), but a database carried over from the legacy runtime
-- may hold it as `json`, so the type is read first and the matching statement
-- runs (0143's pattern).
--
-- Data only: idempotent, no table and no permission, so no shared sibling.
-- `elitea_tools` is not created by this repository's tenant chain (0124), so a
-- tenant schema without the table, or without the column, is skipped.
DO $$
DECLARE
    column_type text;
BEGIN
    IF to_regclass('elitea_tools') IS NULL THEN
        RETURN;
    END IF;

    SELECT format_type(attribute.atttypid, attribute.atttypmod)
      INTO column_type
      FROM pg_attribute AS attribute
     WHERE attribute.attrelid = to_regclass('elitea_tools')
       AND attribute.attname = 'settings'
       AND NOT attribute.attisdropped;

    IF column_type = 'jsonb' THEN
        UPDATE elitea_tools
           SET settings = settings - 'pgvector_configuration'
         WHERE jsonb_typeof(settings) = 'object'
           AND settings ? 'pgvector_configuration';
    ELSIF column_type = 'json' THEN
        UPDATE elitea_tools
           SET settings = (settings::jsonb - 'pgvector_configuration')::json
         WHERE json_typeof(settings) = 'object'
           AND settings::jsonb ? 'pgvector_configuration';
    END IF;
END
$$;
