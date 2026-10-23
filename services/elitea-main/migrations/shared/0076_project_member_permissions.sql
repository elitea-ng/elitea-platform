-- 0076_project_member_permissions.sql — the DEFAULT-mode grants for the project
-- members surface (#386).
--
--   configuration.users.users.create
--   configuration.users.users.edit
--   configuration.users.users.delete
--
-- WHY THIS FILE EXISTS. Each permission above gates a mounted write route. No
-- migration in this corpus grants any of them.
--
-- THE SPLIT THAT MAKES THIS EASY TO MISS. 0068 grants
-- `configuration.users.users.view` and `configuration.roles.roles.view`. So the
-- READS work and the WRITES do not. The members page loads, it lists the
-- members, and every button on it answers 403. That reads as a broken page, not
-- as a missing grant.
--
-- THE ROUTES. internal/api/router.go registers the three member write routes.
-- The same file registers the reads that 0068 already covers. Every route
-- resolves `auth.PermissionModeDefault`, so this file grants in the `default`
-- mode only.
--
-- WHERE THE SPLIT COMES FROM. testdata/postgres/legacy-rbac-matrix.json gives
-- all three strings to `admin` in `default` mode. It gives none of them to
-- `editor` or to `viewer`. This file copies that split. It makes no new policy.
-- An editor that could add or remove a project member is a widening this file
-- has no mandate to make.
--
-- The matrix also gives all three to `system` and to `super_admin`. This file
-- omits both, as every other file in this corpus does. Go seeds neither role in
-- the default mode.
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
-- non-member has no row to fall back from, and gains nothing at all. The grant
-- therefore gives a project admin power over their OWN project only.
--
-- 0060's VIRGIN-mode guard is NOT reproduced here, for the reason 0061 and 0072
-- state. These permissions never existed on a Go deployment, so no operator can
-- have revoked them.
DO $$
BEGIN

IF to_regclass('public.auth_core__role') IS NULL
   OR to_regclass('public.auth_core__role_permission') IS NULL THEN
    RAISE NOTICE '0076: auth_core tables absent, nothing to grant';
    RETURN;
END IF;

-- Granted to admin only. The legacy matrix withholds every string below from
-- the default-mode editor and from the default-mode viewer.
INSERT INTO public.auth_core__role_permission (role_id, permission)
SELECT role.id, grant_row.permission
FROM public.auth_core__role AS role
CROSS JOIN (VALUES
    ('configuration.users.users.create'),
    ('configuration.users.users.edit'),
    ('configuration.users.users.delete')
) AS grant_row(permission)
WHERE role.mode = 'default' AND role.name = 'admin'
ON CONFLICT (role_id, permission) DO NOTHING;

END
$$;
