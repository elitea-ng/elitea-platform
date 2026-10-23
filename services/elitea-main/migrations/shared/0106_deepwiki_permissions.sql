-- 0106_deepwiki_permissions.sql — the two DEFAULT-mode grants behind the
-- DeepWiki facade (ADR-0022, phase P2).
--
--   models.applications.deepwiki.read      ← GET  /deepwiki/slots/{project_id}
--                                            GET  /deepwiki/invocations/...
--   models.applications.deepwiki.generate  ← POST /deepwiki/tools/.../invoke
--                                            DELETE /deepwiki/invocations/...
--
-- WHY THESE NAMES ARE NOT A TRANSCRIPTION. Every other grant file in this
-- corpus restores a string that legacy already declared, and says where it
-- came from. This one cannot: the legacy `deepwiki_plugin` has no `api/`
-- package and no `check_api` call anywhere in it. Its five routes
-- (descriptor, health, slots, invoke, invocations) were reached by the legacy
-- runtime provider hub over mTLS and authorised by the HOP, never by a project
-- role. testdata/legacy/legacy-rbac-static-catalog.json therefore carries
-- nothing for it, and neither does testdata/postgres/legacy-rbac-matrix.json.
--
-- So these are new strings, chosen rather than recovered — 0104 was the first
-- file here to do that, for Agent Evaluation, and this is the second. The
-- naming follows the corpus convention for authored project content
-- (`models.applications.*`) because that is what a generated wiki is: one
-- project's content, produced from that project's repositories.
--
-- WHY THE SPLIT IS READ/GENERATE AND NOT READ/WRITE. Reading capacity is what
-- the UI polls to decide whether to offer the button at all, and polling an
-- invocation is how it follows one already running. Gating those behind the
-- write grant would make the page 403 for everyone allowed to look but not to
-- generate — a viewer would see a broken screen rather than a disabled button.
-- Starting a generation is the expensive, side-effectful half: it clones a
-- repository, spends model budget and writes an index. Cancelling is grouped
-- with it, not with the reads, because stopping another member's running
-- generation is a mutation of shared state and not an observation of it.
--
-- Viewer therefore holds the read and not the generate. That is the same
-- asymmetry 0105 records from legacy's own predict_llm declarations, arrived
-- at here from the route semantics rather than from a dump.
--
-- WHY A NEW FILE. Migrations here are checksum-immutable; nothing already
-- applied can be appended to. And the route is gated on these strings by
-- internal/api/v2/deepwiki/route.go, so without this file the facade would
-- ship registered and refuse every caller — 0063's stated consequence, "403
-- for everyone, which reads as a broken page rather than as a missing grant".
--
-- BLAST RADIUS. Additive and idempotent: it grants to roles that already
-- exist, never creates one, and conflicts are ignored. The facade itself is
-- mounted only when ELITEA_DEEPWIKI_ENABLED is set, so on every deployment
-- that has not turned it on these two strings grant access to nothing.
--
-- The central grant alone would be inert on any project carrying its own
-- permission rows, because legacyrbac's projectPermissions() discards the
-- central set wholesale for those callers — shared/0090's header and
-- migrations/project_override_reconciliation_test.go both say so. The second
-- statement is that override delivery. It is unconditional, as 0105's is: these
-- strings have never existed on any deployment, so no operator can have
-- narrowed a saved matrix to exclude them. Every omission is an absence.
--
-- 0060's VIRGIN-mode guard is not reproduced, for the reason 0061/0062/0063/
-- 0066/0068/0102/0104/0105 all give: never granted anywhere, so never revoked.
-- `system` and `super_admin` are omitted, as everywhere else in this corpus.
DO $$
BEGIN

IF to_regclass('public.auth_core__role') IS NULL
   OR to_regclass('public.auth_core__role_permission') IS NULL THEN
    RAISE NOTICE '0106: auth_core tables absent, nothing to grant';
    RETURN;
END IF;

-- The read: capacity and progress, which every project role may see.
INSERT INTO public.auth_core__role_permission (role_id, permission)
SELECT role.id, 'models.applications.deepwiki.read'
FROM public.auth_core__role AS role
WHERE role.mode = 'default' AND role.name IN ('admin', 'editor', 'viewer')
ON CONFLICT (role_id, permission) DO NOTHING;

-- The write: starting and cancelling a generation.
INSERT INTO public.auth_core__role_permission (role_id, permission)
SELECT role.id, 'models.applications.deepwiki.generate'
FROM public.auth_core__role AS role
WHERE role.mode = 'default' AND role.name IN ('admin', 'editor')
ON CONFLICT (role_id, permission) DO NOTHING;

-- The override delivery. Only projects that ALREADY carry per-project rows are
-- touched: a project role with no override rows still falls back to the central
-- matrix above, and handing it a snapshot it never had would freeze it out of
-- every future central grant — the very hole this block exists to close.
IF to_regclass('public.auth_core__project_role') IS NULL
   OR to_regclass('public.auth_core__project_role_permission') IS NULL THEN
    RAISE NOTICE '0106: no per-project permission tables, central grants are the whole story here';
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
    ('models.applications.deepwiki.read', ARRAY['admin', 'editor', 'viewer']),
    ('models.applications.deepwiki.generate', ARRAY['admin', 'editor'])
) AS grant_row(permission, roles)
WHERE project_role.name = ANY (grant_row.roles)
ON CONFLICT (project_id, role_id, permission) DO NOTHING;

END
$$;
