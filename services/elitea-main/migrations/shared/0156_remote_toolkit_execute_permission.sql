-- 0156_remote_toolkit_execute_permission.sql — the ONE default-mode grant
-- behind the desktop's remote toolkit call.
--
--   models.applications.tool.execute
--     ← POST /api/v2/elitea_core/remote_toolkit_call/prompt_lib/{p}/{toolkit}
--       (internal/api/v2/desktopops/remote_toolkit.go, RemoteToolkitPermission)
--
-- A NEW STRING, NOT A RECOVERED ONE. ADR-0029 decision 5b: a desktop runs its
-- agent loop locally and calls one tool of one credentialed toolkit through
-- the cloud worker, under the caller's grants and with write operations
-- allowed. It needs an EXECUTE permission rather than
-- `models.applications.tool.patch` (what test_tool takes): patch is an edit of
-- the toolkit, and a viewer who may chat with an agent may run that agent's
-- tools in a cloud turn without being allowed to edit them. pylon has no
-- `check_api` declaration for it, so testdata/legacy/legacy-rbac-static-catalog.json
-- does not list it; the route takes it from a package constant, as the
-- evaluation routes do for their platform strings.
--
-- WHO HOLDS IT. The roles that may chat, i.e. the holders of
-- `models.chat.messages.create` (0070): `admin`, `editor` and `viewer` in the
-- default mode, and each of them ONLY where it actually holds the chat send —
-- centrally, and per project in a saved matrix. A remote toolkit call runs
-- inside a chat turn (a live local turn), so a role an operator denied chat
-- must not be handed the tools. `system` and `super_admin` are omitted, as
-- everywhere else in this corpus: Go seeds neither role in the default mode.
--
-- WHY A NEW FILE. Migrations are checksum-immutable, and 0060 early-returns on
-- every configured database, so neither can carry it.
--
-- 0060's VIRGIN-mode guard is NOT reproduced, for the reason 0070 and 0120
-- give: this permission has never existed on any deployment, so no operator
-- can have revoked it.
--
-- THE OVERRIDE BLOCK. legacyrbac's projectPermissions() reads the central
-- grants only for a caller with NO per-project rows (shared/0090's header).
-- The admin console writes those rows whenever an operator saves a matrix, so
-- the second block hands the string to every admin/editor/viewer project role
-- whose saved rows include `models.chat.messages.create` in that project. The
-- string has never been granted anywhere, so no saved matrix can have omitted
-- it deliberately; the chat send is what the matrix did decide.
--
-- Idempotent and additive: it grants to roles that already exist, never creates
-- one, and conflicts are ignored.
DO $$
BEGIN

IF to_regclass('public.auth_core__role') IS NULL
   OR to_regclass('public.auth_core__role_permission') IS NULL THEN
    RAISE NOTICE '0156: auth_core tables absent, nothing to grant';
    RETURN;
END IF;

INSERT INTO public.auth_core__role_permission (role_id, permission)
SELECT role.id, grant_row.permission
FROM public.auth_core__role AS role
CROSS JOIN (VALUES
    ('models.applications.tool.execute')
) AS grant_row(permission)
WHERE role.mode = 'default' AND role.name IN ('admin', 'editor', 'viewer')
  AND EXISTS (
      SELECT 1 FROM public.auth_core__role_permission AS chat
      WHERE chat.role_id = role.id AND chat.permission = 'models.chat.messages.create'
  )
ON CONFLICT (role_id, permission) DO NOTHING;

IF to_regclass('public.auth_core__project_role') IS NULL
   OR to_regclass('public.auth_core__project_role_permission') IS NULL THEN
    RAISE NOTICE '0156: no per-project permission tables, central grants are the whole story here';
    RETURN;
END IF;

INSERT INTO public.auth_core__project_role_permission (project_id, role_id, permission)
SELECT DISTINCT overridden.project_id, overridden.role_id, grant_row.permission
FROM (
    -- Only a (project, role) that already holds the chat send in its saved
    -- matrix: a matrix that withheld chat from a role withheld its tools too.
    SELECT DISTINCT project_id, role_id
    FROM public.auth_core__project_role_permission
    WHERE role_id IS NOT NULL AND permission = 'models.chat.messages.create'
) AS overridden
JOIN public.auth_core__project_role AS project_role
  ON project_role.id = overridden.role_id
 AND project_role.project_id = overridden.project_id
CROSS JOIN (VALUES
    ('models.applications.tool.execute', ARRAY['admin', 'editor', 'viewer'])
) AS grant_row(permission, roles)
WHERE project_role.name = ANY (grant_row.roles)
ON CONFLICT (project_id, role_id, permission) DO NOTHING;

END
$$;
