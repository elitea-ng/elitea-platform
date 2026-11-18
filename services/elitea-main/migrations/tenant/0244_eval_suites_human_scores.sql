-- 0244_eval_suites_human_scores.sql — the evaluation suite (run settings) and the
-- append-only human score store (issues 2008 and 2013, step 1).
--
-- eval_suites holds the settings a run is started with. Today those are the
-- token and cost limits per case and per run, and what a run does when its run
-- limit is reached. A run does not read its suite while it executes: the start
-- copies the settings into `eval_runs.snapshot.limits`, so editing a suite
-- cannot change a run that already started. A NULL limit means "no limit".
--
--   token_limit_per_case / cost_limit_per_case  prompt + completion tokens, and
--                       estimated USD, of ONE case (the agent turn and every
--                       judge call of that case).
--   token_limit_per_run / cost_limit_per_run    the same for the whole run.
--   on_run_limit        'stop' ends the run as errored with the reason, and
--                       'report' lets it finish and only records that a limit
--                       was reached (eval_runs.limit_reached).
--
-- eval_human_scores is the second scoring channel: one row per human score of
-- one (run, case, dimension), with the reviewer and a note. It is APPEND-ONLY:
-- a re-score is a new row and the newest row is the score in force, so the
-- history of who changed a score survives. A trigger refuses UPDATE; a DELETE is
-- allowed only because deleting a run cascades to its scores.
--
-- eval_runs.limit_reached is 'tokens' or 'cost' once a run limit was reached.
--
-- Unqualified names: the runner pins search_path to the tenant schema. No
-- BEGIN/COMMIT. Idempotent.

CREATE TABLE IF NOT EXISTS eval_suites (
    id serial PRIMARY KEY,
    uuid uuid UNIQUE DEFAULT gen_random_uuid(),
    name varchar(128) NOT NULL,
    description text,
    token_limit_per_case integer,
    cost_limit_per_case numeric(14, 4),
    token_limit_per_run integer,
    cost_limit_per_run numeric(14, 4),
    on_run_limit varchar(16) NOT NULL DEFAULT 'stop',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT eval_suites_name_nonempty_check CHECK (btrim(name) <> ''),
    CONSTRAINT eval_suites_name_max_len CHECK (char_length(name) <= 128),
    CONSTRAINT eval_suites_on_run_limit_check CHECK (on_run_limit IN ('stop', 'report')),
    CONSTRAINT eval_suites_limits_positive_check CHECK (
        (token_limit_per_case IS NULL OR token_limit_per_case > 0)
        AND (cost_limit_per_case IS NULL OR cost_limit_per_case > 0)
        AND (token_limit_per_run IS NULL OR token_limit_per_run > 0)
        AND (cost_limit_per_run IS NULL OR cost_limit_per_run > 0))
);

CREATE UNIQUE INDEX IF NOT EXISTS eval_suites_name_unique_idx ON eval_suites (lower(name));

CREATE TABLE IF NOT EXISTS eval_human_scores (
    id serial PRIMARY KEY,
    run_id integer NOT NULL REFERENCES eval_runs(id) ON DELETE CASCADE,
    dataset_case_id integer NOT NULL,
    dimension_id integer NOT NULL,
    -- The score on the dimension's own scale, and its 0..100 normalisation as
    -- computed when it was recorded (the snapshot's scale, not the library's).
    native_score double precision NOT NULL,
    normalized_score double precision NOT NULL,
    note text,
    -- The reviewer. No foreign key, for tenant/0131's reason: the score must
    -- outlive the account.
    reviewer_id integer,
    created_at timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT eval_human_scores_normalized_range_check
        CHECK (normalized_score >= 0 AND normalized_score <= 100),
    CONSTRAINT eval_human_scores_note_max_len CHECK (note IS NULL OR char_length(note) <= 2000)
);

CREATE INDEX IF NOT EXISTS eval_human_scores_run_idx
    ON eval_human_scores (run_id, dataset_case_id, dimension_id, id);

CREATE OR REPLACE FUNCTION eval_human_scores_no_update() RETURNS trigger AS $fn$
BEGIN
    RAISE EXCEPTION 'eval_human_scores is append-only: record a new score instead of changing one'
        USING ERRCODE = 'restrict_violation';
END
$fn$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS eval_human_scores_no_update ON eval_human_scores;
CREATE TRIGGER eval_human_scores_no_update
    BEFORE UPDATE ON eval_human_scores
    FOR EACH ROW EXECUTE FUNCTION eval_human_scores_no_update();

ALTER TABLE eval_runs ADD COLUMN IF NOT EXISTS limit_reached varchar(16);

COMMENT ON TABLE eval_suites IS
    'Run settings: token and cost limits per case and per run, and what a run does when its run limit is reached. Copied into eval_runs.snapshot.limits at run start.';
COMMENT ON TABLE eval_human_scores IS
    'Append-only human scores per (run, case, dimension) with reviewer and note. The newest row is the score in force.';
COMMENT ON COLUMN eval_runs.limit_reached IS
    'tokens or cost: the run limit that was reached. NULL when none was.';
