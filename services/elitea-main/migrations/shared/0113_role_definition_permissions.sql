-- 0113_role_definition_permissions.sql — the three ADMINISTRATION-mode grants
-- behind role create, rename and delete (gap G9).
--
--   configuration.roles.roles.create  ← POST   /admin/roles/{scope}/{mode}
--   configuration.roles.roles.edit    ← PUT    /admin/roles/{scope}/{mode}
--   configuration.roles.roles.delete  ← DELETE /admin/roles/{scope}/{mode}
--
-- RECOVERED, NOT CHOSEN. All three strings are pylon's own: they are the
-- `permissions` list of the four handlers in
-- legacy/plugins/admin/api/v2/roles.py, and all four are already in the
-- catalogue this repository transcribed into
-- testdata/postgres/legacy-rbac-matrix.json (`global_permission_catalog`).
--
-- 0068 and 0085 granted the fourth, `configuration.roles.roles.view`. The
-- three WRITE strings were granted by no migration in this corpus. That is not
-- a small omission: internal/api/router_permission_grant_gate_test.go exists
-- because a route gated on a permission no migration grants answers 403 to
-- every caller on a clean database while every seeded database hides it. The
-- three routes ship in the same change as this file, so the gate never sees
-- them ungranted.
--
-- WHO HOLDS THEM. `super_admin`, `admin` and `system` in the `administration`
-- mode. That is the legacy matrix row-for-row: in
-- testdata/postgres/legacy-rbac-matrix.json those three roles hold all four
-- `configuration.roles.roles.*` strings in every mode, and `editor` and
-- `viewer` hold `.view` alone. 0085 already transcribed that split for the
-- read; this file transcribes it for the writes and widens nothing.
--
-- `system` is included although internal/infra/db/migrations/001_initial.sql
-- seeds no administration-mode `system` role. The WHERE clause simply matches
-- nothing there. On a pylon-backed database the role does exist, and omitting
-- it would NARROW that deployment's existing matrix.
--
-- WHY A NEW FILE. 0060 returns early when any administration-mode role exists,
-- so an edit there would seed fresh databases only and leave every running
-- deployment at 403; migrations are also checksum-immutable, so an edit to a
-- landed file is not an option. The to_regclass guard makes this file inert on
-- a database that has no auth_core tables at all, and ON CONFLICT DO NOTHING
-- makes a repeat run a no-op.
DO $$
BEGIN

IF to_regclass('public.auth_core__role') IS NULL
   OR to_regclass('public.auth_core__role_permission') IS NULL THEN
    RAISE NOTICE '0113: auth_core tables absent, nothing to grant';
    RETURN;
END IF;

INSERT INTO public.auth_core__role_permission (role_id, permission)
SELECT role.id, grant_row.permission
FROM public.auth_core__role AS role
CROSS JOIN (VALUES
    ('configuration.roles.roles.create'),
    ('configuration.roles.roles.edit'),
    ('configuration.roles.roles.delete')
) AS grant_row(permission)
WHERE role.mode = 'administration' AND role.name IN ('super_admin', 'admin', 'system')
ON CONFLICT (role_id, permission) DO NOTHING;

END
$$;
