-- 0081_project_permissions.sql — the DEFAULT-mode grants for the projects
-- surface (#386).
--
--   projects.projects.project.view
--   projects.projects.project.edit
--   projects.projects.groups.edit
--   projects.projects.group.create
--   projects.projects.group.delete
--
-- WHY THIS FILE EXISTS. Each permission above gates a mounted route. No
-- migration in this corpus grants any of them.
--
-- READ `projects.projects.project.view` FIRST. It gates the project LIST, which
-- every session calls before it can show anything. On a clean database that call
-- answers 403, so the user reaches an empty project picker and stops. This is
-- the widest-blast-radius string in #386.
--
-- THE ROUTES. internal/api/v2/projects/handler.go and production.go register
-- them. Every route resolves `auth.PermissionModeDefault`, so this file grants in
-- the `default` mode only.
--
-- WHERE THE SPLIT COMES FROM. testdata/postgres/legacy-rbac-matrix.json gives
-- `projects.projects.project.view` to `admin`, `editor` and `viewer` in
-- `default` mode. It gives `projects.projects.project.edit` to `admin` only. It
-- gives the three group strings to `admin` and `editor`. This file copies that
-- split. It makes no new policy.
--
-- Note that the project EDIT is narrower than the group edit. Renaming a project
-- is an owner action in the matrix, and changing a group inside it is not. This
-- file keeps that difference rather than smooth it.
--
-- The matrix also gives all five to `system` and to `super_admin`. This file
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
-- non-member has no row to fall back from, and gains nothing at all. A user
-- therefore lists the projects they belong to, and gains no view of any other.
--
-- 0060's VIRGIN-mode guard is NOT reproduced here, for the reason 0061 and 0072
-- state. These permissions never existed on a Go deployment, so no operator can
-- have revoked them.
DO $$
BEGIN

IF to_regclass('public.auth_core__role') IS NULL
   OR to_regclass('public.auth_core__role_permission') IS NULL THEN
    RAISE NOTICE '0081: auth_core tables absent, nothing to grant';
    RETURN;
END IF;

-- Granted to admin, editor and viewer. A viewer must list the projects they
-- belong to, or they cannot reach any page at all.
INSERT INTO public.auth_core__role_permission (role_id, permission)
SELECT role.id, grant_row.permission
FROM public.auth_core__role AS role
CROSS JOIN (VALUES
    ('projects.projects.project.view')
) AS grant_row(permission)
WHERE role.mode = 'default' AND role.name IN ('admin', 'editor', 'viewer')
ON CONFLICT (role_id, permission) DO NOTHING;

-- Granted to admin and editor. The legacy matrix withholds the three group
-- strings from the default-mode viewer.
INSERT INTO public.auth_core__role_permission (role_id, permission)
SELECT role.id, grant_row.permission
FROM public.auth_core__role AS role
CROSS JOIN (VALUES
    ('projects.projects.groups.edit'),
    ('projects.projects.group.create'),
    ('projects.projects.group.delete')
) AS grant_row(permission)
WHERE role.mode = 'default' AND role.name IN ('admin', 'editor')
ON CONFLICT (role_id, permission) DO NOTHING;

-- Granted to admin only. The legacy matrix makes the project edit narrower than
-- the group edit above.
INSERT INTO public.auth_core__role_permission (role_id, permission)
SELECT role.id, grant_row.permission
FROM public.auth_core__role AS role
CROSS JOIN (VALUES
    ('projects.projects.project.edit')
) AS grant_row(permission)
WHERE role.mode = 'default' AND role.name = 'admin'
ON CONFLICT (role_id, permission) DO NOTHING;

END
$$;
