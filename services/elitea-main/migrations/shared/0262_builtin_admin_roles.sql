-- 0262_builtin_admin_roles.sql — the built-in administration roles of ADR-0059
-- decision 3, their grants and the record of which roles are built-in (issue
-- #2496, ADR-0059 R0.7, migration B of decision 4).
--
-- BUILT-IN IS A RECORDED FACT, NOT A NAME. A deployment may already have a
-- custom role called `llm_manager` or `security_auditor`. Such a role must not
-- be silently upgraded (it would receive `access.breakglass.review`, which is
-- seeded only) and must not be locked. So this file writes one row of
-- elitea_auth.admin_role_seeds for each role it inserts and for the four roles
-- 001_initial.sql seeds, and the roles API locks exactly the roles with a row
-- (internal/api/v2/admin/roles_crud.go, seededRole). A role of a built-in name
-- that already exists WITHOUT a row is left untouched, gets no row and is
-- reported by a notice.
--
-- THE SEED TABLE. ADR-0058 story E5.1 (#2403) creates the same table if it
-- merges first, with this definition character for character. Copy it from
-- here; the two must not differ. `marker`:
--
--   declared      the registry (internal/adminaccess, BuiltInRoles) declares
--                 the role's set: super_admin, admin and the four new roles.
--   as_found      the registry declares no set; the role is locked to the set
--                 it held when this file ran: editor and viewer.
--   unassignable  the role is locked and never assigned: the administration
--                 `system` role of a legacy-backed database.
--
-- `role_id` is the key; `name` and `mode` are the role as recorded when the row
-- was written, `seeded_by` the migration that wrote it. Deleting the role
-- deletes its row.
--
-- THE ROLES (decision 3; display names in the console):
--
--   llm_manager            LLM manager                          9 names
--   project_administrator  Project administrator (all projects) 15 names
--   emergency_access       Emergency access                     1 name
--   security_auditor       Security auditor                     3 names here;
--                          ADR-0058 E5.1 (#2403) adds
--                          security.signals.view,
--                          security.signals.project.view and
--                          runs.evidence.read.
--
-- security_auditor, three cases, so that either order of this file and E5.1
-- gives the same six grants:
--   absent                  insert it, its three grants and its seed row;
--   present with a seed row E5.1 created it: add the three grants;
--   present without one     a custom role: change nothing, raise a notice.
-- The other three new roles follow the same rule (the second case only arises
-- when the file runs again).
--
-- LEGACY-BACKED DATABASES are detected by the administration role `system`
-- (decision 3), not by a setting. There `system` gets a seed row marked
-- `unassignable`; `editor` and `viewer` are `as_found` on every database.
--
-- THE ID SEQUENCE. 001_initial.sql inserts the administration roles with
-- explicit ids 4 to 7, so the serial sequence of auth_core__role can be behind
-- its largest id on a database that never created a role through the API. The
-- sequence is advanced to that id first, and never moved back.
--
-- NOBODY IS ASSIGNED A ROLE HERE. DELETE NOTHING, UPDATE NOTHING. Every
-- existing grant stays, `system`, `editor` and `viewer` included. Applying the
-- file twice writes nothing the second time. Do not edit 0060 or
-- 001_initial.sql.
--
-- GUARD. The shared corpus also runs on databases without the auth_core
-- tables; there the file raises a notice and returns. No BEGIN/COMMIT: the
-- ledgered runner executes each file inside one transaction with its ledger
-- row (migrate/runner.go apply).

DO $$
DECLARE
    role_sequence text;
    largest_id    integer;
    sequence_last bigint;
    built_in      record;
    found_role    integer;
    has_seed      boolean;
BEGIN
    IF to_regclass('public.auth_core__role') IS NULL
       OR to_regclass('public.auth_core__role_permission') IS NULL THEN
        RAISE NOTICE '0262: auth_core tables absent, no built-in role to seed';
        RETURN;
    END IF;

    -- 1. The id sequence, advanced to the largest id.
    role_sequence := pg_get_serial_sequence('public.auth_core__role', 'id');
    SELECT max(id) INTO largest_id FROM public.auth_core__role;
    IF role_sequence IS NOT NULL AND largest_id IS NOT NULL THEN
        EXECUTE format('SELECT last_value FROM %s', role_sequence) INTO sequence_last;
        IF largest_id >= sequence_last THEN
            PERFORM setval(role_sequence, largest_id, true);
        END IF;
    END IF;

    -- 2. The seed table.
    CREATE SCHEMA IF NOT EXISTS elitea_auth;
    CREATE TABLE IF NOT EXISTS elitea_auth.admin_role_seeds (
        role_id   integer     PRIMARY KEY
                  REFERENCES public.auth_core__role (id) ON DELETE CASCADE,
        name      varchar(64) NOT NULL,
        mode      varchar(64) NOT NULL,
        marker    text        NOT NULL
                  CHECK (marker IN ('declared', 'as_found', 'unassignable')),
        seeded_by text        NOT NULL,
        seeded_at timestamptz NOT NULL DEFAULT now()
    );

    -- 3. The roles 001_initial.sql seeds, and `system` on a legacy-backed
    -- database. A role that is absent gets no row.
    INSERT INTO elitea_auth.admin_role_seeds (role_id, name, mode, marker, seeded_by)
    SELECT role.id, role.name, role.mode, seed.marker, '0262_builtin_admin_roles.sql'
    FROM public.auth_core__role AS role
    JOIN (VALUES
        ('super_admin', 'declared'),
        ('admin',       'declared'),
        ('editor',      'as_found'),
        ('viewer',      'as_found'),
        ('system',      'unassignable')
    ) AS seed(name, marker) ON seed.name = role.name
    WHERE role.mode = 'administration'
    ON CONFLICT (role_id) DO NOTHING;

    -- 4. The new built-in roles.
    FOR built_in IN
        SELECT * FROM (VALUES ('llm_manager'), ('project_administrator'),
                              ('emergency_access'), ('security_auditor')) AS r(name)
    LOOP
        SELECT id INTO found_role FROM public.auth_core__role
        WHERE mode = 'administration' AND name = built_in.name;

        IF found_role IS NULL THEN
            INSERT INTO public.auth_core__role (name, mode)
            VALUES (built_in.name, 'administration')
            RETURNING id INTO found_role;
            INSERT INTO elitea_auth.admin_role_seeds (role_id, name, mode, marker, seeded_by)
            VALUES (found_role, built_in.name, 'administration', 'declared',
                    '0262_builtin_admin_roles.sql');
        ELSE
            SELECT EXISTS (SELECT 1 FROM elitea_auth.admin_role_seeds WHERE role_id = found_role)
            INTO has_seed;
            IF NOT has_seed THEN
                RAISE NOTICE '0262: the administration role % already exists and is not built-in; it is left unchanged, gets no grant and is not locked', built_in.name;
                CONTINUE;
            END IF;
        END IF;

        INSERT INTO public.auth_core__role_permission (role_id, permission)
        SELECT found_role, grant_row.permission
        FROM (VALUES
            ('llm_manager', 'llm.catalogue.view'),
            ('llm_manager', 'llm.catalogue.manage'),
            ('llm_manager', 'llm.policy.view'),
            ('llm_manager', 'llm.policy.manage'),
            ('llm_manager', 'llm.usage.view'),
            ('llm_manager', 'llm.alerts.manage'),
            ('llm_manager', 'models.admin.project_budgets.view'),
            ('llm_manager', 'models.admin.project_budgets.edit'),
            ('llm_manager', 'projects.projects.projects.view'),

            ('project_administrator', 'projects.projects.projects.view'),
            ('project_administrator', 'projects.projects.projects.edit'),
            ('project_administrator', 'projects.projects.project.create'),
            ('project_administrator', 'projects.projects.project.delete'),
            ('project_administrator', 'projects.members.manage'),
            ('project_administrator', 'projects.settings.view'),
            ('project_administrator', 'projects.settings.manage'),
            ('project_administrator', 'configuration.users.users.view'),
            ('project_administrator', 'configuration.roles.roles.view'),
            ('project_administrator', 'configuration.roles.user_project_permissions.view'),
            ('project_administrator', 'invites.bulkusers'),
            ('project_administrator', 'invites.bulkprojects'),
            ('project_administrator', 'admin.moderation'),
            ('project_administrator', 'admin.moderation.edit'),
            ('project_administrator', 'models.admin.project_budgets.view'),

            ('emergency_access', 'access.breakglass.request'),

            ('security_auditor', 'models.admin.audit_trail.view'),
            ('security_auditor', 'access.roles.view'),
            ('security_auditor', 'access.breakglass.review')
        ) AS grant_row(role_name, permission)
        WHERE grant_row.role_name = built_in.name
        ON CONFLICT (role_id, permission) DO NOTHING;
    END LOOP;
END
$$;
