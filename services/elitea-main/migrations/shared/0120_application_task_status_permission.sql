-- 0120_application_task_status_permission.sql — the ONE default-mode grant the
-- restored `application_task` GET needs.
--
--   models.applications.task.get
--
-- WHY IT IS MISSING. 0068 transcribed the legacy default-mode matrix for the
-- /elitea_core routes that existed at the time. `application_task` had been
-- deleted by #126 and served nothing, so its READ permission had no gate to
-- reach and was left out; its sibling `models.applications.task.delete` went in
-- because the index-cancel route already gated on it. #254 P2 restores the
-- route, so the read string now gates something and has to be granted.
--
-- WHY A NEW FILE. Migrations are checksum-immutable, so 0068 cannot be
-- extended. 0060 is doubly unavailable: it is applied everywhere AND it
-- early-returns on any database that already carries an administration-mode
-- role, which is every database an operator has ever configured. A grant
-- appended there would reach only never-configured installs — the exact
-- opposite of the set that needs it.
--
-- WHERE THE ROLE SPLIT COMES FROM. testdata/postgres/legacy-rbac-matrix.json,
-- the export of a real legacy database, at `mode = 'default'`. It grants
-- `models.applications.task.get` to admin, editor AND viewer — the same three
-- roles it grants `models.applications.task.delete` to, and the same three
-- application_task.py's own `recommended_roles` names for the GET. This is
-- parity restoration, not a new policy. A viewer that could not poll a run it
-- is allowed to watch would see a chat that never settles, with no
-- explanation; that reads as a broken page rather than as a missing grant
-- (0063's header).
--
-- `system` and `super_admin` are omitted, as everywhere else in this corpus.
--
-- 0060's VIRGIN-mode guard is NOT reproduced, for the reason 0061, 0066, 0068,
-- 0079, 0085, 0104 and 0116 all give: this permission has never existed on any
-- Go deployment, so no operator can have revoked it, and skipping configured
-- deployments would leave exactly those unable to reach the route.
--
-- THE CENTRAL GRANT ALONE IS NOT ENOUGH — the second block. legacyrbac's
-- projectPermissions() reads the central default-mode grants only
-- `WHERE NOT EXISTS (SELECT 1 FROM project_permissions)`
-- (internal/infra/legacyrbac/postgres.go), and the suppression is ALL OR
-- NOTHING per caller: one auth_core__project_role_permission row on any
-- project role the caller holds discards the ENTIRE central set for that
-- caller in that project. The admin console writes those rows every time an
-- operator saves a permission matrix. shared/0090 says in its own header that
-- a later migration must carry its own block, and
-- migrations/project_override_reconciliation_test.go makes that a build
-- failure rather than a convention.
--
-- The block is UNCONDITIONAL: the string has never been granted on any Go
-- deployment, so no project's saved matrix can have omitted it deliberately.
-- Every omission is an absence.
--
-- WRITTEN AS 0120 AND RENUMBERED AT MERGE IF NEEDED. 0120 was free when this
-- file was authored. Concurrent packages claim numbers the same way; only the
-- merge can see a collision, and the number belongs to whichever lands first.
--
-- Idempotent and additive: it grants to roles that already exist, never creates
-- one, and conflicts are ignored.
DO $$
BEGIN

IF to_regclass('public.auth_core__role') IS NULL
   OR to_regclass('public.auth_core__role_permission') IS NULL THEN
    RAISE NOTICE '0120: auth_core tables absent, nothing to grant';
    RETURN;
END IF;

INSERT INTO public.auth_core__role_permission (role_id, permission)
SELECT role.id, grant_row.permission
FROM public.auth_core__role AS role
CROSS JOIN (VALUES
    ('models.applications.task.get')
) AS grant_row(permission)
WHERE role.mode = 'default' AND role.name IN ('admin', 'editor', 'viewer')
ON CONFLICT (role_id, permission) DO NOTHING;

-- The override delivery. Only projects that ALREADY carry per-project rows are
-- touched: a project role with no override rows still falls back to the central
-- matrix above, and handing it a snapshot it never had would freeze it out of
-- every future central grant — the hole this block exists to close.
IF to_regclass('public.auth_core__project_role') IS NULL
   OR to_regclass('public.auth_core__project_role_permission') IS NULL THEN
    RAISE NOTICE '0120: no per-project permission tables, central grants are the whole story here';
    RETURN;
END IF;

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
    ('models.applications.task.get', ARRAY['admin', 'editor', 'viewer'])
) AS grant_row(permission, roles)
WHERE project_role.name = ANY (grant_row.roles)
ON CONFLICT (project_id, role_id, permission) DO NOTHING;

END
$$;
