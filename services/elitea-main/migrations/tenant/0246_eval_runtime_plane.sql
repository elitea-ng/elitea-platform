-- 0246_eval_runtime_plane.sql — evaluation cases as real agent runs
-- (issues 2011 step B and 2013 step 2).
--
-- eval_suites gains the settings of a runtime_plane run:
--
--   execution_mode            'predict_blocking' (one LLM call per case, the
--                             default and what every existing suite keeps) or
--                             'runtime_plane' (each case is a real agent run of
--                             the pinned version on the worker).
--   step_limit_per_case       the most agent steps (model calls and tool calls)
--                             one case may take before its run is cancelled.
--   time_limit_per_case_seconds
--                             the wall time one case may take before its run is
--                             cancelled and a timeout result is stored.
--   store_tool_outputs        whether the case's stored evidence keeps the tool
--                             outputs of its steps.
--
-- The three limits mean nothing for predict_blocking, which has no steps and no
-- tools: a CHECK refuses them on such a suite, so a stored suite never claims a
-- control that does nothing. NULL is "no limit".
--
-- eval_case_runs names the agent run (runtime execution) a case ran as, so the
-- gateway calls of that execution are attributed to the evaluation run and the
-- case: predict_blocking calls carry `eval:<run>:case:<case>`, but the worker
-- signs its calls with the execution id. Several rows may name one case: a
-- resumed run executes a case again, and the first execution's spend still
-- belongs to the run.
--
-- Unqualified names: the runner pins search_path to the tenant schema. No
-- BEGIN/COMMIT. Idempotent.

ALTER TABLE eval_suites ADD COLUMN IF NOT EXISTS execution_mode varchar(32) NOT NULL DEFAULT 'predict_blocking';
ALTER TABLE eval_suites ADD COLUMN IF NOT EXISTS step_limit_per_case integer;
ALTER TABLE eval_suites ADD COLUMN IF NOT EXISTS time_limit_per_case_seconds integer;
ALTER TABLE eval_suites ADD COLUMN IF NOT EXISTS store_tool_outputs boolean NOT NULL DEFAULT true;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                   WHERE conname = 'eval_suites_execution_mode_check'
                     AND conrelid = 'eval_suites'::regclass) THEN
        ALTER TABLE eval_suites ADD CONSTRAINT eval_suites_execution_mode_check
            CHECK (execution_mode IN ('predict_blocking', 'runtime_plane'));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                   WHERE conname = 'eval_suites_runtime_limits_check'
                     AND conrelid = 'eval_suites'::regclass) THEN
        ALTER TABLE eval_suites ADD CONSTRAINT eval_suites_runtime_limits_check
            CHECK ((step_limit_per_case IS NULL OR step_limit_per_case > 0)
               AND (time_limit_per_case_seconds IS NULL OR time_limit_per_case_seconds > 0)
               AND (execution_mode = 'runtime_plane'
                    OR (step_limit_per_case IS NULL AND time_limit_per_case_seconds IS NULL)));
    END IF;
END
$$;

CREATE TABLE IF NOT EXISTS eval_case_runs (
    id serial PRIMARY KEY,
    run_id integer NOT NULL REFERENCES eval_runs(id) ON DELETE CASCADE,
    dataset_case_id integer NOT NULL,
    execution_id varchar(128) NOT NULL,
    conversation_uuid uuid,
    created_at timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT eval_case_runs_execution_nonempty_check CHECK (btrim(execution_id) <> ''),
    CONSTRAINT eval_case_runs_execution_key UNIQUE (run_id, execution_id)
);

CREATE INDEX IF NOT EXISTS eval_case_runs_case_idx ON eval_case_runs (run_id, dataset_case_id);
