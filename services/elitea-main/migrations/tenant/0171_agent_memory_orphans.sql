-- 0171_agent_memory_orphans.sql — when an agent's memory lost its agent
-- (ADR-0042 retention, issue 1658; the memory is tenant/0163).
--
-- agent_memory_orphans
--   One row per agent whose memory (agent_memory_entries,
--   agent_memory_subject_optouts) is kept after the agent was deleted.
--   application_id is not a foreign key (tenant/0163 says why). first_seen_at
--   is when the deletion was recorded: the agent delete writes the row in its
--   own transaction, and the retention sweep writes it for an agent deleted
--   before this table, when it first finds the memory without its agent. The
--   sweep deletes the memory, and this row, once first_seen_at is older than
--   the step_record retention maximum of the project and no legal hold covers
--   the agent.
--
-- No to_regclass guard: it references no other table. Unqualified names: the
-- runner pins search_path to the tenant schema. No BEGIN/COMMIT. Idempotent.
CREATE TABLE IF NOT EXISTS agent_memory_orphans (
    application_id bigint      PRIMARY KEY CHECK (application_id > 0),
    first_seen_at  timestamptz NOT NULL DEFAULT clock_timestamp()
);

-- The sweep reads the oldest first.
CREATE INDEX IF NOT EXISTS ix_agent_memory_orphans_first_seen
    ON agent_memory_orphans (first_seen_at);
