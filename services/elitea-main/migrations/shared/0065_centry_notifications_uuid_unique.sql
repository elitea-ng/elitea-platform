-- 0065_centry_notifications_uuid_unique.sql — the constraint an ON CONFLICT names.
--
-- Same defect class as 0064, one level finer: not a missing table, a missing
-- UNIQUE. `queries/runtime_index_ingest.sql:295` ends the index-ingest terminal
-- notification insert with
--
--     ON CONFLICT (uuid) DO NOTHING
--
-- and `internal/db/schema/notifications_baseline.sql:11` declares
-- `uuid uuid NOT NULL UNIQUE`, which is what let it compile. But
-- `001_initial.sql:125-135` creates centry.notifications with
-- `uuid UUID NOT NULL DEFAULT gen_random_uuid()` and no uniqueness at all, so on
-- a deployment built from this repository's own schema PostgreSQL rejects the
-- statement outright:
--
--     ERROR: there is no unique or exclusion constraint matching the
--            ON CONFLICT specification (SQLSTATE 42P10)
--
-- MEASURED on the standalone stack (#93 Surface A). The consequence is worse
-- than a lost notification row, because that INSERT runs inside the transaction
-- that persists the index run's TERMINAL output frame:
--
--   runtime output frame rejected  event_type=..._INDEX_INGEST_RESULT
--   terminal=true error_code=RUNTIME_ERROR_CODE_V1_INTERNAL
--   cause="project index ingest: persist current index terminal notification: ..."
--
-- The whole terminal projection rolls back, so the execution-events stream never
-- emits `index.ingest.completed` (nor `execution.failed`) and the browser's
-- EventSource waits forever on a run that has in fact finished. The worker is
-- told the dependency is unavailable and retries, so it repeats indefinitely.
-- Every non-terminal node event persists normally, which is why the run looks
-- alive right up to the point where it should end.
--
-- Why no test caught it: the postgres integration harness creates its own
-- centry.notifications WITH the unique
-- (repos/configuration_validation_postgres_integration_test.go:761-771), so it
-- type-checks and executes against a shape the migrations never produce.
--
-- A unique INDEX rather than a table constraint, and guarded rather than
-- unconditional: in a legacy-backed deployment the column already carries a
-- uniqueness constraint under its own name, and this migration must leave that
-- one alone. ON CONFLICT (uuid) is satisfied by either.
--
-- The to_regclass guard is not ceremony: a bare `'centry.notifications'::regclass`
-- RAISES on a database where the table is absent, which would turn this
-- migration into a hard failure on exactly the deployments it has nothing to say
-- to.
DO $$
BEGIN
    IF to_regclass('centry.notifications') IS NULL THEN
        RETURN;
    END IF;
    IF NOT EXISTS (
        SELECT 1
        FROM pg_index AS index_row
        JOIN pg_class AS index_class ON index_class.oid = index_row.indexrelid
        WHERE index_row.indrelid = 'centry.notifications'::regclass
          AND index_row.indisunique
          AND index_row.indnatts = 1
          AND index_row.indkey[0] = (
              SELECT attnum FROM pg_attribute
              WHERE attrelid = 'centry.notifications'::regclass
                AND attname = 'uuid'
          )
    ) THEN
        CREATE UNIQUE INDEX notifications_uuid_key
            ON centry.notifications (uuid);
    END IF;
END
$$;
