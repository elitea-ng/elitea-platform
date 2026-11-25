-- 0273_admin_console_areas_default.sql — the default of the console's
-- six-area navigation switch (ADR-0059 decision 6, story R3.2, issue 2532).
--
-- `admin_console.admin_console_areas` in centry.platform_config switches the
-- administration console from its two unlabelled navigation groups to the six
-- labelled areas. Its code default is ON (internal/api/v2/adminaccess,
-- PlatformFlags): a new install gets the areas.
--
-- An UPGRADE keeps the old navigation for one release. "Upgrade" is a recorded
-- fact, not a version: the database already holds an assignment of an
-- administration-mode role. On a new install there is none when this file
-- runs, because 001_initial.sql's bootstrap assignments are revoked by shared
-- 0111 before it and the first administrator is assigned after the corpus.
-- So this file writes the switch OFF exactly when such an assignment exists.
--
-- ON CONFLICT DO NOTHING: a value an operator already stored (on or off) is
-- theirs and stays. Applying the file twice writes nothing the second time.
-- Story R4.9 (issue 2623) removes the switch and the old navigation.
--
-- GUARD. The shared corpus also runs on databases without the auth_core tables
-- or centry.platform_config; there the file raises a notice and returns. No
-- BEGIN/COMMIT: the ledgered runner executes each file inside one transaction
-- with its ledger row (migrate/runner.go apply).

DO $$
BEGIN
    IF to_regclass('public.auth_core__role') IS NULL
       OR to_regclass('public.auth_core__user_role') IS NULL
       OR to_regclass('centry.platform_config') IS NULL THEN
        RAISE NOTICE '0273: auth_core or platform_config tables absent, no console default to record';
        RETURN;
    END IF;

    IF EXISTS (
        SELECT 1
        FROM public.auth_core__user_role AS assignment
        JOIN public.auth_core__role AS role ON role.id = assignment.role_id
        WHERE role.mode = 'administration'
    ) THEN
        INSERT INTO centry.platform_config (section, key, value, updated_at, updated_by)
        VALUES ('admin_console', 'admin_console_areas', 'false'::jsonb, now() AT TIME ZONE 'UTC',
                'migration 0273')
        ON CONFLICT (section, key) DO NOTHING;
    END IF;
END
$$;
