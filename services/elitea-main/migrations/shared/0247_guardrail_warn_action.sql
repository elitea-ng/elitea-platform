-- 0247_guardrail_warn_action.sql — the `warn` guardrail action and the
-- `personal_data` detector type in the verdict table (ADR-0043 decision 1).
--
-- A warn detector leaves the content as it is, like flag, and the chat tells
-- the person who sent the message. Its verdict has action `warn` and outcome
-- `warned`. 0214 pinned the action and outcome columns with CHECK constraints
-- that name the old sets, so a verdict of the new kind would fail the whole
-- verdict INSERT. This migration widens both constraints. The detector type
-- `personal_data` (13 characters) fits the VARCHAR(32) column of 0217.
--
-- No data, no permission. Every statement is guarded; no BEGIN/COMMIT: the
-- ledgered runner wraps the file.

DO $$
BEGIN
    IF to_regclass('gateway.guardrail_verdicts') IS NULL THEN
        RETURN;
    END IF;

    ALTER TABLE gateway.guardrail_verdicts
        DROP CONSTRAINT IF EXISTS guardrail_verdicts_action_check;
    ALTER TABLE gateway.guardrail_verdicts
        ADD CONSTRAINT guardrail_verdicts_action_check
        CHECK (action IN ('block', 'mask', 'flag', 'warn'));

    ALTER TABLE gateway.guardrail_verdicts
        DROP CONSTRAINT IF EXISTS guardrail_verdicts_outcome_check;
    ALTER TABLE gateway.guardrail_verdicts
        ADD CONSTRAINT guardrail_verdicts_outcome_check
        CHECK (outcome IN ('pass', 'flagged', 'warned', 'masked', 'blocked', 'error_open', 'error_closed'));
END
$$;
