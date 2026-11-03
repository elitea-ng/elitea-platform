-- 0222_run_brief_goal_check.sql — the run brief and the goal check of an
-- unattended chain (ADR-0045 decision 11; issue #1564).
--
--   elitea_runtime.run_chains.brief
--       The brief that the small model wrote from the person's request:
--       {goal, done_when[{id, text}], constraints[], assumptions[],
--       ambiguity}. Main validates it and bounds every field before it
--       writes it. NULL until the brief is ready.
--
--   elitea_runtime.run_chains.brief_state
--       none     the grant has goal_check false, or the chain is older
--                than this migration.
--       off      the administrator switch approvals.run_brief was off when
--                the chain started.
--       pending  Main asks the model for the brief.
--       ready    the brief is stored.
--       failed   the model failed, timed out or answered no valid brief.
--       The brief is frozen: Main writes it once (pending -> ready or
--       failed) and never changes it after that.
--
--   elitea_runtime.run_chains.brief_claimed_at
--       When a Main replica claimed the pending brief to ask the model. Only
--       the claimant asks; a claim older than 30 seconds lapses, so another
--       replica can retry after a crash or a transient error. NULL when no
--       replica holds the brief.
--
--   elitea_runtime.run_chains.brief_sha256
--       The SHA-256 of the stored brief JSON (hex). NULL without a brief.
--
--   elitea_runtime.run_chains.goal_check_failures
--       Condition id -> the count of consecutive goal checks of this chain
--       that found the condition not met. 3 ends the chain as failed.
--
--   elitea_runtime.run_chain_links.done_check
--       The goal check of the link's run: {outcome, conditions, met, total,
--       reason, continue_text}. Written once.
--
--   elitea_runtime.run_chain_links.kind
--       Adds goal_not_met: a continuation that a goal check started.
--
-- No backfill: an existing chain keeps brief_state 'none' and ends as
-- before. No permission. Idempotent.
-- No BEGIN/COMMIT: the ledgered runner wraps the file.

ALTER TABLE elitea_runtime.run_chains
    ADD COLUMN IF NOT EXISTS brief jsonb,
    ADD COLUMN IF NOT EXISTS brief_state text NOT NULL DEFAULT 'none',
    ADD COLUMN IF NOT EXISTS brief_sha256 text,
    ADD COLUMN IF NOT EXISTS brief_claimed_at timestamptz,
    ADD COLUMN IF NOT EXISTS goal_check_failures jsonb;

ALTER TABLE elitea_runtime.run_chain_links
    ADD COLUMN IF NOT EXISTS done_check jsonb;

DO $$
BEGIN
    ALTER TABLE elitea_runtime.run_chains
        DROP CONSTRAINT IF EXISTS run_chains_brief_state;
    ALTER TABLE elitea_runtime.run_chains
        ADD CONSTRAINT run_chains_brief_state CHECK (
            brief_state IN ('none', 'off', 'pending', 'ready', 'failed'));

    ALTER TABLE elitea_runtime.run_chains
        DROP CONSTRAINT IF EXISTS run_chains_brief;
    ALTER TABLE elitea_runtime.run_chains
        ADD CONSTRAINT run_chains_brief CHECK (
            (brief IS NULL) = (brief_state <> 'ready')
            AND (brief IS NULL) = (brief_sha256 IS NULL)
            AND (brief IS NULL OR (jsonb_typeof(brief) = 'object' AND octet_length(brief::text) <= 16384))
            AND (brief_sha256 IS NULL OR brief_sha256 ~ '^[0-9a-f]{64}$'));

    ALTER TABLE elitea_runtime.run_chains
        DROP CONSTRAINT IF EXISTS run_chains_goal_check_failures;
    ALTER TABLE elitea_runtime.run_chains
        ADD CONSTRAINT run_chains_goal_check_failures CHECK (
            goal_check_failures IS NULL OR jsonb_typeof(goal_check_failures) = 'object');

    ALTER TABLE elitea_runtime.run_chain_links
        DROP CONSTRAINT IF EXISTS run_chain_links_done_check;
    ALTER TABLE elitea_runtime.run_chain_links
        ADD CONSTRAINT run_chain_links_done_check CHECK (
            done_check IS NULL
            OR (jsonb_typeof(done_check) = 'object' AND octet_length(done_check::text) <= 32768));

    -- 0221 declared the kind check inline, so it has the default name.
    ALTER TABLE elitea_runtime.run_chain_links
        DROP CONSTRAINT IF EXISTS run_chain_links_kind_check;
    ALTER TABLE elitea_runtime.run_chain_links
        ADD CONSTRAINT run_chain_links_kind_check CHECK (kind IN (
            'root', 'step_limit', 'output_limit', 'tool_call_cap', 'resume', 'continue', 'goal_not_met'));
END
$$;

COMMENT ON COLUMN elitea_runtime.run_chains.brief IS
    'The run brief (ADR-0045 decision 11): the goal, the done_when conditions, the constraints, the assumptions and the ambiguity. Written once by Main from the small model''s answer, after validation. Frozen.';
COMMENT ON COLUMN elitea_runtime.run_chains.brief_state IS
    'none, off, pending, ready or failed (ADR-0045 decision 11). Moves once from pending to ready or failed.';
COMMENT ON COLUMN elitea_runtime.run_chains.brief_claimed_at IS
    'When a Main replica claimed the pending brief to ask the model (ADR-0045 decision 11). A claim lapses after 30 seconds. NULL when unclaimed.';
COMMENT ON COLUMN elitea_runtime.run_chains.goal_check_failures IS
    'Condition id -> the count of consecutive goal checks that found it not met. 3 ends the chain as failed.';
COMMENT ON COLUMN elitea_runtime.run_chain_links.done_check IS
    'The goal check of this link''s run (ADR-0045 decision 11): outcome, conditions, met, total, reason, continue_text. Written once.';
