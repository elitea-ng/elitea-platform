-- 0121_bulk_invite_permissions.sql — the two administration-mode grants the
-- cross-project bulk membership invite needs.
--
--   invites.bulkusers
--   invites.bulkprojects
--
-- WHY THESE TWO STRINGS. The route they gate,
-- `POST /admin/invites_bulk/administration`
-- (internal/api/v2/admin/invites_bulk.go), replaces two pylon console pages:
-- legacy/plugins/admin/api/v2/invites_bulkusers.py and invites_bulkprojects.py.
-- Each declares its own permission in its `check_api` decorator
-- (invites_bulkusers.py:35, invites_bulkprojects.py:35) rather than the
-- project-membership one, so this is parity, not a new policy. The gate accepts
-- either string, so an operator who held either console page keeps the
-- capability through one route.
--
-- WHERE THE ROLE SPLIT COMES FROM. legacy/plugins/admin/module.py:511-517 and
-- :527-533 register both console subsections with the SAME
-- `recommended_roles`: `super_admin` true, `admin` true, `editor` false,
-- `viewer` false, in every mode. That is exactly the split
-- shared/0082_admin_panel_permissions.sql gives the other admin-console writes,
-- so this file writes the same two role names in the same mode. Adding a whole
-- project's membership in one action is an administrator's action; an editor
-- who could do it could grant themselves admin in every project on the
-- platform.
--
-- WHY A NEW FILE. Migrations are checksum-immutable, so 0082 cannot be
-- extended. 0060 is doubly unavailable: it is applied everywhere AND it
-- early-returns on any database that already carries an administration-mode
-- role, which is every database an operator has ever configured. A grant
-- appended there would reach only never-configured installs — the exact
-- opposite of the set that needs it.
--
-- 0060's VIRGIN-mode guard is NOT reproduced, for the reason 0061, 0066, 0068,
-- 0079, 0085, 0104, 0116 and 0120 all give: neither permission has ever existed
-- on any Go deployment, so no operator can have revoked it, and skipping
-- configured deployments would leave exactly those unable to reach the route.
--
-- NO PER-PROJECT OVERRIDE BLOCK, and that is not the omission shared/0090's
-- header warns about. That block exists because legacyrbac's
-- projectPermissions() discards the central DEFAULT-mode set for a caller as
-- soon as one auth_core__project_role_permission row exists on a role they
-- hold. These two grants are `administration` mode, which the resolver reads
-- from auth_core__user_role/auth_core__role directly and which no per-project
-- matrix can suppress — the same reason 0082 carries no override block either.
-- migrations/project_override_reconciliation_test.go asserts the rule for
-- default-mode grants only.
--
-- WRITTEN AS 0121 AND RENUMBERED AT MERGE IF NEEDED. 0121 was free when this
-- file was authored. Concurrent packages claim numbers the same way; only the
-- merge can see a collision, and the number belongs to whichever lands first.
--
-- Idempotent and additive: it grants to roles that already exist, never creates
-- one, and conflicts are ignored.
DO $$
BEGIN

IF to_regclass('public.auth_core__role') IS NULL
   OR to_regclass('public.auth_core__role_permission') IS NULL THEN
    RAISE NOTICE '0121: auth_core tables absent, nothing to grant';
    RETURN;
END IF;

INSERT INTO public.auth_core__role_permission (role_id, permission)
SELECT role.id, grant_row.permission
FROM public.auth_core__role AS role
CROSS JOIN (VALUES
    ('invites.bulkusers'),
    ('invites.bulkprojects')
) AS grant_row(permission)
WHERE role.mode = 'administration' AND role.name IN ('super_admin', 'admin')
ON CONFLICT (role_id, permission) DO NOTHING;

END
$$;
