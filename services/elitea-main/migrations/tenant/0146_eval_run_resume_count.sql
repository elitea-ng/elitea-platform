-- Demo issue 4 follow-up: count how many times the recovery sweep resumed an
-- evaluation run whose process died.
--
-- The sweep fails a run that keeps killing its process, so that it is not
-- resumed (and billed) for ever. The bound used to be the run's age since it
-- first started, which also failed a run that was healthy for hours and then
-- lost its process once. A count of sweep resumes bounds the right thing.
-- A graceful shutdown (ReleaseRun) does not count.
DO $$
BEGIN
    IF to_regclass('eval_runs') IS NOT NULL THEN
        ALTER TABLE eval_runs
            ADD COLUMN IF NOT EXISTS resume_count integer NOT NULL DEFAULT 0;
    END IF;
END
$$;
