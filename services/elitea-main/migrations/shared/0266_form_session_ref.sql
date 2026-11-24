-- 0266_form_session_ref.sql — a stable, non-secret reference for every Form
-- browser session (ADR-0059 decision 7, the credentials table, and decision
-- 5 "Bound to one session"; story R1.2a, issue 2506).
--
-- The single sign-on planes name a session by elitea_auth.browser_sessions.ref
-- (shared 0200): the row id is the cookie value and never leaves the table,
-- `ref` is what audit events, runs (execution_jobs.browser_session_ref) and,
-- from R2.1, an elevation name it by. A Form session (elitea_auth.form_sessions,
-- shared 0153) had nothing of the kind, so a Form principal reached the
-- permission resolver with a zero reference and could never hold an
-- elevation bound to its session.
--
--   elitea_auth.form_sessions.ref
--       a server-assigned bigint, drawn from the SAME sequence as
--       browser_sessions.ref, so the two kinds of row never share a number
--       and execution_jobs.browser_session_ref names one session whichever
--       plane wrote it. A row keeps its ref for its life; a rotation writes a
--       new row, and the new row draws a new ref.
--
-- Existing rows get a value now (ADD COLUMN with a volatile default fills
-- every row). A replica of the previous release does not name the column,
-- so its inserts take the default too during a rolling upgrade.
--
-- GUARD. Without the browser_sessions identity (a database the shared corpus
-- runs on without 0200) the column draws from a sequence of its own.
-- Additive only. Idempotent. No BEGIN/COMMIT: the ledgered runner wraps the
-- file.

DO $$
DECLARE
    sequence_name text;
BEGIN
    IF to_regclass('elitea_auth.form_sessions') IS NULL THEN
        RAISE NOTICE '0266: elitea_auth.form_sessions absent, no Form session reference to add';
        RETURN;
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_attribute
        WHERE attrelid = 'elitea_auth.form_sessions'::regclass
          AND attname = 'ref' AND attnum > 0 AND NOT attisdropped
    ) THEN
        IF to_regclass('elitea_auth.browser_sessions') IS NOT NULL THEN
            sequence_name := pg_get_serial_sequence('elitea_auth.browser_sessions', 'ref');
        END IF;
        IF sequence_name IS NULL THEN
            CREATE SEQUENCE IF NOT EXISTS elitea_auth.form_sessions_ref_seq AS bigint;
            sequence_name := 'elitea_auth.form_sessions_ref_seq';
        END IF;
        EXECUTE format(
            'ALTER TABLE elitea_auth.form_sessions ADD COLUMN ref bigint NOT NULL DEFAULT nextval(%L::regclass)',
            sequence_name);
    END IF;

    CREATE UNIQUE INDEX IF NOT EXISTS form_sessions_ref_key
        ON elitea_auth.form_sessions (ref);

    COMMENT ON COLUMN elitea_auth.form_sessions.ref IS
        'Stable non-secret reference of this Form session, from the browser_sessions.ref sequence (ADR-0059 R1.2a). What runs, audit events and elevations name the session by; never the cookie value.';
END
$$;
