-- 0068_elitea_core_route_permissions.sql — the DEFAULT-mode grants the
-- /elitea_core per-route permission gates (#302, #313) are resolved against.
--
-- WHY THIS FILE EXISTS, and why it had to land WITH the gating rather than
-- after it. #302's first pass closed the cross-tenant hole at the MEMBERSHIP
-- tier and deliberately stopped there, because the permission NAMES were
-- recoverable but nothing granted them: measured against a real database
-- (internal/infra/db/migrations/001_initial.sql plus the whole shared history
-- on pgvector:0.8.1-pg18), a Go-bootstrapped deployment resolves exactly SEVEN
-- default-mode permissions — `models.project_context.view` (0062),
-- `models.monitoring.tracing.view`, `models.chat.messages.list`,
-- `models.chat.messages.details` (0063) and the three
-- `models.applications.index_meta.*` strings (0066). The routes gated in this
-- change need roughly sixty more. 0063's header states the consequence:
-- "gating a route on a permission nothing grants is 403-for-everyone, which
-- reads as a broken page rather than as a missing grant" — the same shape as
-- #93's `index_meta.details`, which presented as an indefinitely-loading rail
-- rather than as a refusal.
--
-- WHERE THE SPLIT COMES FROM. Every string below is transcribed from
-- testdata/postgres/legacy-rbac-matrix.json, exported from a real legacy
-- database by scripts/database/export_legacy_rbac_matrix.sql, at the
-- `mode = 'default'` rows for the `admin`, `editor` and `viewer` roles. It is
-- parity restoration, not a new policy: this is what a legacy-backed deployment
-- already resolves today. Nothing here is inferred from the route's verb — the
-- matrix disagrees with the obvious guess in several places and the matrix
-- wins:
--
--   * `models.applications.upload_icon.get` is admin/editor only. A viewer
--     cannot LIST uploaded icons even though it is a read.
--   * `models.applications.task.delete` and `models.applications.fork.post`
--     are granted to viewer even though both are writes.
--   * `models.chat.conversations.delete` is granted to viewer; the neighbouring
--     `models.chat.conversation.edit` is not.
--   * `configuration.roles.permissions.view` is default-mode ADMIN only, which
--     is why no product route in this change is gated on it (see the router's
--     note on `/permissions/prompt_lib/{projectID}`, which stays ungated
--     because it is the caller's own permission self-read — the legacy
--     runtime's counterpart, auth/api/v2/permissions.py, carries no guard
--     either).
--
-- The permission NAMES come from testdata/legacy/legacy-rbac-static-catalog.json
-- (machine-generated from the legacy runtime `check_api` declarations), matched
-- per module and per verb.
-- internal/api/router_elitea_core_permission_map_test.go re-checks both
-- directions against those two files, so a name that exists in neither, or a
-- gate whose permission this file does not grant, fails the build rather than
-- shipping as a 403.
--
-- BLAST RADIUS. `legacyrbac`'s projectPermissions() falls back to the CENTRAL
-- default-mode grants by role NAME only for a project that carries NO
-- per-project role_permission rows. That is the fresh-Go-database shape. It is
-- never the shape of a legacy-backed database, of a legacy dump, or of the E2E
-- stack, all of which seed per-project rows and therefore suppress the fallback
-- entirely — so no existing deployment's members gain anything here (pinned in
-- internal/infra/legacyrbac/elitea_core_route_grant_postgres_integration_test.go,
-- the same way 0062's, 0063's and 0066's blast radius is pinned). The fallback
-- is also joined THROUGH the caller's assigned project roles, so a non-member
-- has no row to fall back from and gains nothing at all.
--
-- Members' permission sets are already non-empty since 0062, so this changes no
-- set-emptiness transition of the kind #276 tracks.
--
-- Idempotent and additive: it grants to roles that already exist, never creates
-- one, and conflicts are ignored. `system` and `super_admin` are omitted, as
-- everywhere else in this corpus. 0060's VIRGIN-mode guard is NOT reproduced,
-- for the reason 0061/0062/0063/0066 all give — these permissions have never
-- existed on a Go deployment, so no operator can have revoked them, and
-- skipping configured deployments would leave exactly those unable to reach the
-- routes.
DO $$
BEGIN

IF to_regclass('public.auth_core__role') IS NULL
   OR to_regclass('public.auth_core__role_permission') IS NULL THEN
    RAISE NOTICE '0068: auth_core tables absent, nothing to grant';
    RETURN;
END IF;

-- Granted to admin, editor AND viewer in the legacy default mode.
INSERT INTO public.auth_core__role_permission (role_id, permission)
SELECT role.id, grant_row.permission
FROM public.auth_core__role AS role
CROSS JOIN (VALUES
    -- Agents and their versions (applications.py, application.py, versions.py,
    -- version.py, application_skills.py, check_version_in_use.py).
    ('models.applications.applications.list'),
    ('models.applications.applications.details'),
    ('models.applications.application.details'),
    ('models.applications.versions.get'),
    ('models.applications.version.details'),
    -- Skills (skills.py, skill.py, skill_export.py).
    ('models.applications.skills.list'),
    ('models.applications.skills.details'),
    ('models.applications.skills.export'),
    -- Toolkits and their index metadata (tools.py, tool.py, toolkits.py,
    -- toolkit_types.py, toolkit_available_tools.py, toolkit_discover_tools.py,
    -- toolkit_validator.py, export_toolkit.py, index_types.py, index_cancel.py).
    ('models.applications.tools.list'),
    ('models.applications.tool.details'),
    ('models.applications.toolkits.details'),
    ('models.applications.toolkit_validator.check'),
    ('models.applications.export_toolkit.export'),
    ('models.applications.index_types.details'),
    ('models.applications.task.delete'),
    -- Fork, export and validation (fork.py, fork_toolkit.py,
    -- attach_public_skill.py, skill_export_fork.py, export_import.py,
    -- version_validator.py).
    ('models.applications.fork.post'),
    ('models.applications.version_validator.check'),
    ('models.applications.export_import.export'),
    -- Catalogue reads reachable from a project page (trending_authors.py,
    -- author.py, recommendations.py shares applications.list above).
    ('models.applications.trending_authors.list'),
    -- Chat (conversations.py, conversation.py, messages.py, message.py,
    -- participants.py, participant.py, entity_settings.py, canvases.py,
    -- canvas.py, attachments.py, regenerate.py, select_conversation.py,
    -- context_analytics.py, folder.py).
    -- conversations.py declares `list` OR `list_custom`; the legacy matrix
    -- grants both to admin, editor and viewer alike, so the route's gate names
    -- `list` alone and `list_custom` is not seeded — a grant that gates nothing
    -- widens a member's rights for no route.
    ('models.chat.conversations.list'),
    ('models.chat.conversations.create'),
    ('models.chat.conversations.delete'),
    ('models.chat.conversations.regenerate'),
    ('models.chat.conversation.details'),
    ('models.chat.conversation.update'),
    ('models.chat.messages.delete'),
    ('models.chat.participants.create'),
    ('models.chat.participant.delete'),
    ('models.chat.entity_settings.update'),
    -- canvases.py declares `list` as well; this router registers the CREATE on
    -- /canvases and the read on /canvas/{id}, and has no canvas listing, so
    -- `canvas.list` is not seeded.
    ('models.chat.canvas.create'),
    ('models.chat.canvas.details'),
    ('models.chat.canvas.update'),
    ('models.chat.attachments.create'),
    ('models.chat.attachments.delete'),
    ('models.chat.folders.get'),
    ('models.chat.folders.create'),
    ('models.chat.folders.update'),
    ('models.chat.folders.delete'),
    -- Shared prompt-library surfaces (tags.py, search_options.py, author.py,
    -- agent_categories.py and skill_categories.py share tags.list).
    ('models.promptlib_shared.tags.list'),
    -- author.py's `author.detail` is not seeded: /author names an AUTHOR id in
    -- its path, not a project, so no project-scoped gate can resolve against it
    -- and the route stays open exactly as the social plugin's copy is.
    ('models.promptlib_shared.search'),
    -- The project member and role listings the project settings page reads
    -- (admin/api/v2/users.py, admin/api/v2/roles.py — the ProjectAPI variants).
    ('configuration.users.users.view'),
    ('configuration.roles.roles.view')
) AS grant_row(permission)
WHERE role.mode = 'default' AND role.name IN ('admin', 'editor', 'viewer')
ON CONFLICT (role_id, permission) DO NOTHING;

-- Granted to admin and editor only. A viewer that could publish an agent,
-- rewrite a toolkit or replace a project icon would be a widening this file has
-- no mandate to make; the legacy matrix withholds every string below from the
-- default-mode viewer.
INSERT INTO public.auth_core__role_permission (role_id, permission)
SELECT role.id, grant_row.permission
FROM public.auth_core__role AS role
CROSS JOIN (VALUES
    ('models.applications.applications.create'),
    ('models.applications.application.update'),
    ('models.applications.application.delete'),
    ('models.applications.versions.create'),
    ('models.applications.version.update'),
    ('models.applications.version.delete'),
    ('models.applications.skills.create'),
    ('models.applications.skills.update'),
    ('models.applications.skills.delete'),
    ('models.applications.skills.publish'),
    ('models.applications.tools.create'),
    ('models.applications.tool.update'),
    ('models.applications.tool.patch'),
    ('models.applications.tool.delete'),
    ('models.applications.publish.post'),
    ('models.applications.unpublish.post'),
    ('models.applications.export_import.import'),
    ('models.applications.application_relation.patch'),
    ('models.applications.upload_icon.get'),
    ('models.applications.upload_icon.post'),
    ('models.applications.upload_icon.update'),
    ('models.applications.upload_icon.delete'),
    ('models.chat.conversation.edit'),
    ('models.project_context.edit')
) AS grant_row(permission)
WHERE role.mode = 'default' AND role.name IN ('admin', 'editor')
ON CONFLICT (role_id, permission) DO NOTHING;

END
$$;
