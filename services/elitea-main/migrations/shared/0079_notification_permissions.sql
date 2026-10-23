-- 0079_notification_permissions.sql — the DEFAULT-mode grants for the
-- notifications surface (#386).
--
--   models.notifications.notifications.list
--   models.notifications.notification.details
--   models.notifications.notification.update
--   models.notifications.notification.delete
--
-- WHY THIS FILE EXISTS. Each permission above gates a mounted route. No
-- migration in this corpus grants any of them. So the notification bell answers
-- 403 to every caller on a clean database.
--
-- THE ROUTES. internal/api/v2/notifications/api.go registers the routes through
-- its `currentNotificationEndpoint` helper. Every route resolves
-- `auth.PermissionModeDefault`, so this file grants in the `default` mode only.
--
-- WHERE THE SPLIT COMES FROM. testdata/postgres/legacy-rbac-matrix.json gives
-- the list, the details read and the delete to `admin`, `editor` and `viewer` in
-- `default` mode. It gives the update to `admin` and `editor` only. This file
-- copies that split. It makes no new policy.
--
-- THE UPDATE AND THE DELETE DIFFER, AND THAT IS THE MATRIX. A viewer may delete
-- a notification and may not update one. The asymmetry looks like an error. It
-- is what the legacy matrix records, and this file transcribes the matrix rather
-- than correct it. A correction is a policy change, and it needs its own issue.
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
-- 0060's VIRGIN-mode guard is NOT reproduced here, for the reason 0061 and 0072
-- state. These permissions never existed on a Go deployment, so no operator can
-- have revoked them.
DO $$
BEGIN

IF to_regclass('public.auth_core__role') IS NULL
   OR to_regclass('public.auth_core__role_permission') IS NULL THEN
    RAISE NOTICE '0079: auth_core tables absent, nothing to grant';
    RETURN;
END IF;

-- Granted to admin, editor and viewer. A viewer may read their notifications,
-- and may dismiss one.
INSERT INTO public.auth_core__role_permission (role_id, permission)
SELECT role.id, grant_row.permission
FROM public.auth_core__role AS role
CROSS JOIN (VALUES
    ('models.notifications.notifications.list'),
    ('models.notifications.notification.details'),
    ('models.notifications.notification.delete')
) AS grant_row(permission)
WHERE role.mode = 'default' AND role.name IN ('admin', 'editor', 'viewer')
ON CONFLICT (role_id, permission) DO NOTHING;

-- Granted to admin and editor only. The legacy matrix withholds the update from
-- the default-mode viewer.
INSERT INTO public.auth_core__role_permission (role_id, permission)
SELECT role.id, grant_row.permission
FROM public.auth_core__role AS role
CROSS JOIN (VALUES
    ('models.notifications.notification.update')
) AS grant_row(permission)
WHERE role.mode = 'default' AND role.name IN ('admin', 'editor')
ON CONFLICT (role_id, permission) DO NOTHING;

END
$$;
