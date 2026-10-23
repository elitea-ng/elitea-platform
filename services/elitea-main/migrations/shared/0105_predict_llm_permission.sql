-- 0105_predict_llm_permission.sql — the one DEFAULT-mode grant behind
-- POST /api/v2/elitea_core/predict_llm/prompt_lib/{projectID} (#194).
--
-- WHY A NEW FILE. Migrations here are checksum-immutable: 0068 and 0072 have
-- run on every deployment, so this string cannot be appended to either. It is
-- also not optional. router.go gates the route on it, and 0063's header
-- records the consequence of gating a route on a permission nothing grants:
-- "403-for-everyone, which reads as a broken page rather than as a missing
-- grant". Nothing in this corpus granted `models.applications.predict.post`
-- before this file, so without it the route would ship registered and refuse
-- every caller — a second, quieter shape of the #126 defect it is meant to
-- close.
--
-- WHERE THE SPLIT COMES FROM, AND WHY VIEWER IS IN IT.
-- testdata/postgres/legacy-rbac-matrix.json's `mode = 'default'` rows carry
-- `models.applications.predict.post` on admin, editor AND viewer; the
-- `administration` rows carry it on admin and editor only. That is not an
-- oversight in the dump — it is what legacy's own handler declares:
-- elitea_core/api/v2/predict_llm.py's check_api sets recommended_roles to
-- {DEFAULT_MODE: viewer True, ADMINISTRATION_MODE: viewer False}. A viewer may
-- send a stateless message to a model; a viewer may not administer one.
--
-- This file grants the DEFAULT mode only, because the route is registered in
-- the default-mode `prompt_lib` group alone. The administration-mode split is
-- recorded here rather than delivered: there is no administration-mode
-- predict_llm route to gate.
--
-- BLAST RADIUS, AND WHY THE CENTRAL GRANT IS NOT ENOUGH ON ITS OWN.
-- legacyrbac's projectPermissions() falls back to the CENTRAL default-mode
-- grants by role name ONLY for a project carrying no per-project
-- role_permission rows — the fresh-Go-database shape alone. A legacy-backed
-- database, a legacy dump and the E2E stack all seed per-project rows and
-- suppress that fallback, so a central-only grant would be inert on every
-- project an operator has ever touched. shared/0090 backfilled the corpus as of
-- 0089 and states in its own header that a later migration must carry its own
-- override block; migrations/project_override_reconciliation_test.go makes that
-- a build failure rather than a convention. The second statement below is that
-- block.
--
-- It is unconditional where 0090's is not. 0090 refuses to deliver a
-- (role name, permission) pair that any project already states for itself,
-- because an operator saving a narrowed matrix cannot be told apart from an
-- absence. That ambiguity cannot arise here: this string has never been granted
-- on any deployment of this platform, so no saved matrix can have omitted it
-- deliberately. Every omission is an absence.
--
-- Idempotent and additive: it grants to roles that already exist, never creates
-- one, and conflicts are ignored. `system` and `super_admin` are omitted, as
-- everywhere else in this corpus. 0060's VIRGIN-mode guard is not reproduced,
-- for the reason 0061/0062/0063/0066/0068/0102 all give — this permission has
-- never existed on a Go deployment, so no operator can have revoked it.
DO $$
BEGIN

IF to_regclass('public.auth_core__role') IS NULL
   OR to_regclass('public.auth_core__role_permission') IS NULL THEN
    RAISE NOTICE '0105: auth_core tables absent, nothing to grant';
    RETURN;
END IF;

INSERT INTO public.auth_core__role_permission (role_id, permission)
SELECT role.id, 'models.applications.predict.post'
FROM public.auth_core__role AS role
WHERE role.mode = 'default' AND role.name IN ('admin', 'editor', 'viewer')
ON CONFLICT (role_id, permission) DO NOTHING;

-- The override delivery. Only projects that ALREADY carry per-project rows are
-- touched: a project role with no override rows still falls back to the central
-- matrix above, and handing it a snapshot it never had would freeze it out of
-- every future central grant — the very hole this block exists to close.
IF to_regclass('public.auth_core__project_role') IS NULL
   OR to_regclass('public.auth_core__project_role_permission') IS NULL THEN
    RAISE NOTICE '0105: no per-project permission tables, central grants are the whole story here';
    RETURN;
END IF;

INSERT INTO public.auth_core__project_role_permission (project_id, role_id, permission)
SELECT DISTINCT overridden.project_id, overridden.role_id, 'models.applications.predict.post'
FROM (
    SELECT DISTINCT project_id, role_id
    FROM public.auth_core__project_role_permission
    WHERE role_id IS NOT NULL
) AS overridden
JOIN public.auth_core__project_role AS project_role
  ON project_role.id = overridden.role_id
 AND project_role.project_id = overridden.project_id
-- The same split the central grant uses: the project role's NAME decides, and
-- it includes `viewer` here for the reason the header gives.
WHERE project_role.name IN ('admin', 'editor', 'viewer')
ON CONFLICT (project_id, role_id, permission) DO NOTHING;

END
$$;
