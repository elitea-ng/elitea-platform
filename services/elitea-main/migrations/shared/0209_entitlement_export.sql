-- 0209_entitlement_export.sql — the entitlement export permission
-- (ADR-0044 decision 5, issue #1501).
--
-- entitlements.export gates the admin entitlement export
-- (GET /api/v2/admin/entitlements, CSV or JSON): who holds which role on
-- which project and agent, and which governance capabilities each role
-- carries. Granted to super_admin and admin in the administration mode only,
-- like retention.manage (0195): the export lists every account and every
-- administrator, so it is a platform-administrator read. The same grant is
-- seeded on a fresh database by internal/infra/db/migrations/001_initial.sql.
--
-- No table: the export reads the grant tables as they are. The SCIM
-- entitlement tree (/api/v2/scim/v2/entitlements) authenticates a SCIM
-- client credential and needs no permission.
--
-- Additive and idempotent. No BEGIN/COMMIT: the ledgered runner wraps the file.

DO $$
BEGIN
    IF to_regclass('public.auth_core__role') IS NULL
       OR to_regclass('public.auth_core__role_permission') IS NULL THEN
        RAISE NOTICE '0209: auth_core tables absent, nothing to grant';
        RETURN;
    END IF;

    INSERT INTO public.auth_core__role_permission (role_id, permission)
    SELECT role.id, 'entitlements.export'
    FROM public.auth_core__role AS role
    WHERE role.mode = 'administration' AND role.name IN ('super_admin', 'admin')
    ON CONFLICT (role_id, permission) DO NOTHING;
END
$$;
