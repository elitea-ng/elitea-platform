-- 0102_skill_icon_permissions.sql — the DEFAULT-mode grants for the skill icon
-- route family (GET/POST/PUT/DELETE
-- /api/v2/elitea_core/upload_skill_icon/prompt_lib/{projectID}).
--
-- WHY A NEW FILE. Migrations here are checksum-immutable: 0068 already ran on
-- every deployment, so the four strings below cannot be appended to it. They
-- are also not optional. The routes are gated on these permissions, and 0063's
-- header states the consequence of gating a route on a permission nothing
-- grants: "403-for-everyone, which reads as a broken page rather than as a
-- missing grant". Without this file the skill icon picker would open and every
-- request inside it would refuse.
--
-- WHERE THE SPLIT COMES FROM. testdata/postgres/legacy-rbac-matrix.json, the
-- `mode = 'default'` rows: `admin` and `editor` carry all four
-- `models.applications.skills.upload_icon.*` strings and `viewer` carries none
-- — the same shape 0068 records for the AGENT icon family
-- (`models.applications.upload_icon.*`), where the header already notes that a
-- viewer cannot even LIST uploaded icons. The names themselves come from
-- testdata/legacy/legacy-rbac-static-catalog.json's `check_api` declarations
-- for elitea_core/api/v2/upload_skill_icon.py, and
-- internal/api/router_elitea_core_permission_map_test.go re-checks both
-- directions, so a gate this file does not grant fails the build.
--
-- BLAST RADIUS, AND WHY THE CENTRAL GRANT IS NOT ENOUGH ON ITS OWN.
-- legacyrbac's projectPermissions() falls back to the CENTRAL default-mode
-- grants by role name only for a project carrying NO per-project
-- role_permission rows, which is the fresh-Go-database shape alone. A
-- legacy-backed database, a legacy dump and the E2E stack all seed per-project
-- rows and suppress the fallback.
--
-- That suppression is the reason nothing here can harm an existing deployment,
-- and it is equally the reason a central-only grant never REACHES one: the skill
-- icon picker would open on every such project and refuse all four requests with
-- 403. shared/0090 backfilled the corpus as of 0089 and states in its own header
-- that a later migration must carry its own override block;
-- migrations/project_override_reconciliation_test.go makes that a build failure
-- rather than a convention. The second statement below is that block.
--
-- It is unconditional where 0090's is not. 0090 refuses to deliver a
-- (role name, permission) pair that any project already states for itself,
-- because an operator saving a narrowed matrix cannot be told apart from an
-- absence. That ambiguity cannot arise here: these four strings have never been
-- granted on any deployment of this platform, so no saved matrix can have
-- omitted them deliberately. Every omission is an absence.
--
-- Idempotent and additive: it grants to roles that already exist, never creates
-- one, and conflicts are ignored. `system` and `super_admin` are omitted, as
-- everywhere else in this corpus. 0060's VIRGIN-mode guard is not reproduced,
-- for the reason 0061/0062/0063/0066/0068 all give — these permissions have
-- never existed on a Go deployment, so no operator can have revoked them.
DO $$
BEGIN

IF to_regclass('public.auth_core__role') IS NULL
   OR to_regclass('public.auth_core__role_permission') IS NULL THEN
    RAISE NOTICE '0102: auth_core tables absent, nothing to grant';
    RETURN;
END IF;

INSERT INTO public.auth_core__role_permission (role_id, permission)
SELECT role.id, grant_row.permission
FROM public.auth_core__role AS role
CROSS JOIN (VALUES
    ('models.applications.skills.upload_icon.get'),
    ('models.applications.skills.upload_icon.post'),
    ('models.applications.skills.upload_icon.update'),
    ('models.applications.skills.upload_icon.delete')
) AS grant_row(permission)
WHERE role.mode = 'default' AND role.name IN ('admin', 'editor')
ON CONFLICT (role_id, permission) DO NOTHING;

-- The override delivery. Only projects that ALREADY carry per-project rows are
-- touched: a project role with no override rows still falls back to the central
-- matrix above, and handing it a snapshot it never had would freeze it out of
-- every future central grant — the very hole this block exists to close.
IF to_regclass('public.auth_core__project_role') IS NULL
   OR to_regclass('public.auth_core__project_role_permission') IS NULL THEN
    RAISE NOTICE '0102: no per-project permission tables, central grants are the whole story here';
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
    ('models.applications.skills.upload_icon.get'),
    ('models.applications.skills.upload_icon.post'),
    ('models.applications.skills.upload_icon.update'),
    ('models.applications.skills.upload_icon.delete')
) AS grant_row(permission)
-- The same split the central grant uses: the project role's NAME decides, so a
-- project `viewer` gains nothing here, exactly as it gains nothing centrally.
WHERE project_role.name IN ('admin', 'editor')
ON CONFLICT (project_id, role_id, permission) DO NOTHING;

END
$$;
