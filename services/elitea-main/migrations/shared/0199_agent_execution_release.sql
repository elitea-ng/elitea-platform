-- 0199_agent_execution_release.sql — the agent release a run executed
-- (ADR-0040 decision 2, issue #1492).
--
--   elitea_runtime.agent_execution_jobs.release_id
--       The release (elitea_lifecycle.application_releases, 0198) the run
--       executed, written at admission. NULL for a run of a draft, an ad hoc
--       chat, a version published before 0198, and every run admitted
--       before 0199. No foreign key: releases are never deleted, and the
--       admission must not take a lock on the release row.
--
-- Additive only. Idempotent. No BEGIN/COMMIT: the ledgered runner wraps
-- the file.

ALTER TABLE elitea_runtime.agent_execution_jobs
    ADD COLUMN IF NOT EXISTS release_id bigint
        CHECK (release_id IS NULL OR release_id > 0);

CREATE INDEX IF NOT EXISTS agent_execution_jobs_release
    ON elitea_runtime.agent_execution_jobs (release_id)
    WHERE release_id IS NOT NULL;
