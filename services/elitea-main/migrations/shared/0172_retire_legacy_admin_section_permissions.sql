-- 0172_retire_legacy_admin_section_permissions.sql — two more permission
-- strings that no code checks, removed the way 0136 removed the first fifteen.
--
--   REMOVED:  configuration.roles, projects.projects
--
-- Both are section names that the legacy admin module registered for its Roles
-- and Projects pages. The Go admin console gates those pages on
-- `configuration.roles.permissions.view` and `projects.projects.projects.view`,
-- which the administration-mode seeds grant. The admin nav also accepted the
-- two section names, so that a legacy-runtime deployment would see the items.
-- No such deployment remains, the nav stopped naming them in the same change
-- as this file, and no route gate, grant or worker ever checked either one.
-- No migration in this corpus seeds them, so the only rows are ones the legacy
-- runtime wrote into a database that was later migrated.
--
-- internal/api/router_permission_retired_gate_test.go reads the
-- dead_permission array here and in 0136, and checks that together they equal
-- the admin Roles catalogue's retiredPermissions (internal/api/v2/admin/roles.go)
-- and that no gate names any of them.
--
-- THE ALL-OR-NOTHING OVERRIDE RULE. As in 0136: a per-project override row is
-- deleted only when its (project, role) pair keeps at least one live row, so a
-- role whose override held only dead strings does not fall back to the full
-- central default-mode set. The central and user rows are plain sets.
--
-- WRITTEN AS 0172 AND RENUMBERED AT MERGE IF NEEDED.
--
-- Idempotent: a second run finds nothing to delete.
DO $$
DECLARE
    dead_permission text[] := ARRAY[
        'configuration.roles',
        'projects.projects'
    ];
BEGIN

IF to_regclass('public.auth_core__role_permission') IS NULL THEN
    RAISE NOTICE '0172: auth_core tables absent, nothing to remove';
    RETURN;
END IF;

DELETE FROM public.auth_core__role_permission
WHERE permission = ANY (dead_permission);

IF to_regclass('public.auth_core__user_permission') IS NOT NULL THEN
    DELETE FROM public.auth_core__user_permission
    WHERE permission = ANY (dead_permission);
END IF;

-- A per-project row goes only where its (project, role) pair keeps a live row.
IF to_regclass('public.auth_core__project_role_permission') IS NOT NULL THEN
    DELETE FROM public.auth_core__project_role_permission AS dead
    WHERE dead.permission = ANY (dead_permission)
      AND EXISTS (
          SELECT 1
          FROM public.auth_core__project_role_permission AS live
          WHERE live.project_id = dead.project_id
            AND live.role_id IS NOT DISTINCT FROM dead.role_id
            AND NOT (live.permission = ANY (dead_permission))
      );
END IF;

END
$$;
