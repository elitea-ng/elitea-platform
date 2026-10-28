-- 0195_retention_policy.sql — retention policy per data class and legal hold
-- (ADR-0042 decision 4, issue #1495).
--
-- elitea_runtime.retention_policy
--   One PLATFORM row per data class (project_id NULL) and at most one PROJECT
--   override per (data class, project).
--
--   On a platform row:
--     min_age_days  the floor: the shortest maximum a project override may set.
--     max_age_days  the platform's maximum age and the ceiling of every
--                   override. NULL = keep forever.
--     allow_project_override  whether projects may override at all.
--   On a project row only max_age_days is meaningful (min_age_days stays 0):
--     the project's own maximum, inside the platform bounds.
--
--   "Inside the bounds" spans two rows, so the API enforces it in code
--   (internal/retention). The CHECKs below hold what one row can state.
--
--   Data classes: conversation, attachment, gateway_request_log, admin_audit,
--   security_audit. step_content and step_record are RESERVED for the step
--   records of ADR-0042 decisions 1-3: the CHECK admits them so that work
--   needs no constraint change, but nothing seeds or enforces them yet.
--
--   updated_by NULL marks a seeded row nobody has edited. The audit sweepers in
--   elitea-scheduler follow a row only once an administrator has saved it
--   (updated_by NOT NULL); until then their deployment configuration
--   (AUDIT_RETENTION_DAYS, SECURITY_AUDIT_OUTBOX_RETENTION_DAYS) applies, so
--   this migration changes nothing on a deployment that tuned them.
--
-- elitea_runtime.legal_holds
--   A hold on a project, an agent (application) of a project, or a user
--   (conversation owner). While released_at IS NULL it suspends deletion of
--   every conversation it covers: by the retention sweep and by a user's own
--   delete. Rows are never deleted; a release is a second stamp.
--
-- elitea_runtime.conversation_attachment_cleanup
--   The row-first journal of the retention sweep. The transaction that deletes
--   an expired conversation records the stored attachment objects it owned;
--   a drain step deletes the bytes and their elitea_storage.objects rows
--   afterwards, retrying with backoff kept in the row.
--
-- Seeded defaults preserve today's behaviour exactly:
--   conversation, attachment     keep forever (max NULL)
--   gateway_request_log          92 days (the gateway's former constant)
--   admin_audit                  365 days (AUDIT_RETENTION_DAYS default)
--   security_audit               keep forever (SECURITY_AUDIT_OUTBOX_RETENTION_DAYS
--                                default 0 = no deletion)
--
-- Permissions retention.manage and legal_hold.manage: super_admin and admin
-- in administration mode only (also seeded by 001_initial.sql).
--
-- Additive and idempotent. No BEGIN/COMMIT: the ledgered runner wraps the file.

CREATE SCHEMA IF NOT EXISTS elitea_runtime;

CREATE TABLE IF NOT EXISTS elitea_runtime.retention_policy (
    id                     bigserial PRIMARY KEY,
    data_class             text NOT NULL CHECK (data_class IN (
                               'conversation', 'attachment', 'step_content', 'step_record',
                               'gateway_request_log', 'admin_audit', 'security_audit')),
    scope                  text NOT NULL CHECK (scope IN ('platform', 'project')),
    project_id             bigint,
    min_age_days           integer NOT NULL DEFAULT 0 CHECK (min_age_days >= 0),
    max_age_days           integer CHECK (max_age_days IS NULL OR max_age_days >= 1),
    allow_project_override boolean NOT NULL DEFAULT false,
    updated_by             bigint CHECK (updated_by IS NULL OR updated_by > 0),
    updated_at             timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT retention_policy_scope_project CHECK (
        (scope = 'platform' AND project_id IS NULL)
        OR (scope = 'project' AND project_id IS NOT NULL AND project_id > 0
            AND min_age_days = 0 AND allow_project_override = false)),
    CONSTRAINT retention_policy_max_not_below_min CHECK (
        max_age_days IS NULL OR max_age_days >= min_age_days)
);

CREATE UNIQUE INDEX IF NOT EXISTS retention_policy_platform_uq
    ON elitea_runtime.retention_policy (data_class) WHERE scope = 'platform';
CREATE UNIQUE INDEX IF NOT EXISTS retention_policy_project_uq
    ON elitea_runtime.retention_policy (data_class, project_id) WHERE scope = 'project';

CREATE TABLE IF NOT EXISTS elitea_runtime.legal_holds (
    id             bigserial PRIMARY KEY,
    scope          text NOT NULL CHECK (scope IN ('project', 'agent', 'user')),
    project_id     bigint CHECK (project_id IS NULL OR project_id > 0),
    agent_id       bigint CHECK (agent_id IS NULL OR agent_id > 0),
    user_id        bigint CHECK (user_id IS NULL OR user_id > 0),
    reason         text NOT NULL CHECK (length(btrim(reason)) BETWEEN 1 AND 2000),
    created_by     bigint NOT NULL CHECK (created_by > 0),
    created_at     timestamptz NOT NULL DEFAULT now(),
    released_by    bigint CHECK (released_by IS NULL OR released_by > 0),
    released_at    timestamptz,
    release_reason text CHECK (release_reason IS NULL OR length(release_reason) <= 2000),
    CONSTRAINT legal_holds_scope_key CHECK (
        (scope = 'project' AND project_id IS NOT NULL AND agent_id IS NULL AND user_id IS NULL)
        OR (scope = 'agent' AND project_id IS NOT NULL AND agent_id IS NOT NULL AND user_id IS NULL)
        OR (scope = 'user' AND user_id IS NOT NULL AND project_id IS NULL AND agent_id IS NULL)),
    CONSTRAINT legal_holds_release_pair CHECK (
        (released_at IS NULL AND released_by IS NULL) OR (released_at IS NOT NULL AND released_by IS NOT NULL))
);

CREATE INDEX IF NOT EXISTS legal_holds_active_ix
    ON elitea_runtime.legal_holds (scope, project_id, agent_id, user_id) WHERE released_at IS NULL;

CREATE TABLE IF NOT EXISTS elitea_runtime.conversation_attachment_cleanup (
    id              bigserial PRIMARY KEY,
    project_id      bigint NOT NULL CHECK (project_id > 0),
    conversation_id bigint NOT NULL,
    bucket          text NOT NULL CHECK (length(bucket) > 0),
    name            text NOT NULL CHECK (length(name) > 0),
    enqueued_at     timestamptz NOT NULL DEFAULT now(),
    attempts        integer NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    last_error      text
);

CREATE INDEX IF NOT EXISTS conversation_attachment_cleanup_due_ix
    ON elitea_runtime.conversation_attachment_cleanup (next_attempt_at, id);

INSERT INTO elitea_runtime.retention_policy
    (data_class, scope, min_age_days, max_age_days, allow_project_override)
VALUES
    ('conversation',        'platform', 0,  NULL, true),
    ('attachment',          'platform', 0,  NULL, false),
    ('gateway_request_log', 'platform', 1,  92,   false),
    ('admin_audit',         'platform', 7,  365,  false),
    ('security_audit',      'platform', 1,  NULL, false)
ON CONFLICT (data_class) WHERE scope = 'platform' DO NOTHING;

DO $$
BEGIN
    IF to_regclass('public.auth_core__role') IS NULL
       OR to_regclass('public.auth_core__role_permission') IS NULL THEN
        RAISE NOTICE '0195: auth_core tables absent, nothing to grant';
        RETURN;
    END IF;

    INSERT INTO public.auth_core__role_permission (role_id, permission)
    SELECT role.id, grant_row.permission
    FROM public.auth_core__role AS role
    CROSS JOIN (VALUES ('retention.manage'), ('legal_hold.manage')) AS grant_row(permission)
    WHERE role.mode = 'administration' AND role.name IN ('super_admin', 'admin')
    ON CONFLICT (role_id, permission) DO NOTHING;
END
$$;
