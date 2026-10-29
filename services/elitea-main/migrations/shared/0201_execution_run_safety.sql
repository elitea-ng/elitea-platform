-- 0201_execution_run_safety.sql — what admission records for the runtime
-- safety controls (ADR-0041 decisions 1 and 3, ADR-0042 decision 5; issues
-- #1496 and #1498).
--
-- elitea_runtime.execution_jobs gains four nullable columns:
--
--   toolkit_types        the canonical toolkit types (guardrails.CanonicalKey)
--                        the run was given at admission: an agent run's
--                        frozen toolkit set, its sub-agents' included; a
--                        toolkit run's own type. A toolkit_type suspension
--                        cancels the in-flight runs whose set contains its
--                        key, in the transaction that saves it. NULL for a
--                        run admitted before 0201 and for a run kind that
--                        names no toolkit; the suspension reads an agent
--                        run's NULL set from its stored input once and writes
--                        it back (repos.backfillToolkitTypes).
--   browser_session_ref  the browser session (browser_sessions.ref, shared
--                        0200) whose request admitted the run. Never the
--                        session id: that is the cookie value. NULL for a
--                        run started by a token, a schedule or a trigger.
--                        Joins a run to the session's audit events.
--   run_started_at       when the WHOLE run started. NULL means admitted_at.
--                        A continuation (a resumed pause, a continued output)
--                        is a new execution of the same response; it copies
--                        the start of the execution it continues, so the
--                        whole-run wall cap counts from the original start.
--   cancel_reason        why Main cancelled the run, when the reason has a
--                        typed outcome: cap_reached (the whole-run wall cap,
--                        run_caps.max_wall_seconds). The projection shows a
--                        cancelled run with this reason as CAP_REACHED.
--
-- No index on toolkit_types: a suspension is rare and filters the
-- non-terminal rows first. Additive only. Idempotent. No BEGIN/COMMIT: the
-- ledgered runner wraps the file.

ALTER TABLE elitea_runtime.execution_jobs
    ADD COLUMN IF NOT EXISTS toolkit_types text[],
    ADD COLUMN IF NOT EXISTS browser_session_ref bigint,
    ADD COLUMN IF NOT EXISTS run_started_at timestamptz,
    ADD COLUMN IF NOT EXISTS cancel_reason text;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'execution_jobs_cancel_reason_known'
          AND conrelid = 'elitea_runtime.execution_jobs'::regclass
    ) THEN
        ALTER TABLE elitea_runtime.execution_jobs
            ADD CONSTRAINT execution_jobs_cancel_reason_known
            CHECK (cancel_reason IS NULL OR cancel_reason IN ('cap_reached'));
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'execution_jobs_browser_session_ref_positive'
          AND conrelid = 'elitea_runtime.execution_jobs'::regclass
    ) THEN
        ALTER TABLE elitea_runtime.execution_jobs
            ADD CONSTRAINT execution_jobs_browser_session_ref_positive
            CHECK (browser_session_ref IS NULL OR browser_session_ref > 0);
    END IF;
END
$$;

-- The audit join: the runs one session started.
CREATE INDEX IF NOT EXISTS execution_jobs_browser_session_ref
    ON elitea_runtime.execution_jobs (browser_session_ref)
    WHERE browser_session_ref IS NOT NULL;
