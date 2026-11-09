-- 0250_agent_workforce_policies.sql — the project's automatic hiring and
-- automatic dismissal policies (ADR-0050 decisions 5c and 5k, issue 1617).
-- Both are OFF by default: a project without a row here has neither.
--
--   elitea_lifecycle.agent_workforce_policies
--       One row per project, written by an agent manager
--       (agents.workforce.manage).
--       auto_hire_*      turn an open agent proposal (shared/0235) into an
--                        agent without a click, only when its tools are all
--                        read-only (computed tier low), its spec has at least
--                        min_successful_runs completed spawns and no failed or
--                        budget-exhausted one, and the project holds fewer
--                        than max_auto_hired automatically hired agents that
--                        nobody reviewed yet. The agent is created
--                        unpublished, with no tools bound, private to the
--                        person the proposing run served, on probation; an
--                        agent manager can undo the hire for 14 days.
--       auto_dismiss_*   rules that map scorecard conditions (shared/0245)
--                        to watch, suspend or retire. watch and suspend apply
--                        at once; retire posts a notice and applies after
--                        grace_days unless the owner or an agent manager
--                        objects. Never demote, never delete, never past
--                        retire; critical-tier agents and agents under legal
--                        hold are exempt. An agent whose owners have left the
--                        project is flagged for reassignment and moved to
--                        watch after 30 days; it is never retired
--                        automatically.
--
--   elitea_lifecycle.agent_automatic_actions
--       Every automatic action, for the list a manager reads and acts on
--       (undo, object, keep): kind auto_hire | auto_dismiss | orphan, the
--       rule, the stage, the proposal it went through (agent_proposals for a
--       hire, agent_workforce_proposals for a stage), its status and when it
--       is due (a retire notice's end, an orphan's watch, a hire's undo
--       window).
--
--   elitea_lifecycle.agent_registry (new columns)
--       auto_hired_at / auto_hire_proposal_id / probation_until /
--       probation_reviewed_at / probation_reviewed_by
--                 an automatically hired agent and its probation: on
--                 probation until an agent manager keeps it (reviewed), undo
--                 possible until probation_until.
--       orphaned_since
--                 the registry's flag of an agent whose owners left.
--
--   elitea_lifecycle.agent_workforce_proposals (widened)
--       source gains policy: a stage the policy applied or noticed, with
--       policy_rule naming the rule; it has no proposing person or agent, a
--       stage of watch, suspend or retire only, and an approval with no
--       deciding person (the platform applied it under the policy a person
--       set).
--
-- Additive only. Idempotent. No BEGIN/COMMIT: the ledgered runner wraps the
-- file.

CREATE SCHEMA IF NOT EXISTS elitea_lifecycle;

CREATE TABLE IF NOT EXISTS elitea_lifecycle.agent_workforce_policies (
    project_id             bigint PRIMARY KEY CHECK (project_id > 0),
    auto_hire_enabled      boolean NOT NULL DEFAULT false,
    min_successful_runs    integer NOT NULL DEFAULT 3 CHECK (min_successful_runs BETWEEN 1 AND 100),
    max_auto_hired         integer NOT NULL DEFAULT 5 CHECK (max_auto_hired BETWEEN 1 AND 50),
    auto_dismiss_enabled   boolean NOT NULL DEFAULT false,
    -- [{id, when: {metric: "op value"}, then}], at most 20.
    auto_dismiss_rules     jsonb NOT NULL DEFAULT '[]'::jsonb
                           CHECK (jsonb_typeof(auto_dismiss_rules) = 'array'
                                  AND jsonb_array_length(auto_dismiss_rules) <= 20
                                  AND octet_length(auto_dismiss_rules::text) <= 16384),
    grace_days             integer NOT NULL DEFAULT 7 CHECK (grace_days BETWEEN 1 AND 90),
    updated_by             bigint CHECK (updated_by IS NULL OR updated_by > 0),
    created_at             timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at             timestamptz NOT NULL DEFAULT clock_timestamp()
);

COMMENT ON TABLE elitea_lifecycle.agent_workforce_policies IS
    'A project''s automatic hiring and dismissal policy (ADR-0050 decisions 5c and 5k). Off by default: no row, nothing automatic.';

CREATE TABLE IF NOT EXISTS elitea_lifecycle.agent_automatic_actions (
    id                     bigserial PRIMARY KEY,
    project_id             bigint NOT NULL CHECK (project_id > 0),
    application_id         bigint NOT NULL CHECK (application_id > 0),
    kind                   text NOT NULL CHECK (kind IN ('auto_hire', 'auto_dismiss', 'orphan')),
    rule                   text NOT NULL CHECK (rule ~ '^[a-z][a-z0-9_]{0,39}$'),
    stage                  text CHECK (stage IS NULL OR stage IN ('watch', 'suspend', 'retire')),
    -- agent_proposals.id for auto_hire; agent_workforce_proposals.id for a stage.
    proposal_id            bigint CHECK (proposal_id IS NULL OR proposal_id > 0),
    -- pending  a retire notice in its grace period;
    -- flagged  an orphan offered for reassignment;
    -- applied  done (a hire, a stage);
    -- held     due but left to a person (nested by other agents, exempt);
    -- objected the owner or a manager objected in the window;
    -- undone   a manager undid it;
    -- kept     a manager kept an automatically hired agent (probation ends);
    -- resolved the orphan got an owner back;
    -- cancelled the policy was turned off, or the agent left the conditions.
    status                 text NOT NULL CHECK (status IN ('pending', 'flagged', 'applied', 'held', 'objected',
                                                           'undone', 'kept', 'resolved', 'cancelled')),
    due_at                 timestamptz,
    evidence               jsonb NOT NULL DEFAULT '{}'::jsonb
                           CHECK (jsonb_typeof(evidence) = 'object' AND octet_length(evidence::text) <= 16384),
    evidence_digest        text NOT NULL CHECK (evidence_digest ~ '^[0-9a-f]{64}$'),
    note                   text CHECK (note IS NULL OR octet_length(note) <= 2048),
    decided_by_user_id     bigint CHECK (decided_by_user_id IS NULL OR decided_by_user_id > 0),
    decided_at             timestamptz,
    applied_at             timestamptz,
    created_at             timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at             timestamptz NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT agent_automatic_actions_stage CHECK ((kind = 'auto_hire') = (stage IS NULL)),
    CONSTRAINT agent_automatic_actions_due CHECK (status NOT IN ('pending', 'flagged') OR due_at IS NOT NULL),
    CONSTRAINT agent_automatic_actions_person CHECK (
        status NOT IN ('objected', 'undone', 'kept') OR (decided_by_user_id IS NOT NULL AND decided_at IS NOT NULL)
    )
);

-- At most one open (pending or flagged) action of a kind per agent.
CREATE UNIQUE INDEX IF NOT EXISTS agent_automatic_actions_one_open
    ON elitea_lifecycle.agent_automatic_actions (project_id, application_id, kind)
    WHERE status IN ('pending', 'flagged');

-- The list, and the due pass.
CREATE INDEX IF NOT EXISTS agent_automatic_actions_project
    ON elitea_lifecycle.agent_automatic_actions (project_id, created_at DESC);
CREATE INDEX IF NOT EXISTS agent_automatic_actions_due
    ON elitea_lifecycle.agent_automatic_actions (project_id, due_at)
    WHERE status IN ('pending', 'flagged');
-- The suppression of a rule a person objected to or undid.
CREATE INDEX IF NOT EXISTS agent_automatic_actions_rule
    ON elitea_lifecycle.agent_automatic_actions (project_id, application_id, rule, decided_at DESC);

COMMENT ON TABLE elitea_lifecycle.agent_automatic_actions IS
    'Every action the workforce policy took or noticed (ADR-0050 decisions 5c and 5k), with its undo, objection and probation review.';

ALTER TABLE elitea_lifecycle.agent_registry
    ADD COLUMN IF NOT EXISTS auto_hired_at          timestamptz,
    ADD COLUMN IF NOT EXISTS auto_hire_proposal_id  bigint,
    ADD COLUMN IF NOT EXISTS probation_until        timestamptz,
    ADD COLUMN IF NOT EXISTS probation_reviewed_at  timestamptz,
    ADD COLUMN IF NOT EXISTS probation_reviewed_by  bigint,
    ADD COLUMN IF NOT EXISTS orphaned_since         timestamptz;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'agent_registry_probation'
          AND conrelid = 'elitea_lifecycle.agent_registry'::regclass
    ) THEN
        ALTER TABLE elitea_lifecycle.agent_registry
            ADD CONSTRAINT agent_registry_probation
            CHECK (auto_hired_at IS NOT NULL OR (probation_until IS NULL AND probation_reviewed_at IS NULL));
    END IF;
END
$$;

-- max_auto_hired counts these.
CREATE INDEX IF NOT EXISTS agent_registry_on_probation
    ON elitea_lifecycle.agent_registry (tenant_project_id)
    WHERE auto_hired_at IS NOT NULL AND probation_reviewed_at IS NULL;

ALTER TABLE elitea_lifecycle.agent_workforce_proposals
    ADD COLUMN IF NOT EXISTS policy_rule text
        CHECK (policy_rule IS NULL OR policy_rule ~ '^[a-z][a-z0-9_]{0,39}$');

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'agent_workforce_proposals_source_known'
          AND conrelid = 'elitea_lifecycle.agent_workforce_proposals'::regclass
    ) THEN
        ALTER TABLE elitea_lifecycle.agent_workforce_proposals
            DROP CONSTRAINT IF EXISTS agent_workforce_proposals_source_check;
        ALTER TABLE elitea_lifecycle.agent_workforce_proposals
            ADD CONSTRAINT agent_workforce_proposals_source_known
            CHECK (source IN ('person', 'hr_agent', 'policy'));
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'agent_workforce_proposals_proposer_v2'
          AND conrelid = 'elitea_lifecycle.agent_workforce_proposals'::regclass
    ) THEN
        ALTER TABLE elitea_lifecycle.agent_workforce_proposals
            DROP CONSTRAINT IF EXISTS agent_workforce_proposals_proposer;
        ALTER TABLE elitea_lifecycle.agent_workforce_proposals
            ADD CONSTRAINT agent_workforce_proposals_proposer_v2 CHECK (
                (source = 'person' AND proposed_by_user_id IS NOT NULL AND proposer_application_id IS NULL
                 AND policy_rule IS NULL)
                OR (source = 'hr_agent' AND proposer_application_id IS NOT NULL AND execution_id IS NOT NULL
                    AND policy_rule IS NULL)
                OR (source = 'policy' AND policy_rule IS NOT NULL AND proposed_by_user_id IS NULL
                    AND proposer_application_id IS NULL)
            );
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'agent_workforce_proposals_decision_v2'
          AND conrelid = 'elitea_lifecycle.agent_workforce_proposals'::regclass
    ) THEN
        ALTER TABLE elitea_lifecycle.agent_workforce_proposals
            DROP CONSTRAINT IF EXISTS agent_workforce_proposals_decision;
        ALTER TABLE elitea_lifecycle.agent_workforce_proposals
            ADD CONSTRAINT agent_workforce_proposals_decision_v2 CHECK (
                (status IN ('proposed', 'asked_owner') AND decided_at IS NULL)
                OR (status IN ('approved', 'rejected') AND decided_at IS NOT NULL
                    AND (decided_by_user_id IS NOT NULL OR source = 'policy'))
            );
    END IF;
    -- The policy never demotes and never deletes.
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'agent_workforce_proposals_policy_stage'
          AND conrelid = 'elitea_lifecycle.agent_workforce_proposals'::regclass
    ) THEN
        ALTER TABLE elitea_lifecycle.agent_workforce_proposals
            ADD CONSTRAINT agent_workforce_proposals_policy_stage
            CHECK (source <> 'policy' OR stage IN ('watch', 'suspend', 'retire'));
    END IF;
END
$$;
