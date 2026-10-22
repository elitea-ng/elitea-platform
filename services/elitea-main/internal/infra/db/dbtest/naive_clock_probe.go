package dbtest

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// InstallNaiveClockProbe installs NaiveClockProbeSQL on pool's database. A
// harness that builds its schema by hand (001_initial.sql, a migration file, a
// table copied into the test) rather than through EnsureTemplate calls this
// once its base schema exists. Tables created or altered afterwards are
// attached by the probe's event trigger (see NaiveClockProbeSQL).
func InstallNaiveClockProbe(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx, NaiveClockProbeSQL); err != nil {
		return fmt.Errorf("dbtest: install the naive clock probe: %w", err)
	}
	return nil
}

// NaiveClockProbeSQL installs, on every table that has a `timestamp` WITHOUT
// time zone column, a row trigger that refuses a write which stored the
// SESSION-local wall clock instead of the UTC wall clock.
//
// The naive columns of this schema hold the UTC wall clock (the legacy runtime
// wrote them with server_default now() in UTC containers, and the web client
// reads them as UTC). A writer that puts a bare now()/clock_timestamp() into
// one, or that leaves it to a column DEFAULT now(), stores the session's local
// time. Every test session runs in a non-UTC zone (internal/testpg), so such a
// value sits within a few minutes of the session-local clock and the UTC
// offset away from the UTC clock. That is the one signature the trigger
// refuses; a value deliberately dated in the past or the future is neither
// close to "now" in the session zone nor refused. On UPDATE only a column
// that changed is checked, so carrying an old value forward passes.
//
// The probe is a behavioural guard over every writer the suite drives, not a
// list of writers: a writer added later without AT TIME ZONE 'UTC' fails the
// first test that runs it against a template database. A writer no test
// drives is not covered, which is the remaining gap.
//
// Tables created or altered after the install (create_tenant_schema, a
// tenant migration, a test that recreates its table, a column added by ALTER
// TABLE, DDL inside plpgsql EXECUTE) are attached by a ddl_command_end event
// trigger, which re-reads the table's naive columns and recreates its row
// trigger. Event triggers belong to the database, so a template clone keeps
// it. Creating one needs a superuser; without one the install still succeeds
// and only the tables present at install time are watched.
const NaiveClockProbeSQL = `
CREATE SCHEMA IF NOT EXISTS elitea_test_probe;

CREATE OR REPLACE FUNCTION elitea_test_probe.refuse_session_local_clock() RETURNS trigger
LANGUAGE plpgsql AS $probe$
DECLARE
    zone text := current_setting('TimeZone');
    utc_now timestamp := clock_timestamp() AT TIME ZONE 'UTC';
    local_now timestamp := clock_timestamp() AT TIME ZONE zone;
    tolerance interval := interval '2 minutes';
    column_name text;
    new_value timestamp;
    old_value timestamp;
BEGIN
    IF current_setting('elitea.naive_clock_probe', true) = 'off' THEN
        RETURN NEW;
    END IF;
    -- A zone with no offset from UTC right now cannot tell the two apart.
    IF abs(extract(epoch FROM local_now - utc_now)) < extract(epoch FROM tolerance) * 2 THEN
        RETURN NEW;
    END IF;
    FOREACH column_name IN ARRAY TG_ARGV LOOP
        EXECUTE format('SELECT ($1).%I::timestamp', column_name) INTO new_value USING NEW;
        CONTINUE WHEN new_value IS NULL;
        IF TG_OP = 'UPDATE' THEN
            EXECUTE format('SELECT ($1).%I::timestamp', column_name) INTO old_value USING OLD;
            CONTINUE WHEN old_value IS NOT DISTINCT FROM new_value;
        END IF;
        IF abs(extract(epoch FROM new_value - local_now)) <= extract(epoch FROM tolerance)
           AND abs(extract(epoch FROM new_value - utc_now)) > extract(epoch FROM tolerance) THEN
            RAISE EXCEPTION
                'naive timestamp %.%.% = % is the session-local clock (TimeZone %), not the UTC wall clock %',
                TG_TABLE_SCHEMA, TG_TABLE_NAME, column_name, new_value, zone, utc_now
                USING ERRCODE = 'P0T01',
                      HINT = 'write now()/clock_timestamp() AT TIME ZONE ''UTC'', and name the column instead of leaving it to DEFAULT now()';
        END IF;
    END LOOP;
    RETURN NEW;
END
$probe$;

-- attach (re)creates the row trigger on one table with its CURRENT naive
-- columns, or drops it when the table has none left.
CREATE OR REPLACE FUNCTION elitea_test_probe.attach(target regclass) RETURNS void
LANGUAGE plpgsql AS $attach$
DECLARE
    columns text;
BEGIN
    -- A partition inherits its parent's row trigger (and may not drop it).
    IF NOT EXISTS (SELECT 1 FROM pg_class
                   WHERE oid = target AND relkind IN ('r', 'p') AND NOT relispartition) THEN
        RETURN;
    END IF;
    SELECT string_agg(quote_literal(a.attname::text), ', ' ORDER BY a.attname)
      INTO columns
      FROM pg_attribute AS a
     WHERE a.attrelid = target
       AND a.attnum > 0
       AND NOT a.attisdropped
       AND a.atttypid = 'timestamp'::regtype;
    IF EXISTS (SELECT 1 FROM pg_trigger
               WHERE tgrelid = target AND tgname = 'zz_elitea_naive_clock_probe') THEN
        EXECUTE format('DROP TRIGGER zz_elitea_naive_clock_probe ON %s', target);
    END IF;
    IF columns IS NOT NULL THEN
        EXECUTE format(
            'CREATE TRIGGER zz_elitea_naive_clock_probe BEFORE INSERT OR UPDATE ON %s '
            'FOR EACH ROW EXECUTE FUNCTION elitea_test_probe.refuse_session_local_clock(%s)',
            target, columns);
    END IF;
END
$attach$;

DO $install$
DECLARE
    target regclass;
BEGIN
    FOR target IN
        SELECT DISTINCT a.attrelid::regclass
        FROM pg_attribute AS a
        JOIN pg_class AS c ON c.oid = a.attrelid AND c.relkind IN ('r', 'p') AND NOT c.relispartition
        JOIN pg_namespace AS n ON n.oid = c.relnamespace
        WHERE a.attnum > 0 AND NOT a.attisdropped
          AND a.atttypid = 'timestamp'::regtype
          AND n.nspname NOT IN ('pg_catalog', 'information_schema', 'elitea_test_probe')
          AND n.nspname NOT LIKE 'pg_toast%'
    LOOP
        PERFORM elitea_test_probe.attach(target);
    END LOOP;
END
$install$;

-- A table created or altered AFTER the install (create_tenant_schema, a
-- tenant migration, a test that drops and recreates its table) is attached
-- by this event trigger, so the probe watches the schema as it is when the
-- write happens, not as it was when the harness installed it.
CREATE OR REPLACE FUNCTION elitea_test_probe.attach_changed_tables() RETURNS event_trigger
LANGUAGE plpgsql AS $changed$
DECLARE
    changed record;
BEGIN
    FOR changed IN
        SELECT DISTINCT command.objid
        FROM pg_event_trigger_ddl_commands() AS command
        WHERE command.classid = 'pg_class'::regclass
          AND command.object_type = 'table'
          AND command.schema_name IS DISTINCT FROM 'elitea_test_probe'
    LOOP
        -- The trigger DDL attach issues fires this event again; the table is
        -- already attached by then, and CREATE TRIGGER is not in the filter.
        IF EXISTS (SELECT 1 FROM pg_class WHERE oid = changed.objid) THEN
            PERFORM elitea_test_probe.attach(changed.objid::regclass);
        END IF;
    END LOOP;
END
$changed$;

DO $event$
BEGIN
    DROP EVENT TRIGGER IF EXISTS zz_elitea_naive_clock_probe_ddl;
    CREATE EVENT TRIGGER zz_elitea_naive_clock_probe_ddl ON ddl_command_end
        WHEN TAG IN ('CREATE TABLE', 'ALTER TABLE', 'CREATE TABLE AS', 'SELECT INTO')
        EXECUTE FUNCTION elitea_test_probe.attach_changed_tables();
EXCEPTION WHEN insufficient_privilege THEN
    -- Event triggers need a superuser. Without one the probe still watches
    -- every table that existed at install time.
    RAISE NOTICE 'naive clock probe: no event trigger (insufficient privilege); tables created later are not watched';
END
$event$;
`
