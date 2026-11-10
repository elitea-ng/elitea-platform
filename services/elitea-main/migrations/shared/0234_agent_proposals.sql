-- 0234_agent_proposals.sql — an agent proposes promoting a temporary
-- sub-agent to a saved agent; a person reviews and creates it, or dismisses
-- the proposal (ADR-0050 decision 5a, issue 1615).
--
--   elitea_lifecycle.agent_proposals
--       One row per proposal. The propose_agent tool writes it through the
--       claim-bound content listener: Main fills the project, the proposing
--       agent, the person the run served, the conversation and the run from
--       the execution kernel, never from the worker. It holds the DRAFT of
--       the agent (an AgentDefinition-shaped object: name, description,
--       instructions, soul, tool references), the evidence (the ephemeral
--       spawn it promotes, keyed (execution_id, ephemeral_agent_id) into
--       elitea_runtime.ephemeral_agents, shared/0232), the suggested memory
--       seeds and the rationale. NOTHING here is an agent: Create calls the
--       ordinary create route, which writes the applications row; the
--       proposal then records the agent it became (created_application_id).
--
--       status    proposed (open) | created | dismissed.
--       spec_digest
--                 the sub-agent's profile digest (ephemeral_agents
--                 .profile_digest). A dismissed proposal suppresses a new
--                 proposal of the same digest by the same agent in the same
--                 project for 30 days (checked by the writer).
--
--       Limits (checked by the writer under an advisory lock per proposing
--       agent): at most 3 open proposals per proposing agent per project.
--
-- Shared, not tenant: the lifecycle registry (0204) and the spawn record
-- (0232) are shared and keyed by project; so is this. No foreign key to a
-- tenant table: application ids are per tenant schema.
--
-- Permissions: none new. Proposing takes the agent's
-- meta.capabilities.can_propose_agents and the person's
-- models.applications.applications.create (checked by Main at the tool
-- call); reading the inbox takes models.applications.applications.get,
-- deciding takes create (Create) or create or ownership of the proposing
-- agent (Dismiss).
--
-- Idempotent. No BEGIN/COMMIT: the ledgered runner wraps the file.

CREATE SCHEMA IF NOT EXISTS elitea_lifecycle;

CREATE TABLE IF NOT EXISTS elitea_lifecycle.agent_proposals (
    id                       bigserial PRIMARY KEY,
    project_id               bigint NOT NULL CHECK (project_id > 0),
    -- The agent that proposed (agent_execution_jobs.application_id).
    proposer_application_id  bigint NOT NULL CHECK (proposer_application_id > 0),
    -- The person the proposing run served (execution_jobs.actor_id).
    proposed_for_user_id     bigint NOT NULL CHECK (proposed_for_user_id > 0),
    execution_id             text NOT NULL CHECK (char_length(execution_id) BETWEEN 1 AND 256),
    -- The conversation of the run; NULL for a run with none.
    conversation_uuid        uuid,
    -- The temporary sub-agent it promotes (ephemeral_agents.agent_id).
    ephemeral_agent_id       text NOT NULL CHECK (ephemeral_agent_id ~ '^a_[0-9a-f]{8}$'),
    spec_digest              text NOT NULL CHECK (spec_digest ~ '^[0-9a-f]{64}$'),
    rationale                text NOT NULL CHECK (btrim(rationale) <> '' AND octet_length(rationale) <= 4096),
    -- The draft the review form opens with (AgentDefinition-shaped).
    draft                    jsonb NOT NULL CHECK (jsonb_typeof(draft) = 'object' AND octet_length(draft::text) <= 65536),
    -- [{kind, content}], at most 20; a person picks the ones to keep.
    memory_seeds             jsonb NOT NULL DEFAULT '[]'::jsonb
                             CHECK (jsonb_typeof(memory_seeds) = 'array' AND jsonb_array_length(memory_seeds) <= 20),
    status                   text NOT NULL DEFAULT 'proposed' CHECK (status IN ('proposed', 'created', 'dismissed')),
    decided_by_user_id       bigint CHECK (decided_by_user_id IS NULL OR decided_by_user_id > 0),
    decided_at               timestamptz,
    -- The agent Create made (in project_id's schema).
    created_application_id   bigint CHECK (created_application_id IS NULL OR created_application_id > 0),
    created_at               timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at               timestamptz NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT agent_proposals_decision CHECK (
        (status = 'proposed' AND decided_at IS NULL AND created_application_id IS NULL)
        OR (status = 'dismissed' AND decided_at IS NOT NULL AND created_application_id IS NULL)
        OR (status = 'created' AND decided_at IS NOT NULL AND created_application_id IS NOT NULL)
    ),
    -- One proposal per spawn: a retried tool call answers the same row.
    CONSTRAINT agent_proposals_one_per_spawn UNIQUE (project_id, execution_id, ephemeral_agent_id)
);

-- The inbox: a project's open proposals, newest first.
CREATE INDEX IF NOT EXISTS agent_proposals_open
    ON elitea_lifecycle.agent_proposals (project_id, created_at DESC)
    WHERE status = 'proposed';

-- The limit and the suppression: one proposing agent's rows.
CREATE INDEX IF NOT EXISTS agent_proposals_proposer
    ON elitea_lifecycle.agent_proposals (project_id, proposer_application_id, status, spec_digest);

-- A conversation's proposal cards.
CREATE INDEX IF NOT EXISTS agent_proposals_conversation
    ON elitea_lifecycle.agent_proposals (project_id, conversation_uuid)
    WHERE conversation_uuid IS NOT NULL;

COMMENT ON TABLE elitea_lifecycle.agent_proposals IS
    'An agent''s proposal to promote a temporary sub-agent to a saved agent (ADR-0050 decision 5a). Nothing here is an agent until a person presses Create.';
