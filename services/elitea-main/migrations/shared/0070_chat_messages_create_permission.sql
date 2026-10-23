-- 0070_chat_messages_create_permission.sql — the DEFAULT-mode grant that makes
-- the agent execution-event stream reachable (#354).
--
--   models.chat.messages.create  ← elitea_core/api/v2/messages.py
--
-- WHY THIS FILE EXISTS. TWO gates on the chat path require this permission,
-- and no migration in this corpus grants it. 001_initial.sql does not seed it
-- either.
--
--   * The agent START route, as `CurrentApplicationStartPermission`
--     (internal/api/v2/agentexecution/route.go:32, applied at :81 and :105).
--     It gates POST .../messages/prompt_lib/... and .../continue_predict/... .
--   * The execution-event STREAM, on the agent branch of
--     AuthorizeExecutionEvents (internal/runtimecomposition/public_authorizer.go,
--     the `agent.execute.application.v1` and `agent.execute.adhoc.v1`
--     capabilities).
--
-- So on a clean database a user cannot start an agent AND cannot read the
-- stream. Both answer 403 to every caller, and the chat never produces a reply.
-- This is 0063's stated consequence again: gating a route on a permission
-- nothing grants is 403-for-everyone, which reads as a broken page rather than
-- as a missing grant.
--
-- The permission name is not new. 0063's header already names it, at the point
-- where it lists the strings it deliberately does NOT grant. The end-to-end
-- seed grants it per project (apps/elitea-web/scripts/e2e-stack.sh:461 and
-- :674), so every test passed and the gap stayed hidden.
--
-- WHERE THE SPLIT COMES FROM. testdata/postgres/legacy-rbac-matrix.json holds
-- `models.chat.messages.create` for `default` / {`admin`, `editor`, `viewer`}.
-- The name itself comes from testdata/legacy/legacy-rbac-static-catalog.json,
-- machine-generated from the legacy runtime `check_api` declarations. So this
-- file is parity restoration, not a new policy. `system` and `super_admin` are
-- omitted, as everywhere else in this corpus: Go seeds neither role in the
-- default mode.
--
-- Its sibling `models.applications.tool.patch` arrived with 0068, for the same
-- authorizer and for the index-ingest capability. The two are read the same
-- way, so this omission was accidental.
--
-- BLAST RADIUS. `legacyrbac`'s projectPermissions() falls back to the CENTRAL
-- default-mode grants by role NAME only for a project that carries NO
-- per-project role_permission rows. That is the fresh-Go-database shape. It is
-- never the shape of a legacy-backed database, of a legacy dump, or of the E2E
-- stack, all of which seed per-project rows and therefore suppress the fallback
-- entirely. So no existing deployment's members gain anything here. The
-- fallback is also joined THROUGH the caller's assigned project roles, so a
-- non-member has no row to fall back from and gains nothing at all.
--
-- Members' permission sets are already non-empty since 0062, so this changes no
-- set-emptiness transition of the kind #276 tracks. The
-- `configuration.validate.v1` branch of the same authorizer reads only the SIZE
-- of the set, so it does not move either.
--
-- 0060's VIRGIN-mode guard is NOT reproduced, for the reason 0061, 0062, 0063,
-- 0066, 0068 and 0069 all give: this permission has never existed on a Go
-- deployment, so no operator can have revoked it. To skip configured
-- deployments would leave exactly those unable to stream a chat reply.
DO $$
BEGIN

IF to_regclass('public.auth_core__role') IS NULL
   OR to_regclass('public.auth_core__role_permission') IS NULL THEN
    RAISE NOTICE '0070: auth_core tables absent, nothing to grant';
    RETURN;
END IF;

INSERT INTO public.auth_core__role_permission (role_id, permission)
SELECT role.id, grant_row.permission
FROM public.auth_core__role AS role
CROSS JOIN (VALUES
    ('models.chat.messages.create')
) AS grant_row(permission)
WHERE role.mode = 'default' AND role.name IN ('admin', 'editor', 'viewer')
ON CONFLICT (role_id, permission) DO NOTHING;

END
$$;
