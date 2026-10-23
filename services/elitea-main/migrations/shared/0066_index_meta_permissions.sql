-- 0066_index_meta_permissions.sql — the DEFAULT-mode grants issue 93's index
-- plane is gated on.
--
--   models.applications.index_meta.details ← internal/api/v2/indexing/index_meta.go:18
--   models.applications.index_meta.edit    ← internal/api/v2/indexing/source_contracts.go:21,26
--   models.applications.index_meta.delete  ← internal/api/v2/indexing/source_contracts.go:16
--
-- `.details` is the LIST permission for the indexes rail and is a DIFFERENT
-- string from `.edit`; that difference is the whole defect. Measured against a
-- live stack: a project whose grant list carries `.edit` but not `.details`
-- answers 403 to `GET /api/v2/elitea_core/index_meta/prompt_lib/{project}/{toolkit}`
-- while the Indexes tab still renders (tab visibility comes from the toolkit
-- type schema, not from permissions), so the refusal is invisible.
--
-- WHY THIS IS NOT MERELY A TEST-SEED GAP, which is the question that decided
-- the layer this file sits at. On a Go-bootstrapped database 001_initial.sql
-- seeds default-mode ROLES and no default-mode role_permission row, and
-- legacyrbac's projectPermissions() falls back to the CENTRAL default-mode
-- grants by role name for any project carrying no per-project rows. Nothing in
-- shared/ granted any member of this family, so on such a database the index
-- plane is 403-for-everyone. A real legacy runtime deployment is NOT in that
-- position: testdata/postgres/legacy-rbac-matrix.json, exported from a real
-- legacy database, has all three strings on default-mode admin and editor, and
-- `.details` alone on default-mode viewer. This migration transcribes exactly
-- that per-role split, so a Go-bootstrapped database resolves what a legacy one
-- already did — it is parity restoration, not a new policy.
--
-- BLAST RADIUS. The central fallback applies only to projects with NO
-- per-project role_permission rows, which is the fresh-Go-database shape and
-- never the shape of a legacy-backed database or a legacy dump — so no existing
-- deployment's members gain anything (pinned in
-- internal/infra/legacyrbac/index_meta_grant_postgres_integration_test.go).
-- Members' permission sets are already non-empty (0062), so this changes no
-- set-emptiness transition of the kind issue 276 tracks. The reachable surface
-- changes by exactly the index_meta read, the two schedule writes and the
-- source-only delete. The index-START route is gated on
-- `models.applications.tool.patch` and the cancel route on
-- `models.applications.task.delete`; neither is granted here.
--
-- Idempotent and additive: it grants to roles that already exist, never creates
-- one, and conflicts are ignored. `system` is omitted, as everywhere else in
-- this corpus, and 0060's VIRGIN-mode guard is not reproduced for the same
-- reason 0061/0062/0063 give — these permissions have never existed on a Go
-- deployment, so no operator can have revoked them.
DO $$
BEGIN

IF to_regclass('public.auth_core__role') IS NULL
   OR to_regclass('public.auth_core__role_permission') IS NULL THEN
    RAISE NOTICE '0066: auth_core tables absent, nothing to grant';
    RETURN;
END IF;

-- Read for every project role; the two writes for admin and editor only. The
-- split is the legacy matrix's, not an invention: a viewer that could edit an
-- index schedule would be a widening this file has no mandate to make.
INSERT INTO public.auth_core__role_permission (role_id, permission)
SELECT role.id, 'models.applications.index_meta.details'
FROM public.auth_core__role AS role
WHERE role.mode = 'default' AND role.name IN ('admin', 'editor', 'viewer')
ON CONFLICT (role_id, permission) DO NOTHING;

INSERT INTO public.auth_core__role_permission (role_id, permission)
SELECT role.id, grant_row.permission
FROM public.auth_core__role AS role
CROSS JOIN (VALUES
    ('models.applications.index_meta.edit'),
    ('models.applications.index_meta.delete')
) AS grant_row(permission)
WHERE role.mode = 'default' AND role.name IN ('admin', 'editor')
ON CONFLICT (role_id, permission) DO NOTHING;

END
$$;
