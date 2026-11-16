-- 0253_marketplace_workforce.sql — workforce managers on the Marketplace
-- (ADR-0051 decision 6, ADR-0050 decision 5, issue 1632, phase M6).
--
-- A workforce manager (an agent whose published version sets
-- capabilities.can_review_agents) gets four Marketplace tools. Two of them
-- leave a proposal a person decides, in the table that already holds the
-- dismissal and demotion proposals:
--
--   kind hire_listing     propose_hire: hire a Marketplace listing version.
--                         There is no agent yet, so application_id is NULL.
--                         listing_id and listing_version_id name what to hire;
--                         payload holds the mode and the bindings hint. A
--                         person opens the hire wizard filled in from it and
--                         the hire settles the proposal (approved). Under the
--                         auto_hire policy the platform hires it itself.
--   kind feedback_report  suggest_feedback: a workforce report on a hired
--                         agent's dismissal or demotion. application_id is the
--                         hired agent, stage the ladder stage the report is
--                         about, payload the outcome, the reason code and the
--                         platform-filled scorecard summary, rationale the
--                         agent's text. It publishes to the listing version
--                         only when a person with agents.workforce.manage
--                         approves it (or under agents.workforce.auto_feedback).
--                         The report endpoint takes this proposal's id, so a
--                         report cannot name an agent that did not draft it.
--
-- The dismissal and demotion kinds, their stage rule and the one-open-proposal
-- index are unchanged: that index now covers those two kinds only, because a
-- feedback report is drafted while the dismissal of the same agent is open.
-- A feedback report is unique per hired agent and stage while it is open or
-- published; a hire proposal is unique per listing while it is open.
--
-- agent_workforce_policies gains the auto_feedback switch (and the person who
-- turned it on, whom an automatic report is recorded under), the person who
-- turned auto_hire on (whom an automatic hire of a listing is made for, and
-- recorded under, however many people save the policy later: backfilled from
-- updated_by for a policy that is on) and the three numbers the auto_hire rule
-- reads for a listing: min_reviews, min_rating and max_dismissal_rate
-- (ADR-0051 decision 6).
--
-- No new permission: deciding these proposals needs agents.workforce.manage,
-- granted by 0236. Additive only. Idempotent. No BEGIN/COMMIT: the ledgered
-- runner wraps the file.

DO $$
BEGIN
    IF to_regclass('elitea_lifecycle.agent_workforce_proposals') IS NULL THEN
        RETURN;
    END IF;

    ALTER TABLE elitea_lifecycle.agent_workforce_proposals
        ALTER COLUMN application_id DROP NOT NULL,
        ALTER COLUMN stage DROP NOT NULL,
        ADD COLUMN IF NOT EXISTS listing_id bigint CHECK (listing_id IS NULL OR listing_id > 0),
        ADD COLUMN IF NOT EXISTS listing_version_id bigint CHECK (listing_version_id IS NULL OR listing_version_id > 0),
        ADD COLUMN IF NOT EXISTS payload jsonb NOT NULL DEFAULT '{}'::jsonb
            CHECK (jsonb_typeof(payload) = 'object' AND octet_length(payload::text) <= 16384);

    ALTER TABLE elitea_lifecycle.agent_workforce_proposals
        DROP CONSTRAINT IF EXISTS agent_workforce_proposals_kind_check,
        DROP CONSTRAINT IF EXISTS agent_workforce_proposals_stage_check,
        DROP CONSTRAINT IF EXISTS agent_workforce_proposals_kind_stage,
        DROP CONSTRAINT IF EXISTS agent_workforce_proposals_marketplace_shape;

    ALTER TABLE elitea_lifecycle.agent_workforce_proposals
        ADD CONSTRAINT agent_workforce_proposals_kind_check
            CHECK (kind IN ('dismiss', 'demote', 'hire_listing', 'feedback_report')),
        ADD CONSTRAINT agent_workforce_proposals_stage_check
            CHECK (stage IS NULL OR stage IN ('watch', 'demote', 'suspend', 'retire', 'delete')),
        -- The ladder kinds keep their rule: a stage, and demote only for demote.
        ADD CONSTRAINT agent_workforce_proposals_kind_stage CHECK (
            kind NOT IN ('dismiss', 'demote')
            OR (application_id IS NOT NULL AND stage IS NOT NULL AND ((kind = 'demote') = (stage = 'demote')))
        ),
        -- The Marketplace kinds are a workforce manager's, name their listing
        -- and are never automatic ladder stages.
        ADD CONSTRAINT agent_workforce_proposals_marketplace_shape CHECK (
            kind NOT IN ('hire_listing', 'feedback_report')
            OR (source = 'workforce_manager' AND listing_id IS NOT NULL AND listing_version_id IS NOT NULL
                AND ((kind = 'hire_listing' AND application_id IS NULL AND stage IS NULL)
                     OR (kind = 'feedback_report' AND application_id IS NOT NULL AND stage IS NOT NULL)))
        );

    -- One open ladder proposal per agent, as before; only the ladder kinds.
    DROP INDEX IF EXISTS elitea_lifecycle.agent_workforce_proposals_one_open;
    CREATE UNIQUE INDEX agent_workforce_proposals_one_open
        ON elitea_lifecycle.agent_workforce_proposals (project_id, application_id)
        WHERE status IN ('proposed', 'asked_owner') AND kind IN ('dismiss', 'demote');

    -- One feedback report per hired agent and stage, while open or published.
    CREATE UNIQUE INDEX IF NOT EXISTS agent_workforce_proposals_one_report
        ON elitea_lifecycle.agent_workforce_proposals (project_id, application_id, stage)
        WHERE kind = 'feedback_report' AND status IN ('proposed', 'asked_owner', 'approved');

    -- One open hire proposal per listing and project.
    CREATE UNIQUE INDEX IF NOT EXISTS agent_workforce_proposals_one_hire
        ON elitea_lifecycle.agent_workforce_proposals (project_id, listing_id)
        WHERE kind = 'hire_listing' AND status IN ('proposed', 'asked_owner');

    CREATE INDEX IF NOT EXISTS agent_workforce_proposals_kind
        ON elitea_lifecycle.agent_workforce_proposals (project_id, kind, status, created_at DESC)
        WHERE kind IN ('hire_listing', 'feedback_report');
END
$$;

DO $$
BEGIN
    IF to_regclass('elitea_lifecycle.agent_workforce_policies') IS NULL THEN
        RETURN;
    END IF;

    ALTER TABLE elitea_lifecycle.agent_workforce_policies
        ADD COLUMN IF NOT EXISTS auto_feedback_enabled boolean NOT NULL DEFAULT false,
        ADD COLUMN IF NOT EXISTS auto_feedback_by bigint CHECK (auto_feedback_by IS NULL OR auto_feedback_by > 0),
        ADD COLUMN IF NOT EXISTS auto_hire_by bigint CHECK (auto_hire_by IS NULL OR auto_hire_by > 0),
        ADD COLUMN IF NOT EXISTS listing_min_reviews integer NOT NULL DEFAULT 3
            CHECK (listing_min_reviews BETWEEN 1 AND 1000),
        ADD COLUMN IF NOT EXISTS listing_min_rating numeric(2, 1) NOT NULL DEFAULT 4.0
            CHECK (listing_min_rating BETWEEN 1.0 AND 5.0),
        ADD COLUMN IF NOT EXISTS listing_max_dismissal_rate numeric(3, 2) NOT NULL DEFAULT 0.20
            CHECK (listing_max_dismissal_rate BETWEEN 0 AND 1);

    UPDATE elitea_lifecycle.agent_workforce_policies
       SET auto_hire_by = updated_by
     WHERE auto_hire_enabled AND auto_hire_by IS NULL AND updated_by IS NOT NULL;
END
$$;
