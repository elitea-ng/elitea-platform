-- The four DEFAULT-mode grants the evaluation dimension library needs.
--
--   models.applications.evaluation.dimension.read
--   models.applications.evaluation.dimension.create
--   models.applications.evaluation.dimension.update
--   models.applications.evaluation.dimension.delete
--
-- WHY A NEW FILE AND NOT AN EDIT TO 0060 OR 0068.
--
-- Migrations are checksum-immutable and every one of them is already applied
-- somewhere, so an edit is never an option. 0060 additionally EARLY-RETURNS on
-- any database that already has an administration-mode role — which is every
-- database that has ever been configured — so a grant appended there would
-- reach only never-configured installs, which is the exact opposite of the set
-- that needs it. 0068 is the /elitea_core seed and is separately pinned:
-- internal/api/router_elitea_core_permission_map_test.go's
-- TestEliteaCoreSeedingMigrationGrantsNothingUngated reads THAT file and
-- requires every string in it to be gated through the `projectPermission`
-- helper, which these four deliberately are not (see below).
--
-- WHY THESE NAMES ARE NOT IN testdata/legacy/legacy-rbac-static-catalog.json,
-- AND WHY THAT IS NOT A DEFECT HERE.
--
-- The catalogue is generated from the legacy runtime `check_api` declarations
-- in the plugin corpus this repository carries, and Agent Evaluation is not in
-- that corpus (`legacy/plugins/elitea_core/api/v2/` has no evaluation module at
-- all). The strings are not invented here either: they are the product's own,
-- transcribed verbatim from the baseline UI's
-- `src/[fsd]/widgets/evaluation/lib/constants/evaluation.constants.js`
-- EVAL_PERMISSIONS block, whose comment names them "RBAC permission strings
-- (must match backend check_api decorators, §19.6)".
--
-- That is precisely why the routes in internal/api/router.go do NOT go through
-- the `projectPermission("...")` helper.
-- TestEliteaCoreGatesNameOnlyRealLegacyPermissions asserts that every literal
-- passed to that helper appears in the catalogue, and the assertion is right:
-- for a PORTED legacy runtime route, a name the catalogue does not know is a
-- typo, and a typo ships as a permanent 403. This is not a ported route. It is
-- a new surface whose permission vocabulary arrives with it, so the evaluation
-- gates name their permissions through exported constants in
-- internal/api/v2/evaluation and pass them to
-- `apimw.RequireResolvedPermissions` directly — the identical resolver, mode
-- and middleware `projectPermission` builds, with the legacy-provenance claim
-- dropped because it would be false.
--
-- The check that DOES still apply is the one that matters:
-- internal/api/router_permission_grant_gate_test.go resolves those constants
-- through the AST and fails unless a shared migration grants each of them in
-- `default` mode. This file is that grant. Without it the whole evaluation tab
-- answers 403 to every caller including the operator, which reads as a broken
-- page rather than as a missing grant (0063's header, the shape #354 and #359
-- shipped).
--
-- THE ROLE SPLIT.
--
-- The legacy matrix has no row for these strings, so the split is taken from
-- the closest thing that does: 0068's treatment of every other piece of
-- authored project content under `models.applications.*` — reads to admin,
-- editor AND viewer; writes to admin and editor only. A dimension is authored
-- content of exactly that kind. A viewer that could delete a project's scoring
-- rubric would be a widening this file has no mandate to make, and a viewer
-- that cannot READ the library sees an empty Evaluation tab with no
-- explanation.
--
-- `system` and `super_admin` are omitted, as everywhere else in this corpus.
--
-- 0060's VIRGIN-mode guard is NOT reproduced, for the reason 0061, 0066, 0068,
-- 0079 and 0085 all give: these permissions have never existed on any
-- deployment of this platform, so no operator can have revoked them, and
-- skipping already-configured deployments would leave exactly those unable to
-- reach the routes.
--
-- THE CENTRAL GRANT ALONE IS NOT ENOUGH, AND THAT IS WHAT THE SECOND BLOCK IS.
--
-- legacyrbac's projectPermissions() reads the central default-mode grants under
-- `WHERE NOT EXISTS (SELECT 1 FROM project_permissions)`
-- (internal/infra/legacyrbac/postgres.go), and the suppression is ALL OR
-- NOTHING per caller: ONE auth_core__project_role_permission row on any project
-- role the caller holds discards the ENTIRE central set for that caller in that
-- project. The product's own admin console creates those rows every time an
-- operator presses "Apply to Projects" or saves a permission matrix.
--
-- So a file that grants centrally and stops there is inert on every project an
-- operator has ever touched, and the Evaluation tab answers 403 there with
-- nothing naming the cause. shared/0090 backfilled the corpus as of 0089 and
-- says in its own header that a later migration must carry its own block;
-- migrations/project_override_reconciliation_test.go is the gate that makes
-- that a build failure rather than a convention.
--
-- WHY THIS BLOCK IS UNCONDITIONAL WHERE 0090's IS NOT. 0090 refuses to deliver
-- a (role name, permission) pair that any project already states for itself,
-- because an operator saving a narrowed matrix is indistinguishable from an
-- absence, and a blanket copy would silently reverse those decisions. That
-- ambiguity cannot exist here: these four strings have never been granted on
-- any deployment of this platform, so no project's saved matrix can have
-- omitted them deliberately. Every omission is an absence. Delivering them is
-- therefore the same additive act as the central INSERT above, and the role
-- split is identical — the project role's NAME decides, so a project `viewer`
-- gets the read and not the writes.
--
-- Idempotent and additive: it grants to roles that already exist, never creates
-- one, and conflicts are ignored.
DO $$
BEGIN

IF to_regclass('public.auth_core__role') IS NULL
   OR to_regclass('public.auth_core__role_permission') IS NULL THEN
    RAISE NOTICE '0100: auth_core tables absent, nothing to grant';
    RETURN;
END IF;

-- The read: the library listing every project role needs to see the tab.
INSERT INTO public.auth_core__role_permission (role_id, permission)
SELECT role.id, 'models.applications.evaluation.dimension.read'
FROM public.auth_core__role AS role
WHERE role.mode = 'default' AND role.name IN ('admin', 'editor', 'viewer')
ON CONFLICT (role_id, permission) DO NOTHING;

-- The writes: authoring, editing and removing a dimension.
INSERT INTO public.auth_core__role_permission (role_id, permission)
SELECT role.id, grant_row.permission
FROM public.auth_core__role AS role
CROSS JOIN (VALUES
    ('models.applications.evaluation.dimension.create'),
    ('models.applications.evaluation.dimension.update'),
    ('models.applications.evaluation.dimension.delete')
) AS grant_row(permission)
WHERE role.mode = 'default' AND role.name IN ('admin', 'editor')
ON CONFLICT (role_id, permission) DO NOTHING;

-- The override delivery. Only projects that ALREADY carry per-project rows are
-- touched: a project role with no override rows still falls back to the central
-- matrix above, and handing it a snapshot it never had would freeze it out of
-- every future central grant — the very hole this block exists to close.
IF to_regclass('public.auth_core__project_role') IS NULL
   OR to_regclass('public.auth_core__project_role_permission') IS NULL THEN
    RAISE NOTICE '0100: no per-project permission tables, central grants are the whole story here';
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
    ('models.applications.evaluation.dimension.read', ARRAY['admin', 'editor', 'viewer']),
    ('models.applications.evaluation.dimension.create', ARRAY['admin', 'editor']),
    ('models.applications.evaluation.dimension.update', ARRAY['admin', 'editor']),
    ('models.applications.evaluation.dimension.delete', ARRAY['admin', 'editor'])
) AS grant_row(permission, roles)
WHERE project_role.name = ANY (grant_row.roles)
ON CONFLICT (project_id, role_id, permission) DO NOTHING;

END
$$;
