-- 0249_agent_workforce_manager_source.sql — the proposal source `hr_agent`
-- becomes `workforce_manager` (ADR-0050 decision 5l, renamed 2026-11-14).
--
-- The agent kind that reviews other agents' scorecards and proposes dismissals
-- was called an "HR agent"; it is a workforce manager. 0236 and 0237 are
-- released and immutable, so this file does what they cannot: it rewrites the
-- stored value and replaces the two CHECK constraints that name it
-- (agent_workforce_proposals_source_known and agent_workforce_proposals_proposer_v2,
-- both from 0237) with the same rules over the new value.
--
-- Order matters: the constraints are dropped first because the UPDATE would
-- violate them, then re-added over the new value. Applying the file twice is a
-- no-op (the UPDATE matches nothing, the constraints are replaced by
-- themselves). No new permission, no schema change beyond the two constraints.
-- No BEGIN/COMMIT: the ledgered runner wraps the file.

DO $$
BEGIN
    IF to_regclass('elitea_lifecycle.agent_workforce_proposals') IS NULL THEN
        RETURN;
    END IF;

    ALTER TABLE elitea_lifecycle.agent_workforce_proposals
        DROP CONSTRAINT IF EXISTS agent_workforce_proposals_source_known;
    ALTER TABLE elitea_lifecycle.agent_workforce_proposals
        DROP CONSTRAINT IF EXISTS agent_workforce_proposals_proposer_v2;

    UPDATE elitea_lifecycle.agent_workforce_proposals
       SET source = 'workforce_manager'
     WHERE source = 'hr_agent';

    ALTER TABLE elitea_lifecycle.agent_workforce_proposals
        ADD CONSTRAINT agent_workforce_proposals_source_known
        CHECK (source IN ('person', 'workforce_manager', 'policy'));
    ALTER TABLE elitea_lifecycle.agent_workforce_proposals
        ADD CONSTRAINT agent_workforce_proposals_proposer_v2 CHECK (
            (source = 'person' AND proposed_by_user_id IS NOT NULL AND proposer_application_id IS NULL
             AND policy_rule IS NULL)
            OR (source = 'workforce_manager' AND proposer_application_id IS NOT NULL AND execution_id IS NOT NULL
                AND policy_rule IS NULL)
            OR (source = 'policy' AND policy_rule IS NOT NULL AND proposed_by_user_id IS NULL
                AND proposer_application_id IS NULL)
        );
END
$$;
