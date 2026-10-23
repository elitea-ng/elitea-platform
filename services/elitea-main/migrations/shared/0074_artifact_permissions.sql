-- 0074_artifact_permissions.sql — the DEFAULT-mode grants for the artifact
-- surface (#386).
--
--   configuration.artifacts.artifacts.view
--   configuration.artifacts.artifacts.create
--   configuration.artifacts.artifacts.edit
--   configuration.artifacts.artifacts.delete
--
-- WHY THIS FILE EXISTS. Each permission above gates a mounted route. No
-- migration in this corpus grants any of them. So each route answers 403 to
-- every caller on a clean database. The artifact page loads and every button on
-- it fails.
--
-- This is the fourth instance of the class that #354 and #359 record. The gate
-- in internal/api/router_permission_grant_gate_test.go found it. That gate
-- compares the permissions the routes gate on against the permissions this
-- corpus grants.
--
-- THE ROUTES. internal/api/router.go registers the four artifact routes. The
-- same file registers the S3-shaped routes on the same permissions. Every route
-- resolves `auth.PermissionModeDefault`, so this file grants in the `default`
-- mode only.
--
-- WHERE THE SPLIT COMES FROM. testdata/postgres/legacy-rbac-matrix.json holds
-- the legacy grants. In `default` mode it gives the read to `admin`, `editor`
-- and `viewer`. It gives the three write strings to `admin` and `editor` only.
-- This file copies that split. It makes no new policy.
--
-- The matrix also gives all four to `system` and to `super_admin`. This file
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
-- non-member has no row to fall back from, and gains nothing at all.
--
-- 0060's VIRGIN-mode guard is NOT reproduced here. 0061, 0062, 0063, 0066, 0068,
-- 0069, 0070 and 0072 all give the same reason. These permissions never existed
-- on a Go deployment, so no operator can have revoked them. A guard that skips
-- configured deployments leaves exactly those unable to read an artifact.
DO $$
BEGIN

IF to_regclass('public.auth_core__role') IS NULL
   OR to_regclass('public.auth_core__role_permission') IS NULL THEN
    RAISE NOTICE '0074: auth_core tables absent, nothing to grant';
    RETURN;
END IF;

-- Granted to admin, editor and viewer. A viewer may list and download the
-- artifacts of a project they belong to.
INSERT INTO public.auth_core__role_permission (role_id, permission)
SELECT role.id, grant_row.permission
FROM public.auth_core__role AS role
CROSS JOIN (VALUES
    ('configuration.artifacts.artifacts.view')
) AS grant_row(permission)
WHERE role.mode = 'default' AND role.name IN ('admin', 'editor', 'viewer')
ON CONFLICT (role_id, permission) DO NOTHING;

-- Granted to admin and editor only. The legacy matrix withholds every string
-- below from the default-mode viewer.
INSERT INTO public.auth_core__role_permission (role_id, permission)
SELECT role.id, grant_row.permission
FROM public.auth_core__role AS role
CROSS JOIN (VALUES
    ('configuration.artifacts.artifacts.create'),
    ('configuration.artifacts.artifacts.edit'),
    ('configuration.artifacts.artifacts.delete')
) AS grant_row(permission)
WHERE role.mode = 'default' AND role.name IN ('admin', 'editor')
ON CONFLICT (role_id, permission) DO NOTHING;

END
$$;
