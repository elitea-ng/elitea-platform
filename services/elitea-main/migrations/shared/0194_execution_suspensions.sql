-- 0194_execution_suspensions.sql — the kill switch (ADR-0041 decision 1,
-- issue #1493).
--
-- One table holds every suspension. A suspension names a scope:
--
--   platform      every run on the platform (the kill switch). project_id
--                 and scope_key are NULL.
--   project       every run of one project. project_id is the project;
--                 scope_key is the project id as text.
--   agent         every run of one agent. Application ids are per tenant
--                 schema, so the project is part of the key: project_id is
--                 the project, scope_key is the application id as text.
--                 project_id 0 with scope_key '0' is the self-test's
--                 synthetic agent: no run has those ids.
--   toolkit_type  every call of one toolkit type. scope_key is the CANONICAL
--                 key (lower-case ASCII letters and digits only), the same
--                 rule as the blocked_toolkits guardrail, so "Git Hub" and
--                 "github" are one suspension.
--
-- source says who wrote the row: an administrator (manual), the agent
-- circuit breaker (breaker, ADR-0041 decision 2), the certification flow
-- (certification) or the scheduled self-test (self_test). This release
-- writes only manual and self_test.
--
-- resume_requires says who may resume it:
--
--   any_admin        an administrator who may suspend that scope
--   governance_role  a platform administrator only, never a project admin
--   two_person       a platform administrator who is not the actor who
--                    suspended it
--
-- A row is active while resumed_at is NULL. The partial unique index keeps
-- one active suspension per (scope, project, key). Rows are never deleted:
-- they are the evidence of who stopped what, when and why.
--
-- agent_execution_jobs gains application_id: the agent of the run, written
-- at admission, so an agent suspension can find the runs it cancels. NULL
-- for an ad hoc chat (no agent) and for every run admitted before 0194.
--
-- The permission safety.suspend goes to the platform administrators
-- (administration mode: super_admin, admin) and to the project admin
-- (default mode: admin). The project admin's grant reaches only the
-- project-scoped routes (agent and project scope); the platform and
-- toolkit_type scopes resolve in administration mode only.
-- internal/infra/db/migrations/001_initial.sql seeds the same grants on a
-- fresh database, because 0060 returns early on an existing one.
--
-- Additive only. Idempotent. No BEGIN/COMMIT: the ledgered runner wraps the
-- file.

CREATE TABLE IF NOT EXISTS elitea_runtime.execution_suspensions (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    scope text NOT NULL
        CHECK (scope IN ('platform', 'project', 'agent', 'toolkit_type')),
    project_id integer
        CHECK (project_id IS NULL OR project_id >= 0),
    scope_key text
        CHECK (scope_key IS NULL OR length(scope_key) BETWEEN 1 AND 128),
    reason text NOT NULL
        CHECK (length(reason) BETWEEN 1 AND 1024),
    actor_id bigint NOT NULL
        CHECK (actor_id >= 0),
    source text NOT NULL DEFAULT 'manual'
        CHECK (source IN ('manual', 'breaker', 'certification', 'self_test')),
    resume_requires text NOT NULL DEFAULT 'any_admin'
        CHECK (resume_requires IN ('any_admin', 'governance_role', 'two_person')),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    cancelled_runs integer NOT NULL DEFAULT 0
        CHECK (cancelled_runs >= 0),
    resumed_at timestamptz,
    resumed_by bigint
        CHECK (resumed_by IS NULL OR resumed_by >= 0),
    resume_reason text
        CHECK (resume_reason IS NULL OR length(resume_reason) <= 1024),
    CONSTRAINT execution_suspensions_scope_key CHECK (
        (scope = 'platform' AND project_id IS NULL AND scope_key IS NULL)
        OR (scope = 'project' AND project_id IS NOT NULL
            AND scope_key = project_id::text)
        OR (scope = 'agent' AND project_id IS NOT NULL
            AND scope_key ~ '^[0-9]+$')
        OR (scope = 'toolkit_type' AND project_id IS NULL
            AND scope_key ~ '^[a-z0-9]+$')
    ),
    CONSTRAINT execution_suspensions_resume_complete CHECK (
        (resumed_at IS NULL AND resumed_by IS NULL)
        OR (resumed_at IS NOT NULL AND resumed_by IS NOT NULL
            AND resumed_at >= created_at)
    ),
    CONSTRAINT execution_suspensions_two_person CHECK (
        resume_requires <> 'two_person' OR resumed_by IS NULL
        OR resumed_by <> actor_id
    )
);

-- One active suspension per (scope, project, key); the admission read is a
-- scan of this small partial index.
CREATE UNIQUE INDEX IF NOT EXISTS execution_suspensions_one_active
    ON elitea_runtime.execution_suspensions
       (scope, COALESCE(project_id, -1), COALESCE(scope_key, ''))
    WHERE resumed_at IS NULL;

CREATE INDEX IF NOT EXISTS execution_suspensions_created
    ON elitea_runtime.execution_suspensions (created_at DESC);

CREATE INDEX IF NOT EXISTS execution_suspensions_project
    ON elitea_runtime.execution_suspensions (project_id, created_at DESC)
    WHERE project_id IS NOT NULL;

ALTER TABLE elitea_runtime.agent_execution_jobs
    ADD COLUMN IF NOT EXISTS application_id bigint
        CHECK (application_id IS NULL OR application_id > 0);

CREATE INDEX IF NOT EXISTS agent_execution_jobs_application
    ON elitea_runtime.agent_execution_jobs (application_id)
    WHERE application_id IS NOT NULL;

DO $$
BEGIN
    IF to_regclass('public.auth_core__role') IS NULL
       OR to_regclass('public.auth_core__role_permission') IS NULL THEN
        RAISE NOTICE '0194: auth_core tables absent, nothing to grant';
        RETURN;
    END IF;

    INSERT INTO public.auth_core__role_permission (role_id, permission)
    SELECT role.id, 'safety.suspend'
    FROM public.auth_core__role AS role
    WHERE (role.mode = 'administration' AND role.name IN ('super_admin', 'admin'))
       OR (role.mode = 'default' AND role.name = 'admin')
    ON CONFLICT (role_id, permission) DO NOTHING;

    -- legacyrbac reads the central grants only for a caller with no
    -- per-project rows (shared/0090). A project whose matrix was saved gets
    -- the string on its `admin` role here, as 0156 does for its grant. The
    -- string never existed, so no saved matrix withheld it on purpose.
    IF to_regclass('public.auth_core__project_role') IS NULL
       OR to_regclass('public.auth_core__project_role_permission') IS NULL THEN
        RETURN;
    END IF;

    INSERT INTO public.auth_core__project_role_permission (project_id, role_id, permission)
    SELECT DISTINCT saved.project_id, saved.role_id, 'safety.suspend'
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
