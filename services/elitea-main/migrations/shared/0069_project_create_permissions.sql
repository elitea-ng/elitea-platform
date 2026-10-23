-- 0069_project_create_permissions.sql — the administration-mode grants that
-- make the new project CREATE and DELETE routes reachable (#333).
--
-- WHY THIS FILE EXISTS. `projects.projects.project.create` and
-- `projects.projects.project.delete` are real legacy permission names —
-- testdata/legacy/legacy-rbac-static-catalog.json carries both, transcribed
-- from the `check_api` declarations on `AdminAPI.post` and `AdminAPI.delete` in
-- legacy/plugins/projects/api/v2/project.py. Neither name is granted by ANY
-- migration in this corpus, and neither is seeded by
-- internal/infra/db/migrations/001_initial.sql. So the routes had to land with
-- their grants, for the reason 0063's header states and 0068's repeats: gating
-- a route on a permission nothing grants is 403-for-everyone, which reads as a
-- broken page rather than as a missing grant.
--
-- WHERE THE SPLIT COMES FROM. testdata/postgres/legacy-rbac-matrix.json holds
-- both strings for `administration` / {`admin`, `super_admin`, `system`}. That
-- is the whole holder set this corpus can reproduce: `system` is omitted here
-- exactly as 0060, 001_initial.sql and e2e-stack.sh omit it — it is not in the
-- Go product's role vocabulary (users.go `adminRolePriority` is
-- super_admin/admin/editor/viewer) and nothing assigns it. `editor` and
-- `viewer` are granted nothing, which is what the handler declares:
-- `{"admin": True, "viewer": False, "editor": False}`.
--
-- ONLY THE administration MODE. The legacy matrix also lists both names under
-- `default` and `developer`, but only for `super_admin` and `system` — and Go
-- seeds neither role in either mode. Granting into the DEFAULT mode would be
-- actively wrong here, for the reason 001_initial.sql gives where it seeds the
-- administration grants: projectPermissions() falls back to the central
-- default-mode grants BY ROLE NAME when a project carries no per-project rows,
-- so a default-mode grant to `admin` would hand project-create to every
-- project admin on every fresh project. The routes resolve in `administration`
-- mode (RequireCentralPermissions), so an administration-mode grant is the only
-- one that can reach them.
--
-- BLAST RADIUS. Additive only. It grants to administration-mode roles that
-- already exist, never creates one, and ignores conflicts. Before this file no
-- caller could create or delete a project through the Go stack at all, so no
-- deployment loses a capability and none gains one it did not already have
-- through the legacy runtime.
--
-- 0060's VIRGIN-mode guard is NOT reproduced, for the reason 0061/0062/0063/
-- 0066/0068 all give: these permissions have never existed on a Go deployment,
-- so no operator can have revoked them, and skipping configured deployments
-- would leave exactly those unable to reach the routes.
DO $$
BEGIN

IF to_regclass('public.auth_core__role') IS NULL
   OR to_regclass('public.auth_core__role_permission') IS NULL THEN
    RAISE NOTICE '0069: auth_core tables absent, nothing to grant';
    RETURN;
END IF;

INSERT INTO public.auth_core__role_permission (role_id, permission)
SELECT role.id, grant_row.permission
FROM public.auth_core__role AS role
CROSS JOIN (VALUES
    ('projects.projects.project.create'),
    ('projects.projects.project.delete')
) AS grant_row(permission)
WHERE role.mode = 'administration' AND role.name IN ('admin', 'super_admin')
ON CONFLICT (role_id, permission) DO NOTHING;

END
$$;
