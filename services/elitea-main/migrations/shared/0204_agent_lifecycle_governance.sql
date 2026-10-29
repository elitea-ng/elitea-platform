-- 0204_agent_lifecycle_governance.sql — the agent registry, change
-- requests with maker-checker approval, recertification and attestations
-- (ADR-0040 decisions 1, 3, 5 and 6, issue #1497).
--
--   elitea_lifecycle.agent_registry
--       One row per application (agent or pipeline): business and technical
--       owner, risk tier, intended and out-of-scope use, data classes and
--       the certification state with the certified release and its
--       recertify_by deadline. Application ids are per tenant schema, so
--       the key is (tenant_project_id, application_id), as in 0198. The
--       application save creates the row; a read of an agent without one
--       creates it (tier low, the default of a new agent).
--
--   elitea_lifecycle.change_requests
--       A request to publish a draft version as a release (kind release),
--       to certify the published release again (recertification), or the
--       record of a version published before change control
--       (legacy_unreviewed, written here only). One open request (draft or
--       submitted) per agent. The final approval publishes the candidate
--       in its own transaction: state published, release_id set.
--
--   elitea_lifecycle.change_request_approvals
--       One row per approval. The approver differs from the proposer, in
--       code and in the CHECK below; the proposer column is tied to the
--       request's own by a composite foreign key, so a writer cannot carry
--       a different proposer to pass the CHECK (the 0184 shape).
--
--   elitea_lifecycle.attestations
--       A review done outside the platform (architecture, legal, model
--       risk, data owner, data accuracy, custom): its subject (an agent, a
--       release, a toolkit or an index), the attester, valid_until, an
--       evidence link and a note. Never deleted; a revocation is stamped.
--
--   elitea_lifecycle.lifecycle_backfills
--       Marks a one-time backfill done, so a re-application of this file
--       (RunMigrations applies it unledgered, then elitea-migrate records
--       it) never backfills agents created in between as medium.
--
-- Backfill (once): every existing application gets a registry row,
-- uncertified, tier medium, except the applications of a personal project
-- (project_user_<id>, one member, so four eyes are impossible), which get
-- tier low. The technical owner is the author of the application's first
-- version. Every version published now gets a legacy_unreviewed change
-- request in state published, linked to its release when 0198 recorded one.
--
-- Permissions: models.applications.governance (the governance role) goes
-- to the platform administrators (administration mode: super_admin, admin)
-- and to the project admin (default mode); lifecycle.attestations.write and
-- lifecycle.registry.mark_due go to the project admin. 0060 returns early on
-- an existing database, so the grants are here and in 001_initial.sql.
--
-- Every statement is guarded: the file tolerates a database without the
-- tenant schemas, centry.project or the auth_core tables, and one that
-- already holds its objects. No BEGIN/COMMIT: the ledgered runner wraps the
-- file.

CREATE SCHEMA IF NOT EXISTS elitea_lifecycle;

CREATE TABLE IF NOT EXISTS elitea_lifecycle.agent_registry (
    tenant_project_id     bigint NOT NULL CHECK (tenant_project_id > 0),
    application_id        bigint NOT NULL CHECK (application_id > 0),
    business_owner_id     bigint CHECK (business_owner_id IS NULL OR business_owner_id > 0),
    technical_owner_id    bigint CHECK (technical_owner_id IS NULL OR technical_owner_id > 0),
    risk_tier             text NOT NULL DEFAULT 'low'
                          CHECK (risk_tier IN ('low', 'medium', 'high', 'critical')),
    intended_use          text NOT NULL DEFAULT '' CHECK (length(intended_use) <= 2000),
    out_of_scope_use      text NOT NULL DEFAULT '' CHECK (length(out_of_scope_use) <= 2000),
    data_classes          text[] NOT NULL DEFAULT '{}' CHECK (cardinality(data_classes) <= 32),
    certification_state   text NOT NULL DEFAULT 'uncertified'
                          CHECK (certification_state IN ('uncertified', 'certified', 'due', 'lapsed', 'suspended')),
    certified_release_id  bigint,
    certified_at          timestamptz,
    recertify_by          timestamptz,
    state_reason          text NOT NULL DEFAULT '' CHECK (length(state_reason) <= 500),
    state_changed_at      timestamptz NOT NULL DEFAULT clock_timestamp(),
    -- The recertification routine's last drift check of the certified
    -- release (oldest first, a bounded batch per pass).
    drift_checked_at      timestamptz,
    created_at            timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at            timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (tenant_project_id, application_id),
    CONSTRAINT agent_registry_certified_release_same_application
        FOREIGN KEY (certified_release_id, tenant_project_id, application_id)
        REFERENCES elitea_lifecycle.application_releases (id, tenant_project_id, application_id),
    -- Every state but uncertified names the release that was certified.
    CONSTRAINT agent_registry_certified_has_release CHECK (
        certification_state = 'uncertified'
        OR (certified_release_id IS NOT NULL AND certified_at IS NOT NULL)
    )
);

-- The recertification routine's scan and the admin filters.
CREATE INDEX IF NOT EXISTS agent_registry_review
    ON elitea_lifecycle.agent_registry (certification_state, recertify_by)
    WHERE certification_state <> 'uncertified';
CREATE INDEX IF NOT EXISTS agent_registry_tier
    ON elitea_lifecycle.agent_registry (risk_tier, tenant_project_id);

CREATE TABLE IF NOT EXISTS elitea_lifecycle.change_requests (
    id                             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_project_id              bigint NOT NULL CHECK (tenant_project_id > 0),
    application_id                 bigint NOT NULL CHECK (application_id > 0),
    kind                           text NOT NULL
                                   CHECK (kind IN ('release', 'recertification', 'legacy_unreviewed')),
    state                          text NOT NULL DEFAULT 'draft'
                                   CHECK (state IN ('draft', 'submitted', 'approved', 'rejected', 'withdrawn', 'published')),
    -- The tier when the request was created; the requirement is read again
    -- at submit from the live tier and the platform's tier table.
    risk_tier                      text NOT NULL
                                   CHECK (risk_tier IN ('low', 'medium', 'high', 'critical')),
    candidate_version_id           bigint CHECK (candidate_version_id IS NULL OR candidate_version_id > 0),
    -- The draft revision (0198 version_revisions.id) submitted. An approval
    -- after a later save is refused: approvers approve what they saw.
    candidate_revision_id          bigint CHECK (candidate_revision_id IS NULL OR candidate_revision_id > 0),
    -- SHA-256 over what the release would record (the definition digest
    -- and the toolkit pins, so toolkit, skill and sub-agent attachments
    -- too), taken at submit and recomputed at the final approval: an
    -- attachment edit leaves version_revisions alone but changes this.
    candidate_digest               bytea CHECK (candidate_digest IS NULL OR length(candidate_digest) = 32),
    version_name                   text CHECK (version_name IS NULL OR length(version_name) BETWEEN 1 AND 128),
    category                       text NOT NULL DEFAULT '' CHECK (length(category) <= 64),
    summary                        text NOT NULL CHECK (length(summary) BETWEEN 1 AND 2000),
    proposed_by                    bigint CHECK (proposed_by IS NULL OR proposed_by > 0),
    -- A direct publish of a tier that needs no approval records its own
    -- change request, approved by the publisher alone.
    self_approved                  boolean NOT NULL DEFAULT false,
    required_approvals             integer NOT NULL DEFAULT 0 CHECK (required_approvals BETWEEN 0 AND 5),
    required_governance_approvals  integer NOT NULL DEFAULT 0
                                   CHECK (required_governance_approvals BETWEEN 0 AND required_approvals),
    -- The evaluation gate (ADR-0040 decision 4); NULL when none ran.
    gate_id                        bigint CHECK (gate_id IS NULL OR gate_id > 0),
    override_reason                text CHECK (override_reason IS NULL OR length(override_reason) BETWEEN 1 AND 500),
    overridden_by                  bigint CHECK (overridden_by IS NULL OR overridden_by > 0),
    overridden_at                  timestamptz,
    -- The published release when the request was submitted (the gate's
    -- regression baseline), and the release the request published or
    -- certified.
    current_release_id             bigint CHECK (current_release_id IS NULL OR current_release_id > 0),
    release_id                     bigint,
    decided_by                     bigint CHECK (decided_by IS NULL OR decided_by > 0),
    decided_at                     timestamptz,
    decision_reason                text NOT NULL DEFAULT '' CHECK (length(decision_reason) <= 500),
    created_at                     timestamptz NOT NULL DEFAULT clock_timestamp(),
    submitted_at                   timestamptz,
    published_at                   timestamptz,
    updated_at                     timestamptz NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT change_requests_proposer_key UNIQUE (id, proposed_by),
    CONSTRAINT change_requests_release_same_application
        FOREIGN KEY (release_id, tenant_project_id, application_id)
        REFERENCES elitea_lifecycle.application_releases (id, tenant_project_id, application_id),
    CONSTRAINT change_requests_proposer_known CHECK (
        proposed_by IS NOT NULL OR kind = 'legacy_unreviewed' OR self_approved
    ),
    CONSTRAINT change_requests_release_has_candidate CHECK (
        kind <> 'release' OR (candidate_version_id IS NOT NULL AND version_name IS NOT NULL)
    ),
    CONSTRAINT change_requests_override_complete CHECK (
        (override_reason IS NULL) = (overridden_by IS NULL)
        AND (overridden_by IS NULL) = (overridden_at IS NULL)
    ),
    -- A published request names its release, except a legacy record of a
    -- version published before 0198 recorded releases.
    CONSTRAINT change_requests_published_has_release CHECK (
        state NOT IN ('published', 'approved') OR release_id IS NOT NULL OR kind = 'legacy_unreviewed'
    )
);

-- One open change request per agent.
CREATE UNIQUE INDEX IF NOT EXISTS change_requests_one_open
    ON elitea_lifecycle.change_requests (tenant_project_id, application_id)
    WHERE state IN ('draft', 'submitted');
-- One legacy record per published version (the backfill is re-runnable).
CREATE UNIQUE INDEX IF NOT EXISTS change_requests_legacy_once
    ON elitea_lifecycle.change_requests (tenant_project_id, candidate_version_id)
    WHERE kind = 'legacy_unreviewed';
CREATE INDEX IF NOT EXISTS change_requests_application
    ON elitea_lifecycle.change_requests (tenant_project_id, application_id, id DESC);

CREATE TABLE IF NOT EXISTS elitea_lifecycle.change_request_approvals (
    id                     bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    change_request_id      bigint NOT NULL,
    proposed_by            bigint NOT NULL,
    approver_id            bigint NOT NULL CHECK (approver_id > 0),
    holds_governance_role  boolean NOT NULL DEFAULT false,
    note                   text NOT NULL DEFAULT '' CHECK (length(note) <= 1000),
    created_at             timestamptz NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT change_request_approvals_request_proposer
        FOREIGN KEY (change_request_id, proposed_by)
        REFERENCES elitea_lifecycle.change_requests (id, proposed_by),
    CONSTRAINT change_request_approvals_approver_not_proposer
        CHECK (approver_id <> proposed_by),
    CONSTRAINT change_request_approvals_one_per_approver
        UNIQUE (change_request_id, approver_id)
);

CREATE TABLE IF NOT EXISTS elitea_lifecycle.attestations (
    id                bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_project_id bigint NOT NULL CHECK (tenant_project_id > 0),
    kind              text NOT NULL
                      CHECK (kind IN ('architecture', 'legal', 'model_risk', 'data_owner', 'data_accuracy', 'custom')),
    custom_kind       text NOT NULL DEFAULT '' CHECK (length(custom_kind) <= 64),
    subject_type      text NOT NULL CHECK (subject_type IN ('agent', 'release', 'toolkit', 'index')),
    subject_id        text NOT NULL CHECK (subject_id ~ '^[A-Za-z0-9-]{1,64}$'),
    attester          text NOT NULL CHECK (length(attester) BETWEEN 1 AND 256),
    attester_user_id  bigint CHECK (attester_user_id IS NULL OR attester_user_id > 0),
    valid_until       timestamptz,
    evidence_url      text NOT NULL DEFAULT '' CHECK (length(evidence_url) <= 2048),
    note              text NOT NULL DEFAULT '' CHECK (length(note) <= 2000),
    recorded_by       bigint NOT NULL CHECK (recorded_by > 0),
    created_at        timestamptz NOT NULL DEFAULT clock_timestamp(),
    revoked_at        timestamptz,
    revoked_by        bigint CHECK (revoked_by IS NULL OR revoked_by > 0),
    revoke_reason     text CHECK (revoke_reason IS NULL OR length(revoke_reason) <= 500),
    CONSTRAINT attestations_custom_named CHECK ((kind = 'custom') = (custom_kind <> '')),
    CONSTRAINT attestations_revocation_complete CHECK ((revoked_at IS NULL) = (revoked_by IS NULL))
);

CREATE INDEX IF NOT EXISTS attestations_subject
    ON elitea_lifecycle.attestations (tenant_project_id, subject_type, subject_id, created_at DESC);

CREATE TABLE IF NOT EXISTS elitea_lifecycle.lifecycle_backfills (
    name     text PRIMARY KEY,
    done_at  timestamptz NOT NULL DEFAULT clock_timestamp()
);

DO $$
DECLARE
    tenant      record;
    schema_name text;
    personal    boolean;
    has_project boolean := to_regclass('centry.project') IS NOT NULL;
BEGIN
    IF EXISTS (SELECT 1 FROM elitea_lifecycle.lifecycle_backfills WHERE name = '0204_agent_registry') THEN
        RETURN;
    END IF;

    FOR tenant IN
        SELECT nspname, substring(nspname FROM 3)::bigint AS project_id
        FROM pg_namespace
        WHERE nspname ~ '^p_[0-9]{1,18}$'
        ORDER BY 2
    LOOP
        schema_name := tenant.nspname;
        IF tenant.project_id <= 0
           OR to_regclass(format('%I.applications', schema_name)) IS NULL
           OR to_regclass(format('%I.application_versions', schema_name)) IS NULL THEN
            CONTINUE;
        END IF;

        personal := false;
        IF has_project THEN
            EXECUTE 'SELECT EXISTS (SELECT 1 FROM centry.project WHERE id = $1 AND name LIKE ''project_user_%'')'
                INTO personal USING tenant.project_id;
        END IF;

        EXECUTE format($sql$
            INSERT INTO elitea_lifecycle.agent_registry
                (tenant_project_id, application_id, technical_owner_id, risk_tier, certification_state)
            SELECT $1, application.id,
                   (SELECT version.author_id FROM %1$I.application_versions AS version
                    WHERE version.application_id = application.id AND version.author_id > 0
                    ORDER BY version.id LIMIT 1),
                   $2, 'uncertified'
            FROM %1$I.applications AS application
            WHERE application.id > 0
            ON CONFLICT (tenant_project_id, application_id) DO NOTHING
        $sql$, schema_name)
        USING tenant.project_id, CASE WHEN personal THEN 'low' ELSE 'medium' END;

        EXECUTE format($sql$
            INSERT INTO elitea_lifecycle.change_requests
                (tenant_project_id, application_id, kind, state, risk_tier, candidate_version_id,
                 version_name, summary, proposed_by, release_id, created_at, submitted_at, published_at)
            SELECT $1, version.application_id, 'legacy_unreviewed', 'published', registry.risk_tier,
                   version.id, left(version.name, 128),
                   'Published before change control (ADR-0040). Not reviewed.',
                   NULL, release.id, clock_timestamp(), NULL, release.created_at
            FROM %1$I.application_versions AS version
            JOIN elitea_lifecycle.agent_registry AS registry
              ON registry.tenant_project_id = $1 AND registry.application_id = version.application_id
            LEFT JOIN LATERAL (
                SELECT r.id, r.created_at FROM elitea_lifecycle.application_releases AS r
                WHERE r.tenant_project_id = $1 AND r.application_id = version.application_id
                  AND r.application_version_id = version.id
                ORDER BY r.id DESC LIMIT 1
            ) AS release ON true
            WHERE version.status = 'published' AND version.id > 0 AND version.application_id > 0
              AND length(version.name) >= 1
            ON CONFLICT DO NOTHING
        $sql$, schema_name)
        USING tenant.project_id;
    END LOOP;

    INSERT INTO elitea_lifecycle.lifecycle_backfills (name) VALUES ('0204_agent_registry')
    ON CONFLICT (name) DO NOTHING;
END
$$;

DO $$
BEGIN
    IF to_regclass('public.auth_core__role') IS NULL
       OR to_regclass('public.auth_core__role_permission') IS NULL THEN
        RAISE NOTICE '0204: auth_core tables absent, nothing to grant';
        RETURN;
    END IF;

    INSERT INTO public.auth_core__role_permission (role_id, permission)
    SELECT role.id, grant_row.permission
    FROM public.auth_core__role AS role
    JOIN (VALUES
        ('administration', 'super_admin', 'models.applications.governance'),
        ('administration', 'admin', 'models.applications.governance'),
        ('default', 'admin', 'models.applications.governance'),
        ('default', 'admin', 'lifecycle.attestations.write'),
        ('default', 'admin', 'lifecycle.registry.mark_due')
    ) AS grant_row(mode, role_name, permission)
      ON grant_row.mode = role.mode AND grant_row.role_name = role.name
    ON CONFLICT (role_id, permission) DO NOTHING;

    -- legacyrbac reads the central grants only for a caller with no
    -- per-project rows (shared/0090). A project whose matrix was saved gets
    -- the strings on its `admin` role here, as 0194 does. The strings never
    -- existed, so no saved matrix withheld them on purpose.
    IF to_regclass('public.auth_core__project_role') IS NULL
       OR to_regclass('public.auth_core__project_role_permission') IS NULL THEN
        RETURN;
    END IF;

    INSERT INTO public.auth_core__project_role_permission (project_id, role_id, permission)
    SELECT DISTINCT saved.project_id, saved.role_id, grant_row.permission
    FROM (
        SELECT DISTINCT project_id, role_id
        FROM public.auth_core__project_role_permission
        WHERE role_id IS NOT NULL
    ) AS saved
    JOIN public.auth_core__project_role AS project_role
      ON project_role.id = saved.role_id
     AND project_role.project_id = saved.project_id
    CROSS JOIN (VALUES
        ('models.applications.governance'),
        ('lifecycle.attestations.write'),
        ('lifecycle.registry.mark_due')
    ) AS grant_row(permission)
    WHERE project_role.name = 'admin'
    ON CONFLICT (project_id, role_id, permission) DO NOTHING;
END
$$;
