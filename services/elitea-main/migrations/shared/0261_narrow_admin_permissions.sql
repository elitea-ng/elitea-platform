-- 0261_narrow_admin_permissions.sql — the narrow administration permissions of
-- ADR-0059 decision 2, granted to both existing administrator roles (issue
-- #2491, ADR-0059 R0.2, migration A of decision 4).
--
-- THE RULE FOR EVERY LATER PERMISSION of ADR-0059 and of ADR-0058. Copy it.
-- A new administration-mode permission goes to BOTH administrator roles,
-- `super_admin` and `admin`, in a NEW shared migration, with
-- ON CONFLICT (role_id, permission) DO NOTHING. Two exceptions:
--
--   identity.sso.trust.manage    super_admin only. Changing whom an identity
--                                provider vouches for can become any account
--                                (#2558); it is equivalent to super_admin.
--   security.signals.reidentify  no built-in role (ADR-0058).
--
-- Nothing is granted to `system`, `editor` or `viewer`, and no narrow role
-- gets a grant here: the built-in narrow roles and their grants are migration
-- B (story R0.7).
--
-- WHAT IS GRANTED. 25 of the 27 names of decision 2. The pair
-- agents.governance.view and agents.governance.manage is story R0.15 (#2552).
-- No gate accepts any of the 25 yet: stories R0.3 to R0.6 wire them, beside
-- the legacy name each one narrows, so a holder of the legacy name sees no
-- change. The grants land first because the grant gate test
-- (router_permission_grant_gate_test.go) fails on a gated name nobody holds.
--
-- THE ONE PRIVILEGE INCREASE. projects.settings.view and
-- projects.settings.manage are new capabilities, not narrowings: once story
-- R0.6b (#2585) wires their gate, both administrator roles reach the guarded
-- settings of projects they are not members of. The upgrade note
-- (docs/upgrading/2026-11-23-2491-narrow-administration-permissions.md) says so.
--
-- DELETE NOTHING, UPDATE NOTHING. Every existing grant stays. Applying the
-- file twice writes nothing the second time. Do not edit 0060 (it returns
-- early on a database that already ran it) or 001_initial.sql to add a name.
--
-- GUARD. The shared corpus also runs on databases without the auth_core
-- tables; there the file raises a notice and returns. No BEGIN/COMMIT: the
-- ledgered runner executes each file inside one transaction with its ledger
-- row (migrate/runner.go apply).

DO $$
BEGIN
    IF to_regclass('public.auth_core__role') IS NULL
       OR to_regclass('public.auth_core__role_permission') IS NULL THEN
        RAISE NOTICE '0261: auth_core tables absent, nothing to grant';
        RETURN;
    END IF;

    -- Both administrator roles: 24 names.
    INSERT INTO public.auth_core__role_permission (role_id, permission)
    SELECT role.id, grant_row.permission
    FROM public.auth_core__role AS role
    CROSS JOIN (VALUES
        -- Models and spend.
        ('llm.catalogue.view'),
        ('llm.catalogue.manage'),
        ('llm.policy.view'),
        ('llm.policy.manage'),
        ('llm.guardrails.manage'),
        ('llm.usage.view'),
        ('llm.alerts.manage'),
        -- Platform.
        ('platform.config.view'),
        ('platform.config.manage'),
        ('platform.email.manage'),
        ('platform.maintenance.manage'),
        ('platform.operations.view'),
        ('platform.operations.manage'),
        ('mcp.catalogue.manage'),
        -- People and access: identity setup other than trust.
        ('identity.sso.view'),
        ('identity.sso.settings.manage'),
        -- Projects.
        ('projects.members.manage'),
        ('projects.settings.view'),
        ('projects.settings.manage'),
        -- People and access: roles and break-glass.
        ('access.roles.view'),
        ('access.breakglass.request'),
        ('access.breakglass.approve'),
        ('access.breakglass.review'),
        ('access.breakglass.manage')
    ) AS grant_row(permission)
    WHERE role.mode = 'administration' AND role.name IN ('super_admin', 'admin')
    ON CONFLICT (role_id, permission) DO NOTHING;

    -- Identity trust: super_admin alone.
    INSERT INTO public.auth_core__role_permission (role_id, permission)
    SELECT role.id, 'identity.sso.trust.manage'
    FROM public.auth_core__role AS role
    WHERE role.mode = 'administration' AND role.name = 'super_admin'
    ON CONFLICT (role_id, permission) DO NOTHING;
END
$$;
