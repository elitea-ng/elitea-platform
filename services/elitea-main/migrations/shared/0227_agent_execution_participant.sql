-- 0227_agent_execution_participant.sql — the participant a chat run speaks
-- as, and whom its answer is for (ADR-0050 decision 7b and 7e, issue 1605).
--
--   elitea_runtime.agent_execution_jobs.participant_id
--       The chat_participants row id (in the run's tenant schema) of the
--       agent that answers: the turn's target participant. Written at
--       admission. NULL for a run admitted before 0226.
--   elitea_runtime.agent_execution_jobs.addressed_to
--       The answer's addressees, as its chat_message_group.addressed_to
--       (tenant/0161): empty is everyone. NULL for a run admitted before 0226.
--
-- The execution event stream stamps both on each agent frame it serves and
-- derives the `agent_state` frames from them
-- (internal/infra/db/repos/replay_participant.go). The stored frame bytes do
-- not change: a terminal frame's digest is the worker's result artifact.
--
-- Additive only. Idempotent. No BEGIN/COMMIT: the ledgered runner wraps the
-- file.

ALTER TABLE elitea_runtime.agent_execution_jobs
    ADD COLUMN IF NOT EXISTS participant_id integer
        CHECK (participant_id IS NULL OR participant_id > 0),
    ADD COLUMN IF NOT EXISTS addressed_to integer[]
        CHECK (addressed_to IS NULL OR cardinality(addressed_to) <= 64);
