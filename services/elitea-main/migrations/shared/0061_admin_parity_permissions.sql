-- 0061_admin_parity_permissions.sql — the four central permissions issue 255's
-- new admin routes are gated on.
--
-- Why this is a separate migration rather than an edit to 0060: 0060 is
-- guarded by `IF EXISTS (SELECT 1 FROM auth_core__role WHERE mode =
-- 'administration') THEN RETURN`. Every deployment that has already run it
-- therefore skips it forever, so adding rows to its VALUES list would seed them
-- on fresh databases only — and leave every existing deployment with routes
-- nobody can reach. That is the 403-for-everyone outcome 0060 exists to
-- prevent, arriving by a different door.
--
-- The grants are transcribed from the `recommended_roles` each legacy runtime
-- handler declares, with the two transcription traps 0060 and 001_initial.sql
-- both document: a BARE `check_api([...])` list parses into the
-- RecommendedRoles defaults (system/super_admin/admin True), and a dict naming
-- only `{"admin": True, "viewer": False, "editor": False}` still leaves
-- super_admin at its default True. `modes.py`, `user_invite.py` and
-- `admin_published_agents.py` are the first form; `user_project_permissions.py`
-- is the second. All four therefore land on super_admin and admin, and on
-- neither editor nor viewer.
--
-- Idempotent and additive: it grants to roles that already exist and never
-- creates one, so a deployment whose administration roles were curated by hand
-- keeps its shape. `system` is omitted, as everywhere else in this corpus — it
-- is not in the Go product's role vocabulary.

-- Two guards, and one guard deliberately NOT taken:
--
--   * the auth_core tables can be absent entirely — elitea-migrate runs against
--     databases whose auth_core schema comes from 001_initial.sql at elitea-main
--     startup, or from a legacy dump. Nothing to grant in that case (0060 opens
--     with the same check).
--   * 0060's VIRGIN-administration-mode guard is NOT reproduced. That guard
--     exists because re-inserting a grant an operator REVOKED through the admin
--     Permissions page would silently undo their decision. These four
--     permissions did not exist before this migration, so nobody can have
--     revoked them, and refusing to grant them to a configured deployment would
--     leave exactly that deployment unable to reach the routes.
DO $$
BEGIN

IF to_regclass('public.auth_core__role') IS NULL
   OR to_regclass('public.auth_core__role_permission') IS NULL THEN
    RAISE NOTICE '0061: auth_core tables absent, nothing to grant';
    RETURN;
END IF;

INSERT INTO public.auth_core__role_permission (role_id, permission)
SELECT role.id, grant_row.permission
FROM public.auth_core__role AS role
JOIN (VALUES
    -- admin/api/v2/modes.py and admin/api/v2/user_invite.py
    ('super_admin', 'modes.users'),
    ('admin', 'modes.users'),
    -- admin/api/v2/user_project_permissions.py
    ('super_admin', 'configuration.roles.user_project_permissions.view'),
    ('super_admin', 'configuration.roles.user_project_permissions.edit'),
    ('admin', 'configuration.roles.user_project_permissions.view'),
    ('admin', 'configuration.roles.user_project_permissions.edit'),
    -- elitea_core/api/v2/admin_published_agents.py
    ('super_admin', 'runtime.admin.published_agents'),
    ('admin', 'runtime.admin.published_agents')
) AS grant_row(role_name, permission) ON grant_row.role_name = role.name
WHERE role.mode = 'administration'
ON CONFLICT (role_id, permission) DO NOTHING;

END
$$;
