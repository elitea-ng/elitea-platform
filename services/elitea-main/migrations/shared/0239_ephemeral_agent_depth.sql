-- 0239_ephemeral_agent_depth.sql — a sub-agent's depth and its parent
-- sub-agent in the spawn record (ADR-0050 decision 4, issue 1872).
--
-- A run may let its sub-agents start sub-agents of their own
-- (agents.subagents_max_depth 2, issue 1824). The record of 0232 kept every
-- spawn as a child of the run's agent, so a grandchild looked like an
-- ordinary child:
--
--   depth             1 for a child of the run's agent, 2 for a child of a
--                     child.
--   parent_agent_id   the sub-agent that started this one (its agent_id in
--                     the same run); NULL at depth 1.
--
-- The worker reports both with ReportEphemeralAgents (EphemeralAgentV1
-- fields 17 and 18). No backfill: rows written before this release are
-- children of the run's agent (depth 1, no parent), which is all an earlier
-- worker could start. No permission. Idempotent. No BEGIN/COMMIT: the
-- ledgered runner wraps the file.

ALTER TABLE elitea_runtime.ephemeral_agents
    ADD COLUMN IF NOT EXISTS depth smallint NOT NULL DEFAULT 1,
    ADD COLUMN IF NOT EXISTS parent_agent_id text;

DO $$
BEGIN
    ALTER TABLE elitea_runtime.ephemeral_agents
        DROP CONSTRAINT IF EXISTS ephemeral_agents_depth;
    ALTER TABLE elitea_runtime.ephemeral_agents
        ADD CONSTRAINT ephemeral_agents_depth CHECK (
            -- IS NOT NULL first: a NULL parent must make the depth-2 arm
            -- false, not unknown (a CHECK passes on unknown).
            (depth = 1 AND parent_agent_id IS NULL)
            OR (depth = 2 AND parent_agent_id IS NOT NULL
                AND parent_agent_id ~ '^a_[0-9a-f]{8}$' AND parent_agent_id <> agent_id)
        );
END
$$;

COMMENT ON COLUMN elitea_runtime.ephemeral_agents.depth IS
    '1 for a sub-agent of the run''s agent, 2 for a sub-agent a sub-agent started (ADR-0050 decision 4).';
COMMENT ON COLUMN elitea_runtime.ephemeral_agents.parent_agent_id IS
    'The sub-agent of the same run that started this one; NULL for a sub-agent of the run''s agent.';
