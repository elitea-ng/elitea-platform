-- 0075_secret_permissions.sql — the DEFAULT-mode grants for the secrets surface
-- (#386).
--
--   configuration.secrets.secret.list
--   configuration.secrets.secret.create
--   configuration.secrets.secret.edit
--   configuration.secrets.secret.delete
--   configuration.secrets.secret.hide
--   configuration.secrets.secret.unsecret
--
-- WHY THIS FILE EXISTS. Each permission above gates a mounted route. No
-- migration in this corpus grants any of them. So the whole secrets surface
-- answers 403 to every caller on a clean database.
--
-- The consequence reaches past the secrets page. A project stores its provider
-- credentials as secrets. An operator who cannot list or create a secret cannot
-- give the project a credential.
--
-- THE ROUTES. internal/api/v2/secrets/handler.go registers all six routes
-- through its `projectGate` helper. Every route resolves
-- `auth.PermissionModeDefault`, so this file grants in the `default` mode only.
--
-- WHERE THE SPLIT COMES FROM. testdata/postgres/legacy-rbac-matrix.json gives
-- all six strings to `admin` and `editor` in `default` mode. It gives none of
-- them to `viewer`. This file copies that split. It makes no new policy.
--
-- READ THE `list` GRANT CAREFULLY. The matrix withholds the LIST from the
-- viewer, not only the writes. A viewer therefore still gets 403 on the secrets
-- page after this file applies. That is legacy parity, and it is deliberate: a
-- secret name is itself sensitive. A viewer that could enumerate a project's
-- secrets is a widening this file has no mandate to make.
--
-- The matrix also gives all six to `system` and to `super_admin`. This file
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
    RAISE NOTICE '0075: auth_core tables absent, nothing to grant';
    RETURN;
END IF;

-- Granted to admin and editor only. The legacy matrix withholds every string
-- below from the default-mode viewer, the read included.
INSERT INTO public.auth_core__role_permission (role_id, permission)
SELECT role.id, grant_row.permission
FROM public.auth_core__role AS role
CROSS JOIN (VALUES
    ('configuration.secrets.secret.list'),
    ('configuration.secrets.secret.create'),
    ('configuration.secrets.secret.edit'),
    ('configuration.secrets.secret.delete'),
    ('configuration.secrets.secret.hide'),
    ('configuration.secrets.secret.unsecret')
) AS grant_row(permission)
WHERE role.mode = 'default' AND role.name IN ('admin', 'editor')
ON CONFLICT (role_id, permission) DO NOTHING;

END
$$;
