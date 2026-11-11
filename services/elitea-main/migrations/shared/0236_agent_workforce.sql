-- 0236_agent_workforce.sql — the agent workforce: the daily scorecard, the
-- dismissal ladder with the retired registry state, dismissal and demotion
-- proposals from people and HR agents, the appeal and the reinstatement
-- (ADR-0050 decisions 5b, 5g-5j, 5l and 5m, issue 1616).
--
--   elitea_lifecycle.agent_registry (new columns)
--       under_review / under_review_since
--                 the ladder's Watch stage: a badge "under review" and a
--                 notice to the owner; nothing else changes.
--       lifecycle_state
--                 active | retired | deleted. Retired: the agent was
--                 unpublished, taken out of conversations' rosters and leads
--                 and its schedules turned off; its runs are refused with
--                 agent_retired and a run of an agent that nests it with
--                 nested_agent_retired. Deleted: its definition and memory
--                 were purged after retention; the registry row, the audit,
--                 the traces and the scorecards are kept as evidence.
--       retired_at / retired_by / reinstate_until / purge_after
--                 when it was retired and by whom; until when it can be
--                 reinstated (30 days); from when it may be deleted (the
--                 reinstate window, and never while a legal hold covers it).
--       retire_snapshot
--                 what retirement changed, so reinstatement restores it: the
--                 published versions and releases, the release head, the
--                 roster rows, the leads and the schedules.
--
--   elitea_lifecycle.agent_scorecards
--       One row per (project, agent, day), computed by the main.agent_scorecard
--       routine from platform data only (run outcomes, breaker trips,
--       approval rejections, guardrail hits, spend, people's feedback); it
--       reads no transcript. Kept 400 days.
--
--   elitea_lifecycle.agent_workforce_proposals
--       A dismissal or demotion proposal (kind dismiss | demote): the target
--       stage, the evidence (a scorecard snapshot and its digest), the
--       narrowing of a demotion, who proposed it (a person, or an HR agent's
--       run), the decision, what applying it did (applied), and the owner's
--       appeal and its review by a different agent manager. At most one open
--       proposal per agent.
--
--   elitea_runtime.execution_suspensions.source
--       gains dismissal: the ladder's Suspend stage is an ADR-0041 agent
--       suspension of source dismissal.
--
-- Permission agents.workforce.manage (the agent manager, "HR"): decide
-- dismissal proposals, apply a stage, reinstate, review an appeal. The
-- project admin (default mode) holds it. 0060 returns early on an existing
-- database, so the grant is here and in internal/infra/db/migrations/
-- 001_initial.sql; a project whose matrix was saved gets it on its admin
-- role, as 0229 does.
--
-- Additive only. Idempotent. Every grant tolerates a database without the
-- auth_core tables. No BEGIN/COMMIT: the ledgered runner wraps the file.

CREATE SCHEMA IF NOT EXISTS elitea_lifecycle;

ALTER TABLE elitea_lifecycle.agent_registry
    ADD COLUMN IF NOT EXISTS under_review       boolean NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS under_review_since timestamptz,
    ADD COLUMN IF NOT EXISTS lifecycle_state    text NOT NULL DEFAULT 'active',
    ADD COLUMN IF NOT EXISTS retired_at         timestamptz,
    ADD COLUMN IF NOT EXISTS retired_by         bigint,
    ADD COLUMN IF NOT EXISTS reinstate_until    timestamptz,
    ADD COLUMN IF NOT EXISTS purge_after        timestamptz,
    ADD COLUMN IF NOT EXISTS retire_snapshot    jsonb;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'agent_registry_lifecycle_state_known'
          AND conrelid = 'elitea_lifecycle.agent_registry'::regclass
    ) THEN
        ALTER TABLE elitea_lifecycle.agent_registry
            ADD CONSTRAINT agent_registry_lifecycle_state_known
            CHECK (lifecycle_state IN ('active', 'retired', 'deleted'));
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'agent_registry_retired_complete'
          AND conrelid = 'elitea_lifecycle.agent_registry'::regclass
    ) THEN
        ALTER TABLE elitea_lifecycle.agent_registry
            ADD CONSTRAINT agent_registry_retired_complete
            CHECK (lifecycle_state = 'active'
                   OR (retired_at IS NOT NULL AND reinstate_until IS NOT NULL AND purge_after IS NOT NULL));
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'agent_registry_under_review_since'
          AND conrelid = 'elitea_lifecycle.agent_registry'::regclass
    ) THEN
        ALTER TABLE elitea_lifecycle.agent_registry
            ADD CONSTRAINT agent_registry_under_review_since
            CHECK (NOT under_review OR under_review_since IS NOT NULL);
    END IF;
END
$$;

-- The admission's read: is any of these agents retired.
CREATE INDEX IF NOT EXISTS agent_registry_retired
    ON elitea_lifecycle.agent_registry (tenant_project_id, application_id)
    WHERE lifecycle_state <> 'active';

CREATE TABLE IF NOT EXISTS elitea_lifecycle.agent_scorecards (
    project_id               bigint NOT NULL CHECK (project_id > 0),
    application_id           bigint NOT NULL CHECK (application_id > 0),
    day                      date NOT NULL,
    runs_30d                 integer NOT NULL DEFAULT 0 CHECK (runs_30d >= 0),
    failed_30d               integer NOT NULL DEFAULT 0 CHECK (failed_30d >= 0),
    cap_reached_30d          integer NOT NULL DEFAULT 0 CHECK (cap_reached_30d >= 0),
    breaker_trips_7d         integer NOT NULL DEFAULT 0 CHECK (breaker_trips_7d >= 0),
    approval_rejections_30d  integer NOT NULL DEFAULT 0 CHECK (approval_rejections_30d >= 0),
    guardrail_hits_30d       integer NOT NULL DEFAULT 0 CHECK (guardrail_hits_30d >= 0),
    spend_30d_usd            double precision NOT NULL DEFAULT 0 CHECK (spend_30d_usd >= 0),
    project_median_spend_usd double precision NOT NULL DEFAULT 0 CHECK (project_median_spend_usd >= 0),
    thumbs_up_30d            integer NOT NULL DEFAULT 0 CHECK (thumbs_up_30d >= 0),
    thumbs_down_30d          integer NOT NULL DEFAULT 0 CHECK (thumbs_down_30d >= 0),
    last_used_at             timestamptz,
    certification_state      text NOT NULL DEFAULT 'uncertified',
    computed_at              timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (project_id, application_id, day)
);

-- The prune.
CREATE INDEX IF NOT EXISTS agent_scorecards_day
    ON elitea_lifecycle.agent_scorecards (day);

CREATE TABLE IF NOT EXISTS elitea_lifecycle.agent_workforce_proposals (
    id                       bigserial PRIMARY KEY,
    project_id               bigint NOT NULL CHECK (project_id > 0),
    -- The agent the proposal is about.
    application_id           bigint NOT NULL CHECK (application_id > 0),
    kind                     text NOT NULL CHECK (kind IN ('dismiss', 'demote')),
    stage                    text NOT NULL CHECK (stage IN ('watch', 'demote', 'suspend', 'retire', 'delete')),
    -- person: a person with agents.workforce.manage or the owner;
    -- hr_agent: an HR agent's propose_dismissal (its run and agent below).
    source                   text NOT NULL CHECK (source IN ('person', 'hr_agent')),
    proposed_by_user_id      bigint CHECK (proposed_by_user_id IS NULL OR proposed_by_user_id > 0),
    proposer_application_id  bigint CHECK (proposer_application_id IS NULL OR proposer_application_id > 0),
    execution_id             text CHECK (execution_id IS NULL OR char_length(execution_id) BETWEEN 1 AND 256),
    rationale                text NOT NULL CHECK (btrim(rationale) <> '' AND octet_length(rationale) <= 4096),
    -- The exact narrowing of a demotion ({} for every other stage).
    narrowing                jsonb NOT NULL DEFAULT '{}'::jsonb
                             CHECK (jsonb_typeof(narrowing) = 'object' AND octet_length(narrowing::text) <= 8192),
    -- The scorecard snapshot the proposal is based on, and its SHA-256.
    evidence                 jsonb NOT NULL DEFAULT '{}'::jsonb
                             CHECK (jsonb_typeof(evidence) = 'object' AND octet_length(evidence::text) <= 16384),
    evidence_digest          text NOT NULL CHECK (evidence_digest ~ '^[0-9a-f]{64}$'),
    -- proposed (open) | asked_owner (open, routed to the owner for comment)
    -- | approved (the stage applied) | rejected.
    status                   text NOT NULL DEFAULT 'proposed'
                             CHECK (status IN ('proposed', 'asked_owner', 'approved', 'rejected')),
    owner_comment            text CHECK (owner_comment IS NULL OR octet_length(owner_comment) <= 4096),
    decided_by_user_id       bigint CHECK (decided_by_user_id IS NULL OR decided_by_user_id > 0),
    decided_at               timestamptz,
    decision_note            text CHECK (decision_note IS NULL OR octet_length(decision_note) <= 2048),
    -- What applying the stage did: the suspension, the change request and
    -- version of a demotion, the counts of a retirement.
    applied                  jsonb CHECK (applied IS NULL OR jsonb_typeof(applied) = 'object'),
    -- The owner's appeal (within 30 days of the decision) and its review by
    -- an agent manager other than the one who decided.
    appeal_state             text CHECK (appeal_state IS NULL OR appeal_state IN ('open', 'granted', 'upheld')),
    appeal_reason            text CHECK (appeal_reason IS NULL OR octet_length(appeal_reason) <= 4096),
    appealed_by_user_id      bigint CHECK (appealed_by_user_id IS NULL OR appealed_by_user_id > 0),
    appealed_at              timestamptz,
    appeal_decided_by_user_id bigint CHECK (appeal_decided_by_user_id IS NULL OR appeal_decided_by_user_id > 0),
    appeal_decided_at        timestamptz,
    appeal_note              text CHECK (appeal_note IS NULL OR octet_length(appeal_note) <= 2048),
    created_at               timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at               timestamptz NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT agent_workforce_proposals_kind_stage CHECK ((kind = 'demote') = (stage = 'demote')),
    CONSTRAINT agent_workforce_proposals_proposer CHECK (
        (source = 'person' AND proposed_by_user_id IS NOT NULL AND proposer_application_id IS NULL)
        OR (source = 'hr_agent' AND proposer_application_id IS NOT NULL AND execution_id IS NOT NULL)
    ),
    -- An agent never proposes about itself.
    CONSTRAINT agent_workforce_proposals_not_self CHECK (
        proposer_application_id IS NULL OR proposer_application_id <> application_id
    ),
    CONSTRAINT agent_workforce_proposals_decision CHECK (
        (status IN ('proposed', 'asked_owner') AND decided_at IS NULL)
        OR (status IN ('approved', 'rejected') AND decided_at IS NOT NULL AND decided_by_user_id IS NOT NULL)
    ),
    CONSTRAINT agent_workforce_proposals_appeal CHECK (
        appeal_state IS NULL
        OR (status = 'approved' AND appealed_at IS NOT NULL AND appealed_by_user_id IS NOT NULL
            AND (appeal_state = 'open') = (appeal_decided_at IS NULL)
            AND (appeal_decided_by_user_id IS NULL OR appeal_decided_by_user_id <> decided_by_user_id))
    )
);

-- At most one open proposal per agent.
CREATE UNIQUE INDEX IF NOT EXISTS agent_workforce_proposals_one_open
    ON elitea_lifecycle.agent_workforce_proposals (project_id, application_id)
    WHERE status IN ('proposed', 'asked_owner');

-- The inbox and an agent's page.
CREATE INDEX IF NOT EXISTS agent_workforce_proposals_project
    ON elitea_lifecycle.agent_workforce_proposals (project_id, status, created_at DESC);
CREATE INDEX IF NOT EXISTS agent_workforce_proposals_agent
    ON elitea_lifecycle.agent_workforce_proposals (project_id, application_id, created_at DESC);

COMMENT ON TABLE elitea_lifecycle.agent_workforce_proposals IS
    'A dismissal or demotion proposal (ADR-0050 decision 5i). An agent never decides one: a person approves, rejects or asks the owner.';

-- The ladder's Suspend stage writes source dismissal.
DO $$
BEGIN
    IF to_regclass('elitea_runtime.execution_suspensions') IS NULL THEN
        RETURN;
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'execution_suspensions_source_known'
          AND conrelid = 'elitea_runtime.execution_suspensions'::regclass
    ) THEN
        ALTER TABLE elitea_runtime.execution_suspensions
            DROP CONSTRAINT IF EXISTS execution_suspensions_source_check;
        ALTER TABLE elitea_runtime.execution_suspensions
            ADD CONSTRAINT execution_suspensions_source_known
            CHECK (source IN ('manual', 'breaker', 'certification', 'self_test', 'dismissal'));
    END IF;
END
$$;

DO $$
BEGIN
    IF to_regclass('public.auth_core__role') IS NULL
       OR to_regclass('public.auth_core__role_permission') IS NULL THEN
        RAISE NOTICE '0236: auth_core tables absent, nothing to grant';
        RETURN;
    END IF;

    -- The mode spelled as a literal: the router's grant gate
    -- (router_permission_grant_gate_test.go) reads the grants of this
    -- directory per `mode = '...'` block.
    INSERT INTO public.auth_core__role_permission (role_id, permission)
    SELECT role.id, 'agents.workforce.manage'
    FROM public.auth_core__role AS role
    WHERE role.mode = 'default' AND role.name = 'admin'
    ON CONFLICT (role_id, permission) DO NOTHING;

    IF to_regclass('public.auth_core__project_role') IS NULL
       OR to_regclass('public.auth_core__project_role_permission') IS NULL THEN
        RETURN;
    END IF;

    INSERT INTO public.auth_core__project_role_permission (project_id, role_id, permission)
    SELECT DISTINCT saved.project_id, saved.role_id, 'agents.workforce.manage'
    FROM (
        SELECT DISTINCT project_id, role_id
        FROM public.auth_core__project_role_permission
        WHERE role_id IS NOT NULL
    ) AS saved
    JOIN public.auth_core__project_role AS project_role
      ON project_role.id = saved.role_id
     AND project_role.project_id = saved.project_id
    WHERE project_role.name = 'admin'
    ON CONFLICT (project_id, role_id, permission) DO NOTHING;
END
$$;
