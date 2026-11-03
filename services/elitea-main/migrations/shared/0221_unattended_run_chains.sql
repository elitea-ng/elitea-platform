-- 0221_unattended_run_chains.sql — unattended grants and run chains
-- (ADR-0045 decisions 1, 2 and 4; issues #1435, #1436).
--
--   elitea_runtime.run_chains
--       One row per chain: a run that a person started with an unattended
--       grant (the chain's root), and every automatic continuation of it.
--       The row holds the root, the owner, the conversation, the grant (the
--       admitted grant as JSON, and its grant_id), the count of
--       continuations, the spend so far, the effective limits, the deadline
--       and the end state. Main checks the limits and adds one continuation
--       in ONE statement (repos/run_chains.go), so two continuations that
--       race cannot both pass the last free slot.
--
--       origin 'cloud'    a cloud agent run (agent_execution_jobs).
--       origin 'desktop'  a desktop local turn (local_turn_executions).
--
--       end_state is NULL while the chain can continue. completed, failed,
--       waiting, limit_reached or cancelled is written once, when the chain
--       ends (end_limit names the limit of a limit_reached chain). The
--       run_finished notification (batch U3) reads it.
--
--   elitea_runtime.run_chain_links
--       One row per link: the root (index 0) and each continuation. A
--       settle hook finds the chain of a settled execution here, and the
--       spend check sums the gateway request log of these executions.
--
--   elitea_runtime.approval_ledger.grant_id
--       The grant of an approval that a grant answered (reason
--       unattended_grant, actor_kind user, actor_user_id the grant's owner).
--
-- This amends the meaning of 0211's note "Auto-approve is not a value".
-- That note stays true for elitea_runtime.unattended_run_policies: a
-- schedule or webhook run never approves on its own (ADR-0041 decision 5).
-- A run that a PERSON starts can carry a grant that names sensitive tools:
-- the person gives each approval in advance, and each use is a user
-- decision row with reason unattended_grant and the grant_id. It is not a
-- system approval.
--
-- No permission: the chain is read by Main only. No backfill. Idempotent.
-- No BEGIN/COMMIT: the ledgered runner wraps the file.

CREATE SCHEMA IF NOT EXISTS elitea_runtime;

CREATE TABLE IF NOT EXISTS elitea_runtime.run_chains (
    root_execution_id   text PRIMARY KEY CHECK (root_execution_id ~ '^[0-9a-f]{32}$'),
    project_id          bigint NOT NULL CHECK (project_id > 0),
    owner_user_id       bigint NOT NULL CHECK (owner_user_id > 0),
    conversation_uuid   uuid NOT NULL,
    origin              text NOT NULL CHECK (origin IN ('cloud', 'desktop')),
    trigger_origin      text NOT NULL DEFAULT 'manual' CHECK (trigger_origin IN ('manual', 'api', 'schedule', 'webhook')),
    grant_id            uuid NOT NULL UNIQUE,
    -- The admitted grant: the parts the person gave, with the platform
    -- defaults filled in. It never holds a secret or a prompt.
    grant_json          jsonb NOT NULL
                        CHECK (jsonb_typeof(grant_json) = 'object' AND octet_length(grant_json::text) <= 65536),
    continuations       integer NOT NULL DEFAULT 0 CHECK (continuations >= 0),
    max_continuations   integer NOT NULL CHECK (max_continuations BETWEEN 0 AND 100000),
    -- US dollars. 0 in max_spend_usd means the chain has no spend limit.
    spend_usd           numeric(18, 6) NOT NULL DEFAULT 0 CHECK (spend_usd >= 0),
    max_spend_usd       numeric(18, 4) NOT NULL DEFAULT 0 CHECK (max_spend_usd >= 0),
    deadline            timestamptz NOT NULL,
    last_execution_id   text NOT NULL CHECK (last_execution_id ~ '^[0-9a-f]{32}$'),
    end_state           text CHECK (end_state IS NULL OR end_state IN (
                            'completed', 'failed', 'waiting', 'limit_reached', 'cancelled')),
    end_limit           text CHECK (end_limit IS NULL OR end_limit IN ('continuations', 'spend', 'deadline')),
    ended_at            timestamptz,
    created_at          timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at          timestamptz NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT run_chains_end CHECK ((end_state IS NULL) = (ended_at IS NULL)),
    CONSTRAINT run_chains_end_limit CHECK (end_limit IS NULL OR end_state = 'limit_reached')
);

-- The owner's chains, newest first (the run_finished notification and the UI).
CREATE INDEX IF NOT EXISTS run_chains_owner_time
    ON elitea_runtime.run_chains (owner_user_id, created_at DESC);
-- The chains of one conversation.
CREATE INDEX IF NOT EXISTS run_chains_conversation
    ON elitea_runtime.run_chains (project_id, conversation_uuid);

CREATE TABLE IF NOT EXISTS elitea_runtime.run_chain_links (
    execution_id        text PRIMARY KEY CHECK (execution_id ~ '^[0-9a-f]{32}$'),
    root_execution_id   text NOT NULL
                        REFERENCES elitea_runtime.run_chains (root_execution_id) ON DELETE CASCADE,
    continuation_index  integer NOT NULL CHECK (continuation_index >= 0),
    -- root: the first run. step_limit, output_limit, tool_call_cap and
    -- resume: why the link started. continue: a desktop continuation (the
    -- host does not say which cap it continues).
    kind                text NOT NULL CHECK (kind IN ('root', 'step_limit', 'output_limit', 'tool_call_cap', 'resume', 'continue')),
    -- The chat response a cloud link writes; NULL for a desktop link.
    response_message_id text CHECK (response_message_id IS NULL OR length(response_message_id) BETWEEN 1 AND 128),
    created_at          timestamptz NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT run_chain_links_root_index CHECK ((kind = 'root') = (continuation_index = 0)),
    UNIQUE (root_execution_id, continuation_index)
);

ALTER TABLE elitea_runtime.approval_ledger
    ADD COLUMN IF NOT EXISTS grant_id uuid;

DO $$
BEGIN
    ALTER TABLE elitea_runtime.approval_ledger
        DROP CONSTRAINT IF EXISTS approval_ledger_grant;
    ALTER TABLE elitea_runtime.approval_ledger
        ADD CONSTRAINT approval_ledger_grant CHECK (
            grant_id IS NULL
            OR (reason = 'unattended_grant' AND actor_kind = 'user' AND phase = 'decision'));
END
$$;

-- The rows of one grant (the evidence export and the chain view).
CREATE INDEX IF NOT EXISTS approval_ledger_grant
    ON elitea_runtime.approval_ledger (grant_id)
    WHERE grant_id IS NOT NULL;

COMMENT ON COLUMN elitea_runtime.approval_ledger.grant_id IS
    'The unattended grant (ADR-0045) that answered this approval in advance. Set only on a decision row with reason unattended_grant and actor_kind user (the grant''s owner).';
COMMENT ON TABLE elitea_runtime.unattended_run_policies IS
    'What a schedule or webhook run does at a step that needs an approval (ADR-0041 decision 5). Auto-approve is not a value: a schedule or webhook run never approves on its own. An unattended grant (ADR-0045, run_chains) is a person''s advance decision on a run that the person starts, and it is never accepted on these origins.';
COMMENT ON TABLE elitea_runtime.run_chains IS
    'One unattended chain (ADR-0045 decision 4): the root run, its owner and grant, the continuation count, the spend so far, the limits, the deadline and the end state.';
