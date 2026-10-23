-- Administration-mode RBAC backfill for databases that already exist.
--
-- Why this is needed at all
-- ------------------------
-- internal/api/router.go now gates the admin panel's READS on the same legacy
-- runtime permissions their Python counterparts declare — `GET
-- /admin/auth_users/{mode}` on `admin.auth.users`, and the rest of the block on
-- `runtime.plugins`, `projects.projects.projects.view`,
-- `configuration.roles.permissions.view` and `admin.moderation`. Unit A14 gated
-- only the writes precisely because that change was unsafe without this one:
-- `legacyrbac.PostgresResolver` resolves those permissions out of
-- auth_core__role/…__role_permission/…__user_role, and a database bootstrapped
-- by the pre-A14 internal/infra/db/migrations/001_initial.sql had NO
-- administration-mode role and NO role_permission row of any kind. Every
-- central permission resolved to the empty set, so a gated read would answer
-- 403 to every user including the operator.
--
-- Who this actually affects
-- -------------------------
--   * legacy-backed deployments (dev.elitea.ai, deploy/centry-hybrid's
--     pov-compose, any legacy dump) already have the roles and the grants:
--     auth_core/db/migrations/202202021633_core.py seeds the roles, and the
--     legacy runtime's `check_api` decorator inserts the role_permission rows
--     from `recommended_roles` at import time. Nothing here should apply to
--     them, and the guard below makes sure of it.
--   * fresh Go databases get everything from 001_initial.sql, which now seeds
--     the same rows. This migration finds them already present and no-ops.
--   * databases created by the OLD 001_initial.sql — existing developer and CI
--     databases, and any long-lived E2E volume — are the real target. They are
--     the only ones with an empty administration mode.
--
-- The guard
-- ---------
-- Everything runs only when the administration mode is VIRGIN: no
-- administration-mode role exists at all. That is deliberately stricter than
-- "the specific rows I want are missing". A deployment that has an
-- administration mode has an operator who may have REVOKED a grant through the
-- admin Permissions page, and a migration that re-inserted it would silently
-- undo that decision. Absent means never-configured; it does not mean
-- configured-to-empty.
DO $$
DECLARE
    admin_role_id INTEGER;
BEGIN
    -- elitea-migrate can run against a database whose auth_core tables have not
    -- been created yet (they come from 001_initial.sql, applied by elitea-main
    -- at startup, or from a legacy dump). Nothing to back-fill in that case.
    IF to_regclass('public.auth_core__role') IS NULL
       OR to_regclass('public.auth_core__role_permission') IS NULL
       OR to_regclass('public.auth_core__user_role') IS NULL
       OR to_regclass('public.auth_core__user') IS NULL THEN
        RAISE NOTICE '0060: auth_core tables absent, nothing to back-fill';
        RETURN;
    END IF;

    IF EXISTS (SELECT 1 FROM public.auth_core__role WHERE mode = 'administration') THEN
        RAISE NOTICE '0060: administration-mode roles already present, leaving RBAC untouched';
        RETURN;
    END IF;

    -- Roles. Mirrors 001_initial.sql and apps/elitea-web/scripts/e2e-stack.sh;
    -- `system` is omitted from all three because it is not in the Go product's
    -- role vocabulary (users.go `adminRolePriority`).
    INSERT INTO public.auth_core__role (name, mode) VALUES
        ('super_admin', 'administration'),
        ('admin', 'administration'),
        ('editor', 'administration'),
        ('viewer', 'administration')
    ON CONFLICT (name, mode) DO NOTHING;

    -- Grants, transcribed from the `recommended_roles` each legacy runtime
    -- handler declares in legacy/plugins/admin/api/v2/. See 001_initial.sql for
    -- the two transcription traps (a bare permission list still defaults
    -- system/super_admin/admin to True; a partial dict still leaves
    -- super_admin True). editor and viewer are granted nothing.
    INSERT INTO public.auth_core__role_permission (role_id, permission)
    SELECT role.id, grant_row.permission
    FROM public.auth_core__role AS role
    JOIN (VALUES
        ('super_admin', 'admin.auth.users'),
        ('super_admin', 'admin.auth.users.super_admin'),
        ('super_admin', 'runtime.plugins'),
        ('super_admin', 'projects.projects.projects.view'),
        ('super_admin', 'configuration.roles.permissions.view'),
        ('super_admin', 'admin.moderation'),
        ('admin', 'admin.auth.users'),
        ('admin', 'runtime.plugins'),
        ('admin', 'projects.projects.projects.view'),
        ('admin', 'configuration.roles.permissions.view'),
        ('admin', 'admin.moderation')
    ) AS grant_row(role_name, permission) ON grant_row.role_name = role.name
    WHERE role.mode = 'administration'
    ON CONFLICT (role_id, permission) DO NOTHING;

    SELECT id INTO admin_role_id
    FROM public.auth_core__role
    WHERE name = 'admin' AND mode = 'administration';

    -- One holder, chosen as narrowly as it can be: the dev bootstrap account
    -- 001_initial.sql seeds at id 1, and only if it still holds the
    -- default-mode `admin` role it was seeded with. Roles that nobody holds
    -- would leave the admin panel at 403-for-everyone, which is the outcome
    -- this migration exists to prevent; but granting global administration to
    -- "everyone who looks like an admin" is an escalation, and on a stale E2E
    -- volume it would hand it to `e2e-member@autotest.local` and destroy the
    -- authorisation difference the admin journeys assert. Any other account is
    -- promoted through the admin Users page, which unit A14 made real.
    --
    -- `admin` rather than `super_admin`, for the two reasons 001_initial.sql
    -- states: least privilege, and eliteacore/handler.go's project-member
    -- listing UNIONs in every holder of a role NAMED `super_admin` without
    -- filtering on `mode`, so seeding one would add a phantom member to every
    -- project's member list.
    IF admin_role_id IS NOT NULL THEN
        INSERT INTO public.auth_core__user_role (user_id, role_id)
        SELECT bootstrap.id, admin_role_id
        FROM public.auth_core__user AS bootstrap
        WHERE bootstrap.id = 1
          AND EXISTS (
              SELECT 1
              FROM public.auth_core__user_role AS existing
              JOIN public.auth_core__role AS existing_role ON existing_role.id = existing.role_id
              WHERE existing.user_id = bootstrap.id
                AND existing_role.name = 'admin'
                AND existing_role.mode = 'default'
          )
        ON CONFLICT (user_id, role_id) DO NOTHING;
    END IF;

    RAISE NOTICE '0060: seeded administration-mode RBAC';
END
$$;
