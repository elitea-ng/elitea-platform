-- 0078_scheduling_permissions.sql — the DEFAULT-mode grants for the scheduling
-- surface (#386).
--
--   configuration.scheduling.schedules.view
--   configuration.scheduling.schedules.edit
--
-- WHY THIS FILE EXISTS. Each permission above gates a mounted route. No
-- migration in this corpus grants either of them. So the project Schedules tab
-- answers 403 to every caller on a clean database.
--
-- THE ROUTES. internal/api/router.go registers the two project-scoped
-- scheduling routes:
--
--   GET /scheduling/schedules/{mode}/{projectID}
--   PUT /scheduling/schedules/{mode}/{projectID}
--
-- Both resolve `auth.PermissionModeDefault`, so this file grants in the
-- `default` mode only.
--
-- THE SAME TWO STRINGS ALSO GATE THE ADMIN PANEL. router.go registers
-- `/scheduling/schedules/administration/{projectID}` on the same two
-- permissions, in the `administration` mode. A default-mode grant does NOT
-- satisfy an administration-mode gate: legacyrbac resolves each mode against its
-- own roles. 0082 grants the administration pair. Both files are needed, and
-- neither one covers the other.
--
-- WHERE THE SPLIT COMES FROM. testdata/postgres/legacy-rbac-matrix.json gives
-- both strings to `admin` in `default` mode. It gives neither to `editor` or to
-- `viewer`. This file copies that split. It makes no new policy. An editor that
-- could read or rewrite a project's cron table is a widening this file has no
-- mandate to make.
--
-- The matrix also gives both to `system` and to `super_admin`. This file omits
-- both, as every other file in this corpus does. Go seeds neither role in the
-- default mode.
--
-- BLAST RADIUS. legacyrbac's projectPermissions() reads the CENTRAL default-mode
-- grants by role NAME. It reads them only for a project that carries NO
-- per-project auth_core__project_role_permission rows. That shape is the fresh
-- Go database. It is never the shape of a legacy-backed database, of a legacy
-- dump, or of the end-to-end stack. Each one seeds per-project rows, and those
-- rows suppress the central fallback completely. So no existing deployment's
-- members gain anything here.
--
-- The fallback also joins THROUGH the caller's assigned project roles. So a
-- non-member has no row to fall back from, and gains nothing at all.
--
-- 0060's VIRGIN-mode guard is NOT reproduced here, for the reason 0061 and 0072
-- state. These permissions never existed on a Go deployment, so no operator can
-- have revoked them.
DO $$
BEGIN

IF to_regclass('public.auth_core__role') IS NULL
   OR to_regclass('public.auth_core__role_permission') IS NULL THEN
    RAISE NOTICE '0078: auth_core tables absent, nothing to grant';
    RETURN;
END IF;

-- Granted to admin only. The legacy matrix withholds both strings from the
-- default-mode editor and from the default-mode viewer.
INSERT INTO public.auth_core__role_permission (role_id, permission)
SELECT role.id, grant_row.permission
FROM public.auth_core__role AS role
CROSS JOIN (VALUES
    ('configuration.scheduling.schedules.view'),
    ('configuration.scheduling.schedules.edit')
) AS grant_row(permission)
WHERE role.mode = 'default' AND role.name = 'admin'
ON CONFLICT (role_id, permission) DO NOTHING;

END
$$;
