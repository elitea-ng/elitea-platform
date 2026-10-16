package dbtest

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// InstallNaiveClockProbe installs NaiveClockProbeSQL on pool's database. A
// harness that builds its schema by hand (001_initial.sql, a migration file, a
// table copied into the test) rather than through EnsureTemplate calls this
// after its LAST schema statement: the probe attaches to the tables that exist
// when it runs, so a table created afterwards is not watched.
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
// The naive columns of this schema hold the UTC wall clock (pylon wrote them
// with server_default now() in UTC containers, and the web client reads them
// as UTC). A writer that puts a bare now()/clock_timestamp() into one, or that
// leaves it to a column DEFAULT now(), stores the session's local time.
// Every test session runs in a non-UTC zone (internal/testpg), so such a
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

DO $install$
DECLARE
    target record;
BEGIN
    FOR target IN
        SELECT c.table_schema, c.table_name, array_agg(c.column_name::text ORDER BY c.column_name) AS columns
        FROM information_schema.columns AS c
        JOIN information_schema.tables AS t
          ON t.table_schema = c.table_schema AND t.table_name = c.table_name AND t.table_type = 'BASE TABLE'
        WHERE c.data_type = 'timestamp without time zone'
          AND c.table_schema NOT IN ('pg_catalog', 'information_schema', 'elitea_test_probe')
        GROUP BY c.table_schema, c.table_name
    LOOP
        EXECUTE format(
            'CREATE TRIGGER zz_elitea_naive_clock_probe BEFORE INSERT OR UPDATE ON %I.%I '
            'FOR EACH ROW EXECUTE FUNCTION elitea_test_probe.refuse_session_local_clock(%s)',
            target.table_schema, target.table_name,
            (SELECT string_agg(quote_literal(name), ', ') FROM unnest(target.columns) AS name));
    END LOOP;
END
$install$;
`
