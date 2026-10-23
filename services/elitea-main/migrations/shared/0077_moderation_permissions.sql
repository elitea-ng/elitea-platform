-- 0077_moderation_permissions.sql — the DEFAULT-mode grants for the moderation
-- surface (#386).
--
--   admin.moderation.view
--   admin.moderation.create
--
-- WHY THIS FILE EXISTS. Each permission above gates a mounted route. No
-- migration in this corpus grants either of them.
--
-- WHY THE EXISTING GRANT DOES NOT HELP. 001_initial.sql grants the BARE string
-- `admin.moderation` to the administration `super_admin` role.
-- middleware.hasIntersection compares exact strings. So the bare grant does not
-- satisfy a `.view` gate or a `.create` gate. Two more reasons make the bare
-- grant useless here: it lands in the `administration` mode, and the two routes
-- below resolve the `default` mode; and 001_initial.sql is the dev bootstrap,
-- which a clean production database never applies.
--
-- THE ROUTES. internal/api/router.go registers the two project-scoped
-- moderation routes:
--
--   GET  /moderation_status/{mode}/{projectID}/{entityID}
--   POST /moderation_status/{mode}/{projectID}/{entityID}
--
-- Both resolve `auth.PermissionModeDefault`, so this file grants in the
-- `default` mode only. The `administration` moderation gate is a different
-- surface, and 0082 grants it.
--
-- WHERE THE SPLIT COMES FROM. testdata/postgres/legacy-rbac-matrix.json gives
-- both strings to `admin`, `editor` and `viewer` in `default` mode. This file
-- copies that split. It makes no new policy.
--
-- A viewer may request moderation. That is what the matrix says, and it fits the
-- route: `admin.moderation.create` REQUESTS a moderation decision, it does not
-- make one. 0082 grants the decision, `admin.moderation.edit`, in the
-- `administration` mode only.
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
    RAISE NOTICE '0077: auth_core tables absent, nothing to grant';
    RETURN;
END IF;

-- Granted to admin, editor and viewer. A viewer may read a moderation status,
-- and may ask for a moderation decision.
INSERT INTO public.auth_core__role_permission (role_id, permission)
SELECT role.id, grant_row.permission
FROM public.auth_core__role AS role
CROSS JOIN (VALUES
    ('admin.moderation.view'),
    ('admin.moderation.create')
) AS grant_row(permission)
WHERE role.mode = 'default' AND role.name IN ('admin', 'editor', 'viewer')
ON CONFLICT (role_id, permission) DO NOTHING;

END
$$;
