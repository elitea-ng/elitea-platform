-- 0072_agent_cancel_and_configuration_permissions.sql — the DEFAULT-mode grants
-- for agent cancel and for the whole configuration surface (#359).
--
--   models.chat.task.delete
--   configurations.configurations.list
--   configurations.configuration.details
--   configurations.configuration.create
--   configurations.configuration.update
--   configurations.configuration.delete
--
-- WHY THIS FILE EXISTS. Each permission above gates a mounted production route.
-- No migration in this corpus grants any of them. 001_initial.sql does not seed
-- them either. So each route answers 403 to every caller on a clean database.
--
-- This is the defect #354 records, and 0063 states its consequence: a route that
-- gates on a permission nothing grants refuses everybody. The page looks broken.
-- The log names no missing grant. 0070 fixed one such string,
-- `models.chat.messages.create`. This file fixes the six that remain.
--
-- THE ROUTES. Each constant below is the gate the route applies.
--
--   * internal/api/v2/agentexecution/cancel.go:17,
--     `CurrentAgentCancelPermission`. It gates
--     DELETE /api/v2/elitea_core/task/prompt_lib/{projectID}/{responseMessageID}.
--     production_router.go:162-164 mounts it.
--   * internal/api/v2/configurations/read.go:21-22,
--     `CurrentConfigurationListPermission` and
--     `CurrentConfigurationGetPermission`. read.go:63-78 applies them.
--   * internal/api/v2/configurations/mutation.go:26-28,
--     `CurrentConfigurationCreatePermission`, `...UpdatePermission` and
--     `...DeletePermission`.
--   * production_router.go:138-148 mounts the five configuration routes.
--     cmd/elitea-main/main.go:554 and :780 wire them.
--
-- Every route above resolves `auth.PermissionModeDefault`, so this file grants
-- in the `default` mode only.
--
-- WHY THIS BLOCKS CHAT. The chat model picker reads the configuration list. A
-- 403 on that list gives the user no model to select. So chat does not work end
-- to end on a clean deployment, even after 0070 grants the start permission.
-- The user also cannot stop a running agent turn: the cancel button gets a 403.
--
-- WHERE THE SPLIT COMES FROM. testdata/postgres/legacy-rbac-matrix.json holds
-- the legacy grants. In `default` mode it gives `models.chat.task.delete`,
-- `configurations.configurations.list` and `configurations.configuration.details`
-- to `admin`, `editor` and `viewer`. It gives the three configuration write
-- strings to `admin` and `editor` only. So this file restores parity. It makes
-- no new policy. A viewer that could create, replace or delete a configuration
-- is a widening this file has no mandate to make.
--
-- The matrix also gives all six to `system` and to `super_admin`. This file
-- omits both, as every other file in this corpus does: Go seeds neither role in
-- the default mode.
--
-- BLAST RADIUS. legacyrbac's projectPermissions() reads the CENTRAL default-mode
-- grants by role NAME. It reads them only for a project that carries NO
-- per-project auth_core__project_role_permission rows. That shape is the fresh
-- Go database. It is never the shape of a legacy-backed database, of a legacy
-- dump, or of the end-to-end stack: each one seeds per-project rows, and those
-- rows suppress the central fallback completely. So no existing deployment's
-- members gain anything here.
--
-- The fallback also joins THROUGH the caller's assigned project roles. So a
-- non-member has no row to fall back from, and gains nothing at all.
--
-- Members' permission sets are non-empty since 0062. So this file moves no
-- set-emptiness transition of the kind #276 tracks.
--
-- 0060's VIRGIN-mode guard is NOT reproduced here. 0061, 0062, 0063, 0066, 0068,
-- 0069 and 0070 all give the same reason: these permissions never existed on a
-- Go deployment, so no operator can have revoked them. A guard that skips
-- configured deployments leaves exactly those unable to read a configuration.
DO $$
BEGIN

IF to_regclass('public.auth_core__role') IS NULL
   OR to_regclass('public.auth_core__role_permission') IS NULL THEN
    RAISE NOTICE '0072: auth_core tables absent, nothing to grant';
    RETURN;
END IF;

-- Granted to admin, editor and viewer. A viewer may stop an agent turn they
-- started, and may read the configurations the model picker lists.
INSERT INTO public.auth_core__role_permission (role_id, permission)
SELECT role.id, grant_row.permission
FROM public.auth_core__role AS role
CROSS JOIN (VALUES
    ('models.chat.task.delete'),
    ('configurations.configurations.list'),
    ('configurations.configuration.details')
) AS grant_row(permission)
WHERE role.mode = 'default' AND role.name IN ('admin', 'editor', 'viewer')
ON CONFLICT (role_id, permission) DO NOTHING;

-- Granted to admin and editor only. The legacy matrix withholds every string
-- below from the default-mode viewer.
INSERT INTO public.auth_core__role_permission (role_id, permission)
SELECT role.id, grant_row.permission
FROM public.auth_core__role AS role
CROSS JOIN (VALUES
    ('configurations.configuration.create'),
    ('configurations.configuration.update'),
    ('configurations.configuration.delete')
) AS grant_row(permission)
WHERE role.mode = 'default' AND role.name IN ('admin', 'editor')
ON CONFLICT (role_id, permission) DO NOTHING;

END
$$;
