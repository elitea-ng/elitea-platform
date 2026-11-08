-- 0231_agent_floor_turns.sql — the turn-taking engine: addressed and lead
-- floors, hand_floor and agent-to-agent turns with loop guards (ADR-0050
-- decisions 7c and 7d, issue 1611).
--
-- ONE EXCHANGE PER HUMAN MESSAGE in a conversation with two or more agents. agent_floor_exchanges is keyed by the
-- message's question id. Its first turn is the ordinary start of that
-- message; every later turn (another addressed agent, an agent handed the
-- floor) is one row of agent_floor_turns, run one at a time in position
-- order: the settle of one starts the next (agentexecution floor hook). A
-- conversation has one active exchange: a new human message skips the
-- QUEUED turns of the earlier ones (skip_code superseded).
--
-- THE GUARDS (internal/domain/turntaking) are checked against these rows
-- when an agent hands the floor, under the exchange's row lock. A hand-off
-- that would pass one is not queued: the exchange records the guard
-- (stopped, stop_limit), Main posts a system message, and the message's
-- unattended chain ends limit_reached with that guard as its end_limit.
--
-- THE CHAIN. Each queued turn of a message with an unattended chain is a
-- link of kind agent_turn, so the chain's continuation, spend and deadline
-- limits bound the whole exchange (ADR-0050 decision 7c).
--
-- No backfill, no permission. Idempotent. No BEGIN/COMMIT: the ledgered
-- runner wraps the file.

CREATE TABLE IF NOT EXISTS elitea_runtime.agent_floor_exchanges (
    project_id BIGINT NOT NULL CHECK (project_id > 0),
    conversation_uuid UUID NOT NULL,
    -- The human message (its question id) that started the exchange.
    question_id UUID NOT NULL,
    -- The person whose message it is: every turn of the exchange runs on
    -- their behalf, with the answering agent's own grants.
    owner_user_id BIGINT NOT NULL CHECK (owner_user_id > 0),
    trigger_origin TEXT NOT NULL DEFAULT 'manual' CHECK (trigger_origin IN ('manual', 'api')),
    -- The agent that answered the message first.
    first_participant_id INTEGER NOT NULL CHECK (first_participant_id > 0),
    -- The guard that stopped the exchange; NULL while none did.
    stopped TEXT CHECK (stopped IS NULL OR stopped IN ('agent_hops', 'consecutive_turns', 'pair_exchanges')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (project_id, conversation_uuid, question_id)
);

CREATE TABLE IF NOT EXISTS elitea_runtime.agent_floor_turns (
    project_id BIGINT NOT NULL,
    conversation_uuid UUID NOT NULL,
    question_id UUID NOT NULL,
    position INTEGER NOT NULL CHECK (position BETWEEN 1 AND 64),
    -- The question id this turn's run uses, derived from the exchange's
    -- question and the position, so a replayed start is the same request.
    turn_question_id UUID NOT NULL UNIQUE,
    -- The agent that handed the floor; NULL for the person (an addressed
    -- agent of the message).
    from_participant_id INTEGER CHECK (from_participant_id IS NULL OR from_participant_id > 0),
    to_participant_id INTEGER NOT NULL CHECK (to_participant_id > 0),
    cause TEXT NOT NULL CHECK (cause IN ('human_message', 'agent_mention')),
    -- What the handing agent asked; NULL for an addressed agent.
    message TEXT CHECK (message IS NULL OR octet_length(message) BETWEEN 1 AND 4000),
    -- QUEUED: waiting its turn; STARTED: its run exists (execution_id);
    -- DONE: its run settled; SKIPPED: not run (skip_code).
    state TEXT NOT NULL CHECK (state IN ('QUEUED', 'STARTED', 'DONE', 'SKIPPED')),
    skip_code TEXT CHECK (skip_code ~ '^[a-z][a-z0-9_]{0,63}$'),
    execution_id TEXT CHECK (execution_id ~ '^[0-9a-f]{32}$'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (project_id, conversation_uuid, question_id, position),
    FOREIGN KEY (project_id, conversation_uuid, question_id)
        REFERENCES elitea_runtime.agent_floor_exchanges (project_id, conversation_uuid, question_id) ON DELETE CASCADE,
    CONSTRAINT agent_floor_turns_cause CHECK (
        (cause = 'human_message' AND from_participant_id IS NULL AND message IS NULL)
        OR (cause = 'agent_mention' AND from_participant_id IS NOT NULL AND message IS NOT NULL)),
    CONSTRAINT agent_floor_turns_state CHECK (
        (state = 'QUEUED' AND execution_id IS NULL AND skip_code IS NULL)
        OR (state IN ('STARTED', 'DONE') AND execution_id IS NOT NULL AND skip_code IS NULL)
        OR (state = 'SKIPPED' AND skip_code IS NOT NULL))
);

-- The settle hook and hand_floor read a conversation's newest exchange.
CREATE INDEX IF NOT EXISTS agent_floor_exchanges_conversation
    ON elitea_runtime.agent_floor_exchanges (project_id, conversation_uuid, created_at DESC);

-- The settle hook finds the turn of a settled run.
CREATE UNIQUE INDEX IF NOT EXISTS agent_floor_turns_execution
    ON elitea_runtime.agent_floor_turns (execution_id)
    WHERE execution_id IS NOT NULL;

-- A chain link of a queued agent turn, and the guards as limits.
DO $$
BEGIN
    ALTER TABLE elitea_runtime.run_chain_links
        DROP CONSTRAINT IF EXISTS run_chain_links_kind_check;
    ALTER TABLE elitea_runtime.run_chain_links
        ADD CONSTRAINT run_chain_links_kind_check CHECK (kind IN (
            'root', 'step_limit', 'output_limit', 'tool_call_cap', 'resume', 'continue', 'goal_not_met',
            'handoff', 'agent_turn'));
    ALTER TABLE elitea_runtime.run_chains
        DROP CONSTRAINT IF EXISTS run_chains_end_limit_check;
    ALTER TABLE elitea_runtime.run_chains
        ADD CONSTRAINT run_chains_end_limit_check CHECK (end_limit IS NULL OR end_limit IN (
            'continuations', 'spend', 'deadline', 'agent_hops', 'consecutive_turns', 'pair_exchanges'));
END
$$;

COMMENT ON TABLE elitea_runtime.agent_floor_exchanges IS
    'One human message of a conversation with several agents (ADR-0050 decision 7c): the agent that answered it first and the loop guard that stopped it, if one did.';
COMMENT ON TABLE elitea_runtime.agent_floor_turns IS
    'The agent turns a human message owes after its first: other addressed agents and agents handed the floor (ADR-0050 decisions 7c and 7d), run one at a time in position order.';
