-- 0136_project_settings_permission_and_dead_permissions.sql — one new
-- default-mode grant, and the removal of permission strings that no code checks.
--
--   GRANTED:  models.project_settings.edit  (default mode, admin only)
--   REMOVED:  the strings in the dead_permission list below
--
-- PART 1. THE NEW GRANT (#6789).
--
-- The project settings writes — the project name, description and icon
-- (`PUT /elitea_core/project_info/...`, `POST`/`DELETE /elitea_core/project_icon/...`)
-- — gated on `models.project_context.edit`. 0068 gives that string to admin AND
-- editor, so an editor could rename a team project. The settings are the
-- project admin's. router.go now gates these three writes on
-- `models.project_settings.edit`, and this file grants it to `admin` only.
-- The project CONTEXT routes keep `models.project_context.edit`.
--
-- A personal project has no admin. Its owner is its only member, and pylon made
-- that owner an `editor`. The router admits `models.project_context.edit` in the
-- caller's OWN personal project, so no grant to `editor` is necessary here.
--
-- `system` and `super_admin` are omitted, as everywhere else in this corpus.
--
-- WHY A NEW FILE. Migrations are checksum-immutable, so 0068 cannot be
-- extended. 0060 is doubly unavailable: it is applied everywhere AND it
-- early-returns on any database that already carries an administration-mode
-- role, which is every database an operator has ever configured.
--
-- 0060's VIRGIN-mode guard is NOT reproduced, for the reason 0120 gives: this
-- permission has never existed on any deployment, so no operator can have
-- revoked it.
--
-- THE CENTRAL GRANT ALONE IS NOT ENOUGH. legacyrbac's projectPermissions()
-- discards the central default-mode set for a caller as soon as one
-- auth_core__project_role_permission row exists on a role they hold. So the
-- second block copies the grant into every project that already carries
-- override rows, for its `admin` role. The block is unconditional for the
-- reason 0120 gives: no saved matrix can have omitted a string that did not
-- exist.
--
-- PART 2. THE DEAD STRINGS (#6874).
--
-- A permission row is real only when some code checks that EXACT string. The
-- resolver matches exact strings; it does no prefix matching. So a bare
-- `projects` row grants nothing to `projects.projects.*`. The strings below
-- are checked by no Go route, no elitea-web gate and no worker:
--
--   * bare section nodes that pylon's admin module still registered:
--     projects, configuration, runtime, modes, migration, invites,
--     invites.platform;
--
-- PYLON STILL CHECKS THE BARE SECTION NODES. legacy/plugins/admin/module.py
-- registers its administration sections with exactly these permissions
-- (["projects"], ["configuration"], ["modes"], ["runtime"], ["migration"],
-- ["invites"], ["invites.platform"]). Where pylon's own admin UI still runs
-- against this database, those sections disappear for every central admin
-- after this file runs, until pylon re-seeds its recommended roles on its
-- next restart. The rows then come back, and only the admin Roles catalogue
-- (retiredPermissions) hides them. The Go admin console checks none of them.
--   * prefix nodes that the legacy admin_ui Roles matrix saved as rows when an
--     operator clicked a group toggle: admin, configurations;
--   * models.chat.conversations.list_custom, which no role holds without
--     `.list`;
--   * the five Collections strings that pylon's auth_core migration
--     202602261000 seeds and nothing checks.
--
-- internal/api/router_permission_retired_gate_test.go keeps this list honest in
-- both directions: no gate may check a string listed here, and the admin
-- Roles catalogue (internal/api/v2/admin/roles.go, retiredPermissions) hides
-- exactly this list.
--
-- THE ALL-OR-NOTHING OVERRIDE RULE, AND WHY ONE DELETE IS CONDITIONAL.
--
-- Deleting a per-project override row can do more than remove a string. When
-- the deleted rows are the ONLY override rows of a project role, that role
-- stops having an override and falls back to the full central default-mode
-- set. A role whose override held only dead strings granted nothing; after an
-- unconditional delete it would grant everything its central role grants. So a
-- per-project row is deleted only when its (project, role) pair keeps at least
-- one live row. A pair that holds only dead strings keeps them, and keeps its
-- current (empty) effect.
--
-- The central rows and the user rows carry no such rule: the resolver reads
-- them as plain sets.
--
-- STATEMENT ORDER. The removals run first and the grants last, and the central
-- INSERT is the final statement. The source gates in internal/api read a grant
-- block from `INSERT INTO public.auth_core__role_permission` to the next such
-- INSERT or to the end of the file, so any literal after the central INSERT
-- would read as a grant. The dead list is one array, declared once, for the
-- same reason.
--
-- WRITTEN AS 0136 AND RENUMBERED AT MERGE IF NEEDED. 0136 was free when this
-- file was authored. Concurrent packages claim numbers the same way; only the
-- merge can see a collision, and the number belongs to whichever lands first.
--
-- Idempotent: a second run grants nothing new and finds nothing to delete.
DO $$
DECLARE
    dead_permission text[] := ARRAY[
        'projects',
        'configuration',
        'runtime',
        'modes',
        'migration',
        'invites',
        'invites.platform',
        'admin',
        'configurations',
        'models.chat.conversations.list_custom',
        'models.promptlib_shared.collection.details',
        'models.promptlib_shared.collections.list',
        'models.promptlib_shared.public_collection.details',
        'models.promptlib_shared.approve_collection.post',
        'models.promptlib_shared.reject_collection.delete'
    ];
    has_project_tables boolean;
BEGIN

IF to_regclass('public.auth_core__role') IS NULL
   OR to_regclass('public.auth_core__role_permission') IS NULL THEN
    RAISE NOTICE '0136: auth_core tables absent, nothing to grant or remove';
    RETURN;
END IF;

has_project_tables := to_regclass('public.auth_core__project_role') IS NOT NULL
    AND to_regclass('public.auth_core__project_role_permission') IS NOT NULL;

/* ── part 2: the dead strings ─────────────────────────────────────────── */

DELETE FROM public.auth_core__role_permission
WHERE permission = ANY (dead_permission);

IF to_regclass('public.auth_core__user_permission') IS NOT NULL THEN
    DELETE FROM public.auth_core__user_permission
    WHERE permission = ANY (dead_permission);
END IF;

-- A per-project row goes only where its (project, role) pair keeps a live row.
IF has_project_tables THEN
    DELETE FROM public.auth_core__project_role_permission AS dead
    WHERE dead.permission = ANY (dead_permission)
      AND EXISTS (
          SELECT 1
          FROM public.auth_core__project_role_permission AS live
          WHERE live.project_id = dead.project_id
            AND live.role_id IS NOT DISTINCT FROM dead.role_id
            AND NOT (live.permission = ANY (dead_permission))
      );
END IF;

/* ── part 1: the new grant ────────────────────────────────────────────── */

-- The override delivery. Only projects that ALREADY carry per-project rows are
-- touched: a project role with no override rows still falls back to the
-- central grant below.
IF has_project_tables THEN
    INSERT INTO public.auth_core__project_role_permission (project_id, role_id, permission)
    SELECT DISTINCT overridden.project_id, overridden.role_id, grant_row.permission
    FROM (
        SELECT DISTINCT project_id, role_id
        FROM public.auth_core__project_role_permission
        WHERE role_id IS NOT NULL
    ) AS overridden
    JOIN public.auth_core__project_role AS project_role
      ON project_role.id = overridden.role_id
     AND project_role.project_id = overridden.project_id
    CROSS JOIN (VALUES
        ('models.project_settings.edit', ARRAY['admin'])
    ) AS grant_row(permission, roles)
    WHERE project_role.name = ANY (grant_row.roles)
    ON CONFLICT (project_id, role_id, permission) DO NOTHING;
ELSE
    RAISE NOTICE '0136: no per-project permission tables, the central grant is the whole story here';
END IF;

INSERT INTO public.auth_core__role_permission (role_id, permission)
SELECT role.id, grant_row.permission
FROM public.auth_core__role AS role
CROSS JOIN (VALUES
    ('models.project_settings.edit')
) AS grant_row(permission)
WHERE role.mode = 'default' AND role.name IN ('admin')
ON CONFLICT (role_id, permission) DO NOTHING;

END
$$;
