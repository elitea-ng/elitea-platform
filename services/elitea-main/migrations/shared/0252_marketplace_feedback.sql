-- 0252_marketplace_feedback.sql — feedback per listing version, the daily
-- signals, and the dismissal record they need (ADR-0051 decision 5, issue 1631,
-- phase M5).
--
-- listing_feedback holds the three kinds of feedback a listing version gets:
--   review           a person in a project that hired the listing: a rating
--                    from 1 to 5 and optional text. One per person and listing,
--                    editable.
--   workforce_report an agent's report on a dismissal or demotion, with the
--                    person who approved it, or, under the auto_feedback
--                    policy, the person who set the policy (author_kind
--                    'policy', author_label 'policy:auto_feedback'). It
--                    carries the platform-filled scorecard summary and never a
--                    transcript, a prompt, tool arguments or memory.
--   signal           reserved for a platform-written note. The daily signals
--                    themselves are in listing_signals_daily.
--
-- project_id is stored so a project can manage its own report and so the
-- one-report-per-hired-agent-and-stage guard holds. The API never returns it,
-- and never returns a project name (owner question Q4).
--
-- A publisher answers an item once (reply_*) and may flag it (flagged_*). A
-- platform administrator hides an item (state 'hidden', hidden_*). Every one
-- of those actions writes a row to listing_feedback_events in the same
-- transaction: the audit trail of moderation.
--
-- installs.dismissed_at records that the hired agent was removed from its
-- project, so the active hires, the 30-day retention and the dismissal rate
-- can be told apart from the hires that stayed. A row that is never dismissed
-- is an active hire.
--
-- The ratings in elitea_marketplace.listings (rating_avg, rating_count) are
-- kept by the service from the visible reviews.
--
-- No permission: moderation uses the admin published agents permission, and
-- reading and reviewing use the project's agent permissions. No BEGIN/COMMIT:
-- the ledgered runner wraps the file.

ALTER TABLE elitea_marketplace.installs
    ADD COLUMN IF NOT EXISTS dismissed_at timestamptz;
ALTER TABLE elitea_marketplace.installs
    ADD COLUMN IF NOT EXISTS dismissed_by bigint CHECK (dismissed_by IS NULL OR dismissed_by > 0);

CREATE INDEX IF NOT EXISTS installs_active
    ON elitea_marketplace.installs (listing_id, version_id) WHERE dismissed_at IS NULL;

CREATE TABLE IF NOT EXISTS elitea_marketplace.listing_feedback (
    id               bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    listing_id       bigint NOT NULL,
    version_id       bigint NOT NULL,
    kind             text NOT NULL CHECK (kind IN ('review', 'workforce_report', 'signal')),
    rating           smallint CHECK (rating IS NULL OR rating BETWEEN 1 AND 5),
    body             text NOT NULL DEFAULT '' CHECK (length(body) <= 2000),
    -- The report's facts: outcome (kept, demoted, dismissed), the stage and the
    -- reason code. Empty for a review.
    outcome          text CHECK (outcome IS NULL OR outcome IN ('kept', 'demoted', 'dismissed')),
    stage            text NOT NULL DEFAULT '' CHECK (length(stage) <= 64),
    reason_code      text NOT NULL DEFAULT '' CHECK (length(reason_code) <= 64),
    -- Failure rate, breaker trips, cost against median and active days: filled
    -- by the platform, never by the agent.
    scorecard        jsonb NOT NULL DEFAULT '{}'::jsonb,
    author_kind      text NOT NULL CHECK (author_kind IN ('person', 'agent', 'policy', 'platform')),
    -- The person: the reviewer, the approving person of an agent's report, or
    -- the person who set the policy.
    author_user_id   bigint CHECK (author_user_id IS NULL OR author_user_id > 0),
    author_agent_id  bigint CHECK (author_agent_id IS NULL OR author_agent_id > 0),
    author_label     text NOT NULL DEFAULT '' CHECK (length(author_label) <= 64),
    -- The hiring project. Stored, never returned.
    project_id       bigint NOT NULL CHECK (project_id > 0),
    -- The hired agent a report is about.
    hired_entity_id  bigint CHECK (hired_entity_id IS NULL OR hired_entity_id > 0),
    state            text NOT NULL DEFAULT 'visible' CHECK (state IN ('visible', 'hidden')),
    hidden_at        timestamptz,
    hidden_by        bigint CHECK (hidden_by IS NULL OR hidden_by > 0),
    hide_reason      text NOT NULL DEFAULT '' CHECK (length(hide_reason) <= 500),
    flagged_at       timestamptz,
    flagged_by       bigint CHECK (flagged_by IS NULL OR flagged_by > 0),
    flag_reason      text NOT NULL DEFAULT '' CHECK (length(flag_reason) <= 500),
    reply_body       text CHECK (reply_body IS NULL OR length(reply_body) BETWEEN 1 AND 2000),
    replied_at       timestamptz,
    replied_by       bigint CHECK (replied_by IS NULL OR replied_by > 0),
    created_at       timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at       timestamptz NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT listing_feedback_version_fk FOREIGN KEY (listing_id, version_id)
        REFERENCES elitea_marketplace.listing_versions (listing_id, version_id) ON DELETE CASCADE,
    CONSTRAINT listing_feedback_review_shape CHECK (
        kind <> 'review' OR (author_kind = 'person' AND author_user_id IS NOT NULL AND rating IS NOT NULL)),
    CONSTRAINT listing_feedback_report_shape CHECK (
        kind <> 'workforce_report' OR (
            author_kind IN ('agent', 'policy') AND author_user_id IS NOT NULL
            AND author_agent_id IS NOT NULL AND hired_entity_id IS NOT NULL AND outcome IS NOT NULL
            AND rating IS NULL
            AND (author_kind <> 'policy' OR author_label = 'policy:auto_feedback'))),
    CONSTRAINT listing_feedback_reply_whole CHECK (
        (reply_body IS NULL) = (replied_at IS NULL))
);

-- One review per person and listing, and one report per hired agent and stage.
CREATE UNIQUE INDEX IF NOT EXISTS listing_feedback_one_review
    ON elitea_marketplace.listing_feedback (listing_id, author_user_id) WHERE kind = 'review';
CREATE UNIQUE INDEX IF NOT EXISTS listing_feedback_one_report
    ON elitea_marketplace.listing_feedback (listing_id, project_id, hired_entity_id, stage)
    WHERE kind = 'workforce_report';
CREATE INDEX IF NOT EXISTS listing_feedback_by_version
    ON elitea_marketplace.listing_feedback (listing_id, version_id, created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS listing_feedback_flagged
    ON elitea_marketplace.listing_feedback (flagged_at DESC) WHERE flagged_at IS NOT NULL;

-- The audit trail of moderation and replies. Rows are only ever added.
CREATE TABLE IF NOT EXISTS elitea_marketplace.listing_feedback_events (
    id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    feedback_id   bigint NOT NULL,
    listing_id    bigint NOT NULL,
    action        text NOT NULL CHECK (action IN
        ('created', 'edited', 'reply', 'flag', 'unflag', 'hide', 'unhide')),
    actor_user_id bigint NOT NULL CHECK (actor_user_id > 0),
    reason        text NOT NULL DEFAULT '' CHECK (length(reason) <= 500),
    created_at    timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE INDEX IF NOT EXISTS listing_feedback_events_item
    ON elitea_marketplace.listing_feedback_events (feedback_id, created_at DESC);

-- The signals, computed daily by main.marketplace_signals. version_id 0 is the
-- whole listing. The rates are not stored: they are counts the reader divides.
--   active_hires      installs that are not dismissed
--   hires_total       every install ever recorded
--   dismissed         installs whose agent was dismissed
--   cohort_30d        installs at least 30 days old
--   retained_30d      of those, the ones still active on day 30
--   upgrade_eligible  active installs hired before the current version
--   upgrade_adopted   of those, the ones now on the current version
CREATE TABLE IF NOT EXISTS elitea_marketplace.listing_signals_daily (
    listing_id        bigint NOT NULL REFERENCES elitea_marketplace.listings (id) ON DELETE CASCADE,
    version_id        bigint NOT NULL CHECK (version_id >= 0),
    day               date NOT NULL,
    active_hires      integer NOT NULL CHECK (active_hires >= 0),
    hires_total       integer NOT NULL CHECK (hires_total >= 0),
    dismissed         integer NOT NULL CHECK (dismissed >= 0),
    cohort_30d        integer NOT NULL CHECK (cohort_30d >= 0),
    retained_30d      integer NOT NULL CHECK (retained_30d >= 0),
    upgrade_eligible  integer NOT NULL DEFAULT 0 CHECK (upgrade_eligible >= 0),
    upgrade_adopted   integer NOT NULL DEFAULT 0 CHECK (upgrade_adopted >= 0),
    computed_at       timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (listing_id, version_id, day)
);
