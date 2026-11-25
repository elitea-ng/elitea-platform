-- 0273_entitlement_export_manage.sql — the write permission of the
-- entitlement_export Configuration section (issue #3026, ADR-0059 decision 2).
--
-- The section lists the SCIM clients that may read the entitlement tree
-- (/api/v2/scim/v2/entitlements). Its save was gated by entitlements.export,
-- a read permission (0209): a role given the export to read, such as an
-- auditor, could then widen which tools read it. The save now takes
-- entitlements.export.manage, and entitlements.export reads the section.
--
-- The rule of 0261 for a new administration-mode permission: both
-- administrator roles, super_admin and admin, in a new shared migration. No
-- narrow role and no legacy role (system, editor, viewer) gets it. The
-- legacy path of the section (runtime.plugins with entitlements.export) is
-- unchanged, so no holder of today's grants loses the save.
--
-- Additive and idempotent. No BEGIN/COMMIT: the ledgered runner wraps the
-- file. The shared corpus also runs on databases without the auth_core
-- tables; there the file raises a notice and returns.

DO $$
BEGIN
    IF to_regclass('public.auth_core__role') IS NULL
       OR to_regclass('public.auth_core__role_permission') IS NULL THEN
        RAISE NOTICE '0273: auth_core tables absent, nothing to grant';
        RETURN;
    END IF;

    INSERT INTO public.auth_core__role_permission (role_id, permission)
    SELECT role.id, 'entitlements.export.manage'
    FROM public.auth_core__role AS role
    WHERE role.mode = 'administration' AND role.name IN ('super_admin', 'admin')
    ON CONFLICT (role_id, permission) DO NOTHING;
END
$$;
