-- 0229_agent_memory_permissions.sql — the agent memory permissions
-- (ADR-0050 decision 1b, issue 1607).
--
--   models.applications.memory.list     read an agent's memory entries
--                                       (default mode: admin, editor, viewer)
--   models.applications.memory.update   edit, pin, archive an entry
--   models.applications.memory.delete   delete an entry, clear the memory
--   models.applications.memory.approve  approve or reject a pending entry
--                                       (the three: default mode admin)
--
-- The agent's owners (agent_registry business_owner_id, technical_owner_id)
-- hold update, delete and approve on their own agent without the grant: the
-- route checks ownership itself. Anyone may delete or correct an entry about
-- themself with list alone. The tables are tenant/0163.
--
-- 0060 returns early on an existing database, so
-- internal/infra/db/migrations/001_initial.sql seeds the same grants on a
-- fresh one. A project whose matrix was saved gets the strings on its roles
-- of the same names, as 0212 does: the strings never existed, so no saved
-- matrix withheld them on purpose.
--
-- Additive only. Idempotent. Every statement tolerates a database without
-- the auth_core tables. No BEGIN/COMMIT: the ledgered runner wraps the file.
DO $$
BEGIN
    IF to_regclass('public.auth_core__role') IS NULL
       OR to_regclass('public.auth_core__role_permission') IS NULL THEN
        RAISE NOTICE '0229: auth_core tables absent, nothing to grant';
        RETURN;
    END IF;

    -- The mode spelled as a literal, one per INSERT: the router's grant
    -- gate (router_permission_grant_gate_test.go) reads the grants of this
    -- directory per `mode = '...'` block.
    INSERT INTO public.auth_core__role_permission (role_id, permission)
    SELECT role.id, 'models.applications.memory.list'
    FROM public.auth_core__role AS role
    WHERE role.mode = 'default' AND role.name IN ('admin', 'editor', 'viewer')
    ON CONFLICT (role_id, permission) DO NOTHING;

    INSERT INTO public.auth_core__role_permission (role_id, permission)
    SELECT role.id, grant_row.permission
    FROM public.auth_core__role AS role
    CROSS JOIN (VALUES
        ('models.applications.memory.update'),
        ('models.applications.memory.delete'),
        ('models.applications.memory.approve')
    ) AS grant_row(permission)
    WHERE role.mode = 'default' AND role.name = 'admin'
    ON CONFLICT (role_id, permission) DO NOTHING;

    IF to_regclass('public.auth_core__project_role') IS NULL
       OR to_regclass('public.auth_core__project_role_permission') IS NULL THEN
        RETURN;
    END IF;

    INSERT INTO public.auth_core__project_role_permission (project_id, role_id, permission)
    SELECT DISTINCT saved.project_id, saved.role_id, grant_row.permission
    FROM (
        SELECT DISTINCT project_id, role_id
        FROM public.auth_core__project_role_permission
        WHERE role_id IS NOT NULL
    ) AS saved
    JOIN public.auth_core__project_role AS project_role
      ON project_role.id = saved.role_id
     AND project_role.project_id = saved.project_id
    CROSS JOIN (VALUES
        ('models.applications.memory.list', 'admin'),
        ('models.applications.memory.list', 'editor'),
        ('models.applications.memory.list', 'viewer'),
        ('models.applications.memory.update', 'admin'),
        ('models.applications.memory.delete', 'admin'),
        ('models.applications.memory.approve', 'admin')
    ) AS grant_row(permission, role_name)
    WHERE project_role.name = grant_row.role_name
    ON CONFLICT (project_id, role_id, permission) DO NOTHING;
END
$$;
