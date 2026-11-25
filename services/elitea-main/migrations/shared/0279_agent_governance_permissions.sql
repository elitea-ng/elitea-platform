-- 0279_agent_governance_permissions.sql — the agent governance pair of
-- ADR-0059 decision 2 (issue #2552, story R0.15): agents.governance.view and
-- agents.governance.manage, granted to both administrator roles.
--
-- The agent registry, the delegation review, the worker egress hosts and the
-- agent governance Configuration sections were the last subjects that only
-- configuration.governance gated. Each gate now accepts the narrow name beside
-- the legacy one: the reads take agents.governance.view, the delegation
-- review scan and the section saves agents.governance.manage. A holder of
-- configuration.governance sees no change.
--
-- The rule of 0261 for a new administration-mode permission: both
-- administrator roles, super_admin and admin, in a new shared migration. No
-- narrow role and no legacy role (system, editor, viewer) gets either name:
-- the agent governance role is deferred (owner decision 10) and is one
-- migration later. This is the third and last seeding migration of the R0
-- epic.
--
-- Additive and idempotent: delete nothing, update nothing. Do not edit 0060
-- (it returns early on a database that already ran it) or 001_initial.sql.
-- No BEGIN/COMMIT: the ledgered runner wraps the file. The shared corpus also
-- runs on databases without the auth_core tables; there the file raises a
-- notice and returns.

DO $$
BEGIN
    IF to_regclass('public.auth_core__role') IS NULL
       OR to_regclass('public.auth_core__role_permission') IS NULL THEN
        RAISE NOTICE '0279: auth_core tables absent, nothing to grant';
        RETURN;
    END IF;

    INSERT INTO public.auth_core__role_permission (role_id, permission)
    SELECT role.id, grant_name.permission
    FROM public.auth_core__role AS role
    CROSS JOIN (VALUES ('agents.governance.view'), ('agents.governance.manage')) AS grant_name (permission)
    WHERE role.mode = 'administration' AND role.name IN ('super_admin', 'admin')
    ON CONFLICT (role_id, permission) DO NOTHING;
END
$$;
