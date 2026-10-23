-- 0088_administration_secret_permissions.sql — the ADMINISTRATION-mode grants
-- for the GLOBAL secret vault.
--
--   configuration.secrets.secret.view    in administration mode
--   configuration.secrets.secret.create  in administration mode
--   configuration.secrets.secret.edit    in administration mode
--   configuration.secrets.secret.delete  in administration mode
--
-- WHY THIS FILE EXISTS. internal/api/v2/secrets/handler.go Routes() wires six
-- administration branches through `adminGate`, which is
-- RequireCentralPermissions(resolver, "administration", permission). No
-- migration granted any of the four strings above in that mode, and
-- `configuration.secrets.secret.view` was granted in no mode at all. So on a
-- database built from migrations alone, all six routes answered 403 to every
-- principal, a super_admin included. The whole global vault could not be
-- listed, read, created, renamed, deleted or hidden.
--
-- THAT IS NOT A COSMETIC HOLE. The global vault is merged into every project's
-- `{{secret.<name>}}` resolution, so a platform-wide credential could not be
-- managed at all. apps/elitea-web/src/pages/admin/api/adminSecretsApi.ts calls
-- these routes, and admin_ui's Secrets page is built on them.
--
-- A DEFAULT-MODE GRANT DOES NOT SATISFY AN ADMINISTRATION GATE.
-- internal/infra/legacyrbac/postgres.go filters `role.mode = $2`, so the six
-- default-mode grants that 0075 wrote are invisible here. 0075's header says
-- "Every route resolves auth.PermissionModeDefault"; that sentence was true of
-- the six PROJECT routes it was written for and wrong about the file's own
-- scope. 0075 is checksum-immutable, so this file states the correction:
-- the secrets surface serves TWO modes, and 0075 grants only one of them.
--
-- WHY FOUR STRINGS AND NOT SEVEN. The matrix gives the administration-mode
-- admin and super_admin all seven secrets strings. Only four of them are
-- reachable through a gate in this mode:
--
--   AdminList, AdminGet          .view
--   AdminCreate                  .create
--   AdminUpdate, AdminHide       .edit
--   AdminDelete                  .delete
--
-- `.list`, `.hide` and `.unsecret` gate no administration route. Granting them
-- would be transcription for its own sake, and a permission that reaches
-- nothing is a permission nobody can reason about. internal/api/
-- router_permission_grant_gate_test.go names exactly these four, so a future
-- route that needs another one fails that gate and arrives with its own file.
--
-- WHERE THE SPLIT COMES FROM. testdata/postgres/legacy-rbac-matrix.json, read
-- for the `administration` mode:
--
--   .view                    admin, super_admin, system
--   .create/.edit/.delete    admin, editor, super_admin, system
--
-- This file copies that. It makes no new policy.
--
-- THE EDITOR MAY WRITE AND MAY NOT READ, AND THAT IS THE MATRIX. The
-- administration-mode editor gets create, edit and delete, and not view. The
-- asymmetry is the legacy runtime's, and admin.go's header already records it.
-- It costs the editor the listing and the single-value read of the GLOBAL vault
-- only; the project vault is a different store, and 0075 gives the default-mode
-- editor the reads there.
--
-- `system` is omitted, as every other file in this corpus omits it. 0060 seeds
-- four administration-mode roles — super_admin, admin, editor and viewer — and
-- states that `system` is not in the Go product's role vocabulary. A row for it
-- would join nothing and would contradict that decision.
--
-- The administration-mode viewer gets nothing, which is the matrix.
--
-- BLAST RADIUS. These are CENTRAL grants, read by role NAME from
-- auth_core__user_role. They reach the six administration registrations only.
-- No project settings page calls those routes, so no project member gains
-- anything: the project vault stays on the default-mode grants 0075 and 0083
-- wrote.
--
-- 0060's VIRGIN-mode guard is NOT reproduced here, for the reason 0061, 0072,
-- 0079 and 0085 state. None of these grants has ever existed in this mode on a
-- Go deployment, so no operator can have revoked it.
DO $$
BEGIN

IF to_regclass('public.auth_core__role') IS NULL
   OR to_regclass('public.auth_core__role_permission') IS NULL THEN
    RAISE NOTICE '0087: auth_core tables absent, nothing to grant';
    RETURN;
END IF;

-- The reads: the listing and the single-value get. The matrix withholds them
-- from the administration-mode editor and from the administration-mode viewer.
INSERT INTO public.auth_core__role_permission (role_id, permission)
SELECT role.id, grant_row.permission
FROM public.auth_core__role AS role
CROSS JOIN (VALUES
    ('configuration.secrets.secret.view')
) AS grant_row(permission)
WHERE role.mode = 'administration' AND role.name IN ('super_admin', 'admin')
ON CONFLICT (role_id, permission) DO NOTHING;

-- The writes: create, rename/re-value, hide and delete. The editor holds these.
INSERT INTO public.auth_core__role_permission (role_id, permission)
SELECT role.id, grant_row.permission
FROM public.auth_core__role AS role
CROSS JOIN (VALUES
    ('configuration.secrets.secret.create'),
    ('configuration.secrets.secret.edit'),
    ('configuration.secrets.secret.delete')
) AS grant_row(permission)
WHERE role.mode = 'administration' AND role.name IN ('super_admin', 'admin', 'editor')
ON CONFLICT (role_id, permission) DO NOTHING;

END
$$;
