-- 0062_budgets_quota_statistics.sql — the tables and grants issue #246's
-- budgets/quotas/usage routes need.
--
-- Three independent pieces, kept in one migration because they land with one
-- API surface:
--
--   1. centry.project_quota and centry.statistic. Both exist in every
--      legacy-backed database
--      (legacy/plugins/projects/models/{quota,statistics}.py create them) and
--      in NO Go-bootstrapped one — 001_initial.sql never carried them. `GET/PUT
--      /api/v2/projects/quota/{project_id}` and `GET
--      /api/v2/projects/statistics/{project_id}` read them, so a fresh database
--      needs them or those three routes answer 500 forever.
--   2. The two administration-mode grants the admin budget routes are gated on.
--   3. The default-mode grant the project-scoped budget/usage READS are gated
--      on.
--
-- Why (3) matters more than it looks: `legacyrbac.PostgresResolver`'s
-- projectPermissions() falls back to CENTRAL default-mode grants by role name
-- when a project has no per-project rows, and a Go-bootstrapped database has
-- default-mode ROLES (001_initial.sql) but not one default-mode
-- role_permission row. So every project-scoped permission resolves to the
-- empty set, and gating a project read without this grant is
-- 403-for-everyone — the exact failure 0060's header exists to describe.
--
-- Idempotent and additive throughout: CREATE TABLE IF NOT EXISTS leaves a
-- dump-loaded quota table and its data untouched, and the grants are
-- ON CONFLICT DO NOTHING against roles that must already exist.

CREATE SCHEMA IF NOT EXISTS centry;

-- ---------------------------------------------------------------------------
-- centry.project_quota — per-project resource ceilings
-- ---------------------------------------------------------------------------
-- Column-for-column legacy/plugins/projects/models/quota.py, verified against
-- the deployed table (\d centry.project_quota). The commented-out columns in
-- that model (vuh_limit, storage_space) are absent from the deployed table too
-- and are absent here; -1 is its "unlimited" sentinel, which check_quota reads
-- before comparing against centry.statistic.
CREATE TABLE IF NOT EXISTS centry.project_quota (
    id                        SERIAL PRIMARY KEY,
    project_id                INTEGER NOT NULL,
    data_retention_limit      INTEGER,
    test_duration_limit       INTEGER DEFAULT -1,
    cpu_limit                 INTEGER DEFAULT -1,
    memory_limit              INTEGER DEFAULT -1,
    last_update_time          TIMESTAMP DEFAULT (now() AT TIME ZONE 'utc'),
    dast_scans                INTEGER DEFAULT -1,
    sast_scans                INTEGER DEFAULT -1,
    vcu_hard_limit            INTEGER,
    vcu_soft_limit            INTEGER,
    vcu_limit_total_block     BOOLEAN NOT NULL DEFAULT false,
    storage_hard_limit        INTEGER,
    storage_soft_limit        INTEGER,
    storage_limit_total_block BOOLEAN NOT NULL DEFAULT false
);

-- A plain index, NOT a unique one, and that is the whole point of this comment.
--
-- The reference model declares no unique constraint and every reader takes
-- `.first()`, so two rows for one project are schema-legal there and a
-- dump-loaded table may contain them. `CREATE UNIQUE INDEX` against inherited
-- rows would then fail with a unique violation and abort this whole migration —
-- the grants below included — on exactly the deployments it is written for, and
-- only on those with the duplicate, so no amount of local testing would find
-- it. Uniqueness is not ours to assert retroactively over data we did not
-- write; the readers keep the reference's take-the-first-row semantics.
CREATE INDEX IF NOT EXISTS project_quota_project_idx
    ON centry.project_quota (project_id);

-- ---------------------------------------------------------------------------
-- centry.statistic — per-project usage counters
-- ---------------------------------------------------------------------------
-- legacy/plugins/projects/models/statistics.py. The counters are written by
-- the carrier-era test runners, none of which exist in this platform, so on a
-- Go deployment they stay at their defaults; the row still has to exist for
-- the statistics endpoint to have a current-value side at all.
CREATE TABLE IF NOT EXISTS centry.statistic (
    id                       SERIAL PRIMARY KEY,
    project_id               INTEGER NOT NULL,
    start_time               TIMESTAMP DEFAULT (now() AT TIME ZONE 'utc'),
    vuh_used                 INTEGER DEFAULT 0,
    performance_test_runs    INTEGER DEFAULT 0,
    sast_scans               INTEGER DEFAULT 0,
    dast_scans               INTEGER DEFAULT 0,
    public_pool_workers      INTEGER DEFAULT 0,
    ui_performance_test_runs INTEGER DEFAULT 0,
    tasks_executions         INTEGER DEFAULT 0
);

-- Plain, for the reason given above project_quota_project_idx.
CREATE INDEX IF NOT EXISTS statistic_project_idx
    ON centry.statistic (project_id);

-- ---------------------------------------------------------------------------
-- Grants
-- ---------------------------------------------------------------------------
DO $$
BEGIN

-- elitea-migrate can run before 001_initial.sql has created auth_core (see
-- 0060/0061, which open with the same check). Nothing to grant in that case.
IF to_regclass('public.auth_core__role') IS NULL
   OR to_regclass('public.auth_core__role_permission') IS NULL THEN
    RAISE NOTICE '0062: auth_core tables absent, nothing to grant';
    RETURN;
END IF;

-- Administration mode. project_budget.py, project_budgets.py, user_budget.py
-- and user_budgets.py all declare `{"admin": True, "editor": False,
-- "viewer": False}` in ADMINISTRATION_MODE, which — per the transcription trap
-- 0060, 0061 and 001_initial.sql all document — leaves super_admin at its
-- default True. So both roles, and neither editor nor viewer.
--
-- 0060's virgin-mode guard is deliberately NOT reproduced, for 0061's reason:
-- these two permissions did not exist before this migration, so no operator
-- can have revoked them, and skipping a configured deployment would leave
-- exactly that deployment unable to reach the routes.
INSERT INTO public.auth_core__role_permission (role_id, permission)
SELECT role.id, grant_row.permission
FROM public.auth_core__role AS role
JOIN (VALUES
    ('super_admin', 'models.admin.project_budgets.view'),
    ('super_admin', 'models.admin.project_budgets.edit'),
    ('admin', 'models.admin.project_budgets.view'),
    ('admin', 'models.admin.project_budgets.edit')
) AS grant_row(role_name, permission) ON grant_row.role_name = role.name
WHERE role.mode = 'administration'
ON CONFLICT (role_id, permission) DO NOTHING;

-- Default mode. project_budget.py, user_budget.py, user_budgets.py and
-- usage.py declare `{"admin": True, "editor": True, "viewer": True}` in
-- DEFAULT_MODE for their project-scoped reads, so all three project roles hold
-- it. This is a project-scoped permission being granted to project-scoped
-- roles; it is not the "central admin permission leaking into project
-- resolution" that 001_initial.sql warns about. It is also parity, not
-- invention: the legacy runtime database grants exactly this permission to
-- default-mode admin, editor, viewer, super_admin and system.
--
-- WHAT ELSE THIS CHANGES, because it is more than the budgets routes.
--
-- This is the FIRST default-mode role_permission row in this codebase, so it is
-- the first time a project member on a Go-bootstrapped database resolves to a
-- NON-EMPTY permission set instead of an empty one. Verified against a real
-- PostgreSQL, and pinned by
-- internal/infra/legacyrbac/default_mode_grant_postgres_integration_test.go:
--
--   * a project MEMBER goes from [] to [models.project_context.view];
--   * a NON-MEMBER still resolves to [] — the central fallback is joined
--     through the caller's assigned project roles, so it cannot reach anyone
--     outside the project;
--   * a project that carries its OWN per-project grants — every legacy-backed
--     database and every legacy dump — suppresses the fallback entirely, so
--     this grant is INERT there and cannot change what an existing deployment's
--     members can do.
--
-- Two consequences outside the budgets surface, both confined to
-- Go-bootstrapped databases and to members of the project in question:
--
--  1. `GET /api/v2/elitea_core/project_info/{mode}/{project_id}/project-info`
--     is gated on this same permission and is therefore 403-for-everyone today.
--     It starts working for members. That is the state it was written to have.
--  2. internal/runtimecomposition/public_authorizer.go's
--     ConfigurationValidationCapability branch admits on
--     `len(resolution.Permissions) != 0` as a stand-in for "is a project
--     member" — its own comment says so, pending a decision about persisting
--     the originating permission. It refuses every caller today because every
--     caller resolves to []; it will admit project members. That is what the
--     branch says it means to do, but it is a behaviour change reached through
--     a set SIZE rather than a named permission, and the coupling is worth
--     replacing with an explicit membership check. Left alone here on purpose:
--     that is a decision about execution-event authorization, not about
--     budgets. Tracked as issue #276, which also names the replacement — the
--     admission path for that same capability already asks the membership
--     question directly (AuthorizeRuntimeValidationProject).
INSERT INTO public.auth_core__role_permission (role_id, permission)
SELECT role.id, 'models.project_context.view'
FROM public.auth_core__role AS role
WHERE role.mode = 'default' AND role.name IN ('admin', 'editor', 'viewer')
ON CONFLICT (role_id, permission) DO NOTHING;

END
$$;
