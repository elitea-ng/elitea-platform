-- Let an author EXCLUDE one evaluation case from a run without deleting it
-- (legacy issue 6700: "Exclude/Include" becomes a checkbox per case).
--
-- WHAT WAS MISSING. A dataset case was either in the dataset or deleted. An
-- author who wanted to run nine of ten cases had to delete the tenth and type
-- it again later. The reference UI shows a checkbox per case and an "N active"
-- chip, and a run executes the active cases only.
--
-- WHY A COLUMN. Exclusion is one boolean fact about one case, read on the
-- same row the run start already reads. `NOT NULL DEFAULT false` makes every
-- existing case active, so this migration changes NO run on its own.
--
-- WHERE IT IS READ. The run start (run_handler.go) freezes the active case
-- ids into the run snapshot and refuses a run where no case is active (422).
-- The orchestrator executes the snapshot's cases only, so a case excluded
-- AFTER a run starts does not change that run.
--
-- The guard is 0134's: a tenant schema that has not yet reached 0132 has no
-- table here, and an unguarded ALTER on a missing relation raises 42P01 and
-- fails the whole chain. The COMMENT is inside the guard for the same reason.
DO $$
BEGIN
    IF to_regclass('eval_dataset_cases') IS NULL THEN
        RETURN;
    END IF;

    ALTER TABLE eval_dataset_cases
        ADD COLUMN IF NOT EXISTS excluded boolean NOT NULL DEFAULT false;

    COMMENT ON COLUMN eval_dataset_cases.excluded IS
        'true = the author keeps the case, but a new run does not execute it. Frozen into the run snapshot at start.';
END
$$;
