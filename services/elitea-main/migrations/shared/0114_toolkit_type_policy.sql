-- 0114_toolkit_type_policy.sql — the operator's control over WHICH toolkit
-- types this deployment offers, and to whom.
--
-- ---------------------------------------------------------------------------
-- WHY THIS EXISTS
-- ---------------------------------------------------------------------------
--
-- The platform serves a toolkit TYPE catalogue at
-- `GET /api/v2/elitea_core/toolkits/prompt_lib/{projectID}`. Every type in that
-- map becomes a tile in the "+ Toolkit" chooser. An operator who wants one
-- fewer tile, or one tile for one client project only, had exactly two levers
-- before this file:
--
--   * the guardrails DENY-list (`toolkit_security.blocked_toolkits`, a JSON
--     array in `centry.platform_config`), which is deployment-wide, carries no
--     provenance, and cannot say WHY a type is off; and
--   * a code change, because the served key set was a hand-written Go map.
--
-- Neither is a native admin surface. This file is the store behind one:
-- `Admin › Toolkits`, which lists every served type and lets an administrator
-- enable it, disable it, or restrict it to named projects, always with a
-- written reason and a recorded decider.
--
-- ---------------------------------------------------------------------------
-- ABSENCE IS NOT A DECISION TO WITHHOLD
-- ---------------------------------------------------------------------------
--
-- THE TABLES RECORD DEVIATIONS ONLY. A deployment that has never opened the
-- page has zero rows here, and zero rows must serve the FULL default catalogue.
--
-- This is stated first because the opposite reading is the recurring defect in
-- this repository: an empty result set read as "everything is off". An
-- allow-list table would have made a fresh install serve no toolkit at all, and
-- the failure would have looked like a broken catalogue rather than a missing
-- seed. There is deliberately no bootstrap INSERT below, and there must never
-- be one: a seeded row would turn every later default change in the SDK
-- snapshot into a row nobody updated.
--
-- ---------------------------------------------------------------------------
-- WHY TWO TABLES, AND NOT `centry.platform_config`
-- ---------------------------------------------------------------------------
--
-- The guardrails block is one JSONB value holding an array of names. It can say
-- "github is blocked". It cannot say "github is blocked because the legal
-- review of 2026-08 refused source-code egress, decided by alice@example.com on
-- that date", and it cannot say "blocked everywhere except project 42". A
-- deny-list of bare strings is the wrong shape for a decision an operator has
-- to justify and later revisit.
--
-- The provider-hub plane (`provider_hub.*`, migrations 0107 and 0109) is also
-- the wrong home. That plane is about EXTERNAL origins: a manifest a third
-- party published, its digest, and the reviewed policy overlay bound to it.
-- These rows are about BUILT-IN types that ship inside the pinned SDK snapshot.
-- Putting them there would make `provider_id` mean two different things.
--
-- WHY SHARED AND NOT PER TENANT. The catalogue is one fact per deployment, for
-- the reason 0094 gives for the pre-built MCP catalogue: a per-tenant copy
-- would be N copies of one platform decision, and they would drift. The
-- per-PROJECT exception lives in the second table as a row, not as a schema.
--
-- ---------------------------------------------------------------------------
-- THE TYPE KEY IS THE SCHEMA TITLE
-- ---------------------------------------------------------------------------
--
-- `toolkit_type` holds the name the served catalogue uses as its map key — the
-- Pydantic schema `title` in the SDK, e.g. `kubernetes`. It is NOT the SDK
-- registry import key, which for that same toolkit is `k8s`. A policy keyed on
-- the import key silently never matches the catalogue, which is a control that
-- appears to work and does nothing. The Go store normalises nothing and matches
-- the catalogue key verbatim, so the two cannot drift.
--
-- ---------------------------------------------------------------------------
-- GUARDRAILS STAY TERMINAL
-- ---------------------------------------------------------------------------
--
-- `availability = 'enabled'` here does NOT re-admit a type the guardrails
-- deny-list blocks. The Go catalogue applies this policy BEFORE
-- `applyGuardrailsToCatalogue`, never instead of it. Two controls that can each
-- overrule the other are two controls an operator cannot reason about; the
-- deny-list is the last word, and this table decides only among the types that
-- survive it.

CREATE SCHEMA IF NOT EXISTS centry;

-- ── the deployment-wide decision ────────────────────────────────────────────
--
-- One row per type the operator has decided about. No row means "not decided",
-- which serves the type.
CREATE TABLE IF NOT EXISTS centry.toolkit_type_policy (
    toolkit_type TEXT        PRIMARY KEY,
    -- 'enabled'    — served to every project.
    -- 'disabled'   — served to no project. A project grant cannot re-admit it;
    --                see the CHECK on the grant table's own availability and
    --                the resolution order in
    --                internal/application/toolkitcatalogue/resolve.go.
    -- 'restricted' — served ONLY to projects with an 'enabled' grant row. This
    --                is the "this client may use `sql` and nobody else may"
    --                case, and it is a THIRD value rather than
    --                'disabled' + grants because the two read differently to an
    --                operator: 'disabled' is a refusal, 'restricted' is an
    --                allow-list that is expected to have members.
    availability TEXT        NOT NULL,
    -- NEVER EMPTY. The page shows this sentence beside the decision, and a
    -- decision with no reason is one the next operator cannot safely reverse.
    -- The CHECK is what makes the requirement true; a Go validation alone is a
    -- rule the next write path can forget.
    reason       TEXT        NOT NULL,
    decided_by   TEXT        NOT NULL,
    decided_at   TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT toolkit_type_policy_availability_known
        CHECK (availability IN ('enabled', 'disabled', 'restricted')),
    CONSTRAINT toolkit_type_policy_reason_present
        CHECK (btrim(reason) <> ''),
    CONSTRAINT toolkit_type_policy_decider_present
        CHECK (btrim(decided_by) <> ''),
    CONSTRAINT toolkit_type_policy_type_bounded
        CHECK (btrim(toolkit_type) <> '' AND length(toolkit_type) <= 256)
);

-- ── the per-project exception ───────────────────────────────────────────────
--
-- A row here overrides the deployment decision for ONE project, in either
-- direction: 'enabled' admits a type under a 'restricted' policy, 'disabled'
-- withdraws a type from one project under an 'enabled' policy.
--
-- THE FOREIGN KEY IS DELIBERATE. A grant is an exception TO a decision, so a
-- grant with no decision to except is a row that describes nothing. Requiring
-- the policy row first is also what makes the admin page's listing a single
-- join instead of a full outer join over two independent tables, and ON DELETE
-- CASCADE means reverting a type to its default takes its exceptions with it —
-- which is the only correct reading of "revert to default".
CREATE TABLE IF NOT EXISTS centry.toolkit_type_project_grant (
    toolkit_type TEXT        NOT NULL
        REFERENCES centry.toolkit_type_policy(toolkit_type) ON DELETE CASCADE,
    -- BIGINT, matching every other project reference in this corpus. Not a
    -- foreign key: projects live in the pylon-owned `centry.project` table on a
    -- pylon-backed deployment and are created by a different owner, and 0030's
    -- history records what claiming ownership of that table costs.
    project_id   BIGINT      NOT NULL,
    availability TEXT        NOT NULL,
    reason       TEXT        NOT NULL,
    granted_by   TEXT        NOT NULL,
    granted_at   TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (toolkit_type, project_id),
    CONSTRAINT toolkit_type_project_grant_availability_known
        CHECK (availability IN ('enabled', 'disabled')),
    CONSTRAINT toolkit_type_project_grant_reason_present
        CHECK (btrim(reason) <> ''),
    CONSTRAINT toolkit_type_project_grant_granter_present
        CHECK (btrim(granted_by) <> ''),
    CONSTRAINT toolkit_type_project_grant_project_positive
        CHECK (project_id > 0)
);

-- The catalogue read is "every grant for THIS project", once per catalogue
-- request. The primary key leads with toolkit_type, so it cannot serve that
-- predicate; this index can.
CREATE INDEX IF NOT EXISTS toolkit_type_project_grant_project_idx
    ON centry.toolkit_type_project_grant (project_id, toolkit_type);

-- ── the permission that gates the admin surface ─────────────────────────────
--
-- `toolkit_catalogue.type.manage`, and it is a NEW string.
--
-- CHOSEN, NOT RECOVERED. The legacy platform has no add-or-remove-a-type
-- surface at all: its only operator control is the guardrails deny-list, whose
-- editor is gated on `runtime.plugins`. So there is no pylon declaration to
-- transcribe, and testdata/postgres/legacy-rbac-matrix.json has no row for it.
--
-- WHY NOT REUSE `runtime.plugins`. That string is the widest configuration
-- grant on the admin Configuration page and it already reaches the guardrails
-- editor. This control is different in kind: the deny-list stops a type
-- WORKING, while this decides what the product OFFERS, per project. An operator
-- trusted to edit a plugin's configuration values is not automatically the
-- operator who decides which clients may build a Salesforce toolkit. Keeping
-- the strings separate is what makes the two separately grantable later; a
-- reuse cannot be undone without breaking every deployment that relied on it.
--
-- WHO HOLDS IT. `super_admin`, `admin` and `system` in the `administration`
-- mode, which is 0113's holder set and the shape the legacy matrix gives every
-- `configuration.*` write. `editor` and `viewer` get nothing: this is a
-- platform-wide catalogue decision, not project content.
--
-- `system` is included although internal/infra/db/migrations/001_initial.sql
-- seeds no administration-mode `system` role — the WHERE clause simply matches
-- nothing there. On a pylon-backed database the role does exist, and omitting
-- it would narrow that deployment's existing matrix. 0113 records the same.
--
-- WHY A NEW FILE. 0060 returns early when any administration-mode role already
-- exists, so an edit there would seed fresh databases only and leave every
-- running deployment at 403. Migrations are also checksum-immutable. The
-- to_regclass guard makes this block inert on a database that has no auth_core
-- tables at all — elitea-migrate runs against exactly such databases, and
-- several integration suites apply this whole corpus to a bare one. ON CONFLICT
-- DO NOTHING makes a repeat run a no-op.
DO $$
BEGIN

IF to_regclass('public.auth_core__role') IS NULL
   OR to_regclass('public.auth_core__role_permission') IS NULL THEN
    RAISE NOTICE '0114: auth_core tables absent, nothing to grant';
    RETURN;
END IF;

INSERT INTO public.auth_core__role_permission (role_id, permission)
SELECT role.id, 'toolkit_catalogue.type.manage'
FROM public.auth_core__role AS role
WHERE role.mode = 'administration' AND role.name IN ('super_admin', 'admin', 'system')
ON CONFLICT (role_id, permission) DO NOTHING;

END
$$;
