-- 0241_agent_teams_keyset_index.sql — an index for the teams list's keyset
-- page (issue 1955).
--
-- The list orders by (lower(name), id) within a project and starts a page
-- after the last pair the caller holds. agent_teams_project_name_key covers
-- (project_id, lower(name)) only, so the id tiebreak was sorted. This index
-- carries all three, in the list's order. Idempotent.
CREATE INDEX IF NOT EXISTS agent_teams_project_name_id_idx
    ON elitea_chat.agent_teams (project_id, lower(name), id);
