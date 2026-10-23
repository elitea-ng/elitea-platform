-- 0090_project_override_reconciliation.sql — deliver the corpus's default-mode
-- grants to the projects that carry their own permission rows.
--
-- THE DEFECT. legacyrbac's projectPermissions() reads the central default-mode
-- grants under `WHERE NOT EXISTS (SELECT 1 FROM project_permissions)`
-- (internal/infra/legacyrbac/postgres.go). Suppression is ALL OR NOTHING per
-- caller: if any project role the caller holds carries one
-- auth_core__project_role_permission row, the whole central set is discarded
-- for that caller in that project.
--
-- Every permission migration in this corpus writes CENTRAL rows only. So every
-- one of them is inert in a project that carries overrides, and the product's
-- own admin console is what creates those overrides:
--
--   "Apply to Projects"                     internal/api/v2/admin/roles.go
--   PUT /admin/permissions/{public|support}/{mode}
--   PUT /admin/user_project_permissions/administration
--                                           internal/api/v2/admin/user_project_permissions.go
--
-- An operator presses one of them once. Every touched project stops reading the
-- central matrix. The next release ships a migration for a newly gated route,
-- the migration writes central rows, and every member of those projects gets
-- 403 "insufficient permissions" on the new route with nothing to point at.
-- The end-to-end stack already lives with this: apps/elitea-web/scripts/
-- e2e-stack.sh has to hand-list `models.applications.index_meta.details`
-- "because project 1 carries per-project rows".
--
-- THE RULE IS NOT THE BUG. The suppression rule is the legacy runtime's
-- documented contract and TestAProjectWithItsOwnGrantsIsRefusedEveryGate pins
-- it. What the legacy runtime pairs it with, and this corpus never adopted, is
-- a backfill: when the platform adds a permission, the projects that hold their
-- own matrix are given it too. This file is that backfill, applied once to the
-- whole estate.
--
-- WHAT IT WRITES, AND THE ONE THING IT WILL NOT DO.
--
-- It is keyed on (project_id, role_id), because that is how the resolver reads:
-- suppression is per caller-role-set, so a project role that carries NO
-- override rows still falls back and must not be handed a snapshot it never
-- had.
--
-- It does NOT copy every central grant. A pair (role name, permission) that
-- some project already carries as an override is left alone, everywhere. The
-- reason is operator intent: those admin screens SET a matrix, so an operator
-- can narrow a project by saving it without a permission. A blanket copy of the
-- central set would silently reverse every such decision across the estate on
-- upgrade. A pair that appears in NO project's override rows cannot have been
-- revoked by anyone — it is platform surface that never reached the estate at
-- all — and that is exactly the class this file exists to deliver.
--
-- The cost of that restraint is stated plainly: a permission that ONE project
-- revoked stays undelivered to EVERY project with overrides. Repairing those
-- needs an operator decision per project, and the admin console is where that
-- belongs.
--
-- KEEPING IT TRUE. This file reconciles the corpus as of 0089. A later
-- migration that adds a central default-mode grant must carry its own override
-- block, or the projects with overrides never see it.
-- migrations/project_override_reconciliation_test.go is the gate: it fails when
-- a shared migration after 0089 writes auth_core__role_permission in `default`
-- mode and does not also write auth_core__project_role_permission.
DO $$
BEGIN

IF to_regclass('public.auth_core__role') IS NULL
   OR to_regclass('public.auth_core__role_permission') IS NULL
   OR to_regclass('public.auth_core__project_role') IS NULL
   OR to_regclass('public.auth_core__project_role_permission') IS NULL THEN
    RAISE NOTICE '0089: auth_core tables absent, nothing to reconcile';
    RETURN;
END IF;

INSERT INTO public.auth_core__project_role_permission (project_id, role_id, permission)
SELECT DISTINCT overridden.project_id, overridden.role_id, central_grant.permission
FROM (
    SELECT DISTINCT project_id, role_id
    FROM public.auth_core__project_role_permission
    WHERE role_id IS NOT NULL
) AS overridden
JOIN public.auth_core__project_role AS project_role
  ON project_role.id = overridden.role_id
 AND project_role.project_id = overridden.project_id
JOIN public.auth_core__role AS central_role
  ON central_role.name = project_role.name
 AND central_role.mode = 'default'
JOIN public.auth_core__role_permission AS central_grant
  ON central_grant.role_id = central_role.id
-- Leave alone any pair a project already states for itself. See the header:
-- an operator can have removed it on purpose, and this file cannot tell a
-- removal from an absence.
WHERE NOT EXISTS (
    SELECT 1
    FROM public.auth_core__project_role_permission AS stated
    JOIN public.auth_core__project_role AS stated_role
      ON stated_role.id = stated.role_id
     AND stated_role.project_id = stated.project_id
    WHERE stated_role.name = project_role.name
      AND stated.permission = central_grant.permission
)
ON CONFLICT (project_id, role_id, permission) DO NOTHING;

END
$$;
