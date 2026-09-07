-- 0115_evaluation_dataset_run_permissions.sql — the six default-mode grants
-- Agent Evaluation slice 2 needs.
--
--   models.applications.evaluation.dataset.read
--   models.applications.evaluation.dataset.create
--   models.applications.evaluation.dataset.update
--   models.applications.evaluation.dataset.delete
--   models.applications.evaluation.run.read
--   models.applications.evaluation.run.create
--
-- WHY A NEW FILE. Migrations are checksum-immutable, so 0104 — the dimension
-- library's grant, the RBAC half of slice 1 — cannot be extended. 0060 is
-- doubly unavailable: it is applied everywhere AND it early-returns on any
-- database that already has an administration-mode role, which is every
-- database that has ever been configured. A grant appended there would reach
-- only never-configured installs, which is the exact opposite of the set that
-- needs it. 0104's header carries the full argument; nothing about it has
-- changed.
--
-- WHERE THE NAMES COME FROM, AND WHY THE ROUTES DO NOT USE `projectPermission`.
--
-- Same as 0104, for the same reason. Agent Evaluation is NOT in the pylon
-- plugin corpus this repository carries. That was re-verified for this slice
-- rather than assumed: the whole of `legacy/plugins/*` and
-- `legacy/centry/*/plugins/*` was searched for `eval_dataset`, `eval_run`,
-- `eval_result`, `eval_suite` and `eval_dimension`, and the only hits outside
-- vendored `site-packages/` are three vestiges of a feature that was planned
-- and never built — a placeholder `"eval_task_node"` string in elitea_core's
-- bootstrap topology, a `generate_eval_dimensions` prompt key in the
-- `configurations` plugin, and a comment naming a function
-- `build_eval_dimensions_system_prompt` that does not exist in the file it
-- points at.
--
-- So there is no `check_api` declaration to transcribe and no row in
-- testdata/legacy/legacy-rbac-static-catalog.json. The six strings are the
-- product's own, transcribed verbatim from the reference UI's
-- `src/[fsd]/widgets/evaluation/lib/constants/evaluation.constants.js`
-- EVAL_PERMISSIONS block (`datasetRead`, `datasetCreate`, `datasetUpdate`,
-- `datasetDelete`, `runRead`, `runCreate`), whose own comment names them
-- "RBAC permission strings (must match backend check_api decorators, §19.6)".
-- The routes therefore gate through exported constants in
-- internal/api/v2/evaluation and pass them to `apimw.RequireResolvedPermissions`
-- directly — the identical resolver, mode and middleware `projectPermission`
-- builds, with the pylon-provenance claim dropped because it would be false and
-- router_elitea_core_permission_map_test.go is right to refuse it.
--
-- The check that DOES bind is router_permission_grant_gate_test.go, which
-- resolves those constants through the AST and fails unless a shared migration
-- grants each of them in `default` mode. This file is that grant. Without it
-- every dataset and run route answers 403 to every caller including the
-- operator, which reads as a broken page rather than as a missing grant (0063's
-- header; the shape #354 and #359 shipped).
--
-- WHAT IS NOT GRANTED HERE, AND WHY.
--
--   * `models.applications.evaluation.run.delete`. The reference declares it,
--     and this slice serves no run-delete route. A grant for a route that does
--     not exist is not harmful, but it is not verifiable either: nothing would
--     fail if the string were misspelled. It arrives with the route.
--   * `models.applications.evaluation.suite.*` and `.human_score.*`. Neither
--     surface exists in this slice.
--   * A `run.cancel` string. There is none in the reference vocabulary, and
--     the cancel route is gated on `run.create` instead — the same right that
--     started the run. Inventing a name would either widen the vocabulary past
--     the product's own declaration, or (if granted here) hide the fact that
--     the reference has no separate cancel tier.
--
-- THE ROLE SPLIT.
--
-- 0104's, unchanged: reads to admin, editor AND viewer; writes to admin and
-- editor only. A dataset is authored project content of exactly the kind
-- 0068 treats that way under `models.applications.*`. A viewer that could
-- delete a project's evaluation dataset would be a widening this file has no
-- mandate to make, and a viewer that cannot READ a run sees an empty
-- Evaluation tab with no explanation.
--
-- `run.create` is a WRITE and not a read even though it produces no authored
-- row: starting a run spends model credit once per dataset case, so it is the
-- one permission in this file whose cost is not storage. It goes to admin and
-- editor, never viewer.
--
-- `system` and `super_admin` are omitted, as everywhere else in this corpus.
--
-- 0060's VIRGIN-mode guard is NOT reproduced, for the reason 0061, 0066, 0068,
-- 0079, 0085 and 0104 all give: these permissions have never existed on any
-- deployment of this platform, so no operator can have revoked them, and
-- skipping already-configured deployments would leave exactly those unable to
-- reach the routes.
--
-- THE CENTRAL GRANT ALONE IS NOT ENOUGH — the second block.
--
-- legacyrbac's projectPermissions() reads the central default-mode grants
-- under `WHERE NOT EXISTS (SELECT 1 FROM project_permissions)`
-- (internal/infra/legacyrbac/postgres.go), and the suppression is ALL OR
-- NOTHING per caller: ONE auth_core__project_role_permission row on any
-- project role the caller holds discards the ENTIRE central set for that
-- caller in that project. The product's own admin console creates those rows
-- every time an operator presses "Apply to Projects" or saves a permission
-- matrix. A file that grants centrally and stops there is inert on every
-- project an operator has ever touched. shared/0090 says in its own header
-- that a later migration must carry its own block, and
-- migrations/project_override_reconciliation_test.go is the gate that makes
-- that a build failure rather than a convention.
--
-- The block is UNCONDITIONAL where 0090's is not, for 0104's reason: these six
-- strings have never been granted on any deployment, so no project's saved
-- matrix can have omitted them deliberately. Every omission is an absence.
--
-- Idempotent and additive: it grants to roles that already exist, never
-- creates one, and conflicts are ignored.
DO $$
BEGIN

IF to_regclass('public.auth_core__role') IS NULL
   OR to_regclass('public.auth_core__role_permission') IS NULL THEN
    RAISE NOTICE '0115: auth_core tables absent, nothing to grant';
    RETURN;
END IF;

-- The reads: every project role needs to see datasets and run history.
INSERT INTO public.auth_core__role_permission (role_id, permission)
SELECT role.id, grant_row.permission
FROM public.auth_core__role AS role
CROSS JOIN (VALUES
    ('models.applications.evaluation.dataset.read'),
    ('models.applications.evaluation.run.read')
) AS grant_row(permission)
WHERE role.mode = 'default' AND role.name IN ('admin', 'editor', 'viewer')
ON CONFLICT (role_id, permission) DO NOTHING;

-- The writes: authoring a dataset and its cases, and starting or cancelling a
-- run.
INSERT INTO public.auth_core__role_permission (role_id, permission)
SELECT role.id, grant_row.permission
FROM public.auth_core__role AS role
CROSS JOIN (VALUES
    ('models.applications.evaluation.dataset.create'),
    ('models.applications.evaluation.dataset.update'),
    ('models.applications.evaluation.dataset.delete'),
    ('models.applications.evaluation.run.create')
) AS grant_row(permission)
WHERE role.mode = 'default' AND role.name IN ('admin', 'editor')
ON CONFLICT (role_id, permission) DO NOTHING;

-- The override delivery. Only projects that ALREADY carry per-project rows are
-- touched: a project role with no override rows still falls back to the
-- central matrix above, and handing it a snapshot it never had would freeze it
-- out of every future central grant — the very hole this block exists to close.
IF to_regclass('public.auth_core__project_role') IS NULL
   OR to_regclass('public.auth_core__project_role_permission') IS NULL THEN
    RAISE NOTICE '0115: no per-project permission tables, central grants are the whole story here';
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
    ('models.applications.evaluation.dataset.read', ARRAY['admin', 'editor', 'viewer']),
    ('models.applications.evaluation.dataset.create', ARRAY['admin', 'editor']),
    ('models.applications.evaluation.dataset.update', ARRAY['admin', 'editor']),
    ('models.applications.evaluation.dataset.delete', ARRAY['admin', 'editor']),
    ('models.applications.evaluation.run.read', ARRAY['admin', 'editor', 'viewer']),
    ('models.applications.evaluation.run.create', ARRAY['admin', 'editor'])
) AS grant_row(permission, roles)
WHERE project_role.name = ANY (grant_row.roles)
ON CONFLICT (project_id, role_id, permission) DO NOTHING;

END
$$;
