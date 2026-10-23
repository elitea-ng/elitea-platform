-- 0108_inventory_permissions.sql — the two DEFAULT-mode grants behind the
-- Inventory facade (ADR-0012 phase P1.5).
--
--   models.applications.inventory.read    ← GET  /inventory/slots/{project_id}
--                                           GET  /inventory/invocations/...
--   models.applications.inventory.invoke  ← POST /inventory/tools/.../invoke
--                                           DELETE /inventory/invocations/...
--
-- CHOSEN, NOT RECOVERED — the third file here to do that, after 0104 and 0106,
-- and for the same reason 0106 gives: the legacy `inventory_plugin` has no
-- `api/` package and no `check_api` call. Its routes were reached by the legacy
-- runtime provider hub over mTLS and authorised by the HOP, never by a project
-- role, so there is nothing in the legacy catalogue to transcribe.
--
-- The split mirrors 0106's read/generate for the same reason: reading capacity
-- is what a screen polls to decide whether to offer a control, and gating that
-- behind the write grant would 403 everyone allowed to look but not to act.
-- The grant blocks below are 0106's, with the two strings substituted. The
-- structure is copied deliberately rather than factored out: each one names
-- its own permissions inline, and a shared procedure would put the strings a
-- migration grants somewhere a reader of that migration cannot see.

DO $$
BEGIN

IF to_regclass('public.auth_core__role') IS NULL
   OR to_regclass('public.auth_core__role_permission') IS NULL THEN
    RAISE NOTICE '0108: auth_core tables absent, nothing to grant';
    RETURN;
END IF;

-- The read: capacity and invocation progress, which every project role may see.
INSERT INTO public.auth_core__role_permission (role_id, permission)
SELECT role.id, 'models.applications.inventory.read'
FROM public.auth_core__role AS role
WHERE role.mode = 'default' AND role.name IN ('admin', 'editor', 'viewer')
ON CONFLICT (role_id, permission) DO NOTHING;

-- The write: starting and cancelling an invocation.
INSERT INTO public.auth_core__role_permission (role_id, permission)
SELECT role.id, 'models.applications.inventory.invoke'
FROM public.auth_core__role AS role
WHERE role.mode = 'default' AND role.name IN ('admin', 'editor')
ON CONFLICT (role_id, permission) DO NOTHING;

-- The override delivery. Only projects that ALREADY carry per-project rows are
-- touched: a project role with no override rows still falls back to the central
-- matrix above, and handing it a snapshot it never had would freeze it out of
-- every future central grant — the very hole this block exists to close.
IF to_regclass('public.auth_core__project_role') IS NULL
   OR to_regclass('public.auth_core__project_role_permission') IS NULL THEN
    RAISE NOTICE '0108: no per-project permission tables, central grants are the whole story here';
    RETURN;
END IF;

INSERT INTO public.auth_core__project_role_permission (project_id, role_id, permission)
SELECT DISTINCT overridden.project_id, overridden.role_id, grant_row.permission
FROM (
    SELECT DISTINCT project_id, role_id
    FROM public.auth_core__project_role_permission
    WHERE role_id IS NOT NULL
) AS overridden
JOIN public.auth_core__project_role AS project_role
  ON project_role.id = overridden.role_id
 AND project_role.project_id = overridden.project_id
CROSS JOIN (VALUES
    ('models.applications.inventory.read', ARRAY['admin', 'editor', 'viewer']),
    ('models.applications.inventory.invoke', ARRAY['admin', 'editor'])
) AS grant_row(permission, roles)
WHERE project_role.name = ANY (grant_row.roles)
ON CONFLICT (project_id, role_id, permission) DO NOTHING;

END
$$;
