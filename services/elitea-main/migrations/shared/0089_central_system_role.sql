-- 0089_central_system_role.sql — the central DEFAULT-mode `system` role, and
-- the callback permissions the per-project system identity needs.
--
-- WHY THIS FILE EXISTS. A scheduled index run does not carry a human's token.
-- `EliteaClientTokenService.Resolve` mints the project-system PAT
-- (internal/infra/storage/index_runtime_context.go), and the Python worker
-- calls back into this service with it. The owner of that PAT is
-- `system_user_<project>@centry.user`, and `projectprovisioning`
-- (internal/application/projectprovisioning/steps.go) assigns it exactly one
-- project role: `system`.
--
-- legacyrbac resolves a project role that carries no per-project grant rows
-- through the CENTRAL fallback, which joins auth_core__role on
-- `name = <project role name> AND mode = 'default'`
-- (internal/infra/legacyrbac/postgres.go). No migration in this corpus, and not
-- internal/infra/db/migrations/001_initial.sql, ever created a default-mode
-- role named `system`. So the join matched nothing and the project-system PAT
-- resolved the EMPTY set.
--
-- The failure that follows is silent and looks like something else. Every
-- gated callback the worker makes — the S3 artifact listing and object read,
-- the secret unsecret, the model catalogue, the application and version reads —
-- answers 403. The same run started by a human succeeds, because that caller's
-- project role is admin/editor/viewer and those DO match a central role. So the
-- report is "the scheduler is broken", not "a role has no permissions".
--
-- WHY THE ROLE IS SEEDED CENTRALLY AND NOT AS PER-PROJECT ROWS. Per-project
-- rows for the `system` role would work on a Go-native database and would
-- REGRESS a legacy-backed one. There, the central default-mode `system` role
-- already exists with the full legacy matrix, so the fallback already answers.
-- Writing per-project rows would suppress that fallback for the system
-- identity and cut it down to the list below. A central role with
-- ON CONFLICT DO NOTHING changes nothing on such a database and repairs the
-- Go-native one.
--
-- WHY NOT THE WHOLE MATRIX ROW. testdata/postgres/legacy-rbac-matrix.json gives
-- the default-mode `system` role 195 permissions. This is a machine identity,
-- so it gets the callback surface and nothing else. The list below is taken
-- from the URLs elitea-sdk's client builds (runtime/clients/client.py) and the
-- gate each of those routes carries in internal/api/router.go:
--
--   /artifacts/s3/{bucket} and /{bucket}/*   configuration.artifacts.artifacts.*
--   /api/v2/secrets/secret/{projectID}/{name}
--                                            configuration.secrets.secret.unsecret
--   /api/v2/configurations/models/{projectID}
--                                            configurations.configurations.list
--   /api/v2/elitea_core/applications|application|versions|version/prompt_lib/…
--                                            models.applications.*
--
-- Every string below is in the matrix's default-mode `system` row, so this file
-- narrows the legacy role and widens nothing.
--
-- THIS DOES NOT WIDEN A HUMAN'S ACCESS EITHER. The admin console's "Apply to
-- Projects" sync (internal/api/v2/admin/roles.go) CROSS JOINs every central
-- default-mode role into every in-scope project, so after this file `system`
-- becomes a project role an operator can see and assign. Every permission
-- below is already held by the default-mode `editor`, so the set an operator
-- could hand out this way is a SUBSET of a role they can already hand out.
--
-- 0060's VIRGIN-mode guard is NOT reproduced here, for the reason 0061, 0072,
-- 0079 and 0085 state. The role has never existed in this mode on a Go
-- deployment, so no operator can have revoked anything on it.
DO $$
BEGIN

IF to_regclass('public.auth_core__role') IS NULL
   OR to_regclass('public.auth_core__role_permission') IS NULL THEN
    RAISE NOTICE '0088: auth_core tables absent, nothing to seed';
    RETURN;
END IF;

-- The role. `id` is not given: 001_initial.sql pins 1 to 6 and then advances
-- the sequence, so the serial supplies a free one. A legacy-backed database
-- already holds this row, and keeps its own id.
INSERT INTO public.auth_core__role (name, mode)
SELECT 'system', 'default'
WHERE NOT EXISTS (
    SELECT 1 FROM public.auth_core__role WHERE name = 'system' AND mode = 'default'
);

-- The callback surface, and only it.
INSERT INTO public.auth_core__role_permission (role_id, permission)
SELECT role.id, grant_row.permission
FROM public.auth_core__role AS role
CROSS JOIN (VALUES
    ('configuration.artifacts.artifacts.view'),
    ('configuration.artifacts.artifacts.create'),
    ('configuration.artifacts.artifacts.edit'),
    ('configuration.artifacts.artifacts.delete'),
    ('configuration.secrets.secret.unsecret'),
    ('configurations.configurations.list'),
    ('models.applications.applications.list'),
    ('models.applications.application.details'),
    ('models.applications.versions.get'),
    ('models.applications.version.details')
) AS grant_row(permission)
WHERE role.mode = 'default' AND role.name = 'system'
ON CONFLICT (role_id, permission) DO NOTHING;

END
$$;
