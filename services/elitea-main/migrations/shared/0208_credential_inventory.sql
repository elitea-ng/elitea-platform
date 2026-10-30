-- 0208_credential_inventory.sql — evidence for credential and access review
-- (ADR-0044 decision 5, issue #1501): the credential inventory and the
-- permission trimming report.
--
--   elitea_identity.credential_last_use
--       When each credential an agent can use was last used, one row per
--       (kind, credential_id). A side table, not a column on each source:
--       a personal access token lives in legacy-owned public.auth_core__token
--       (no DDL is issued against it), a toolkit secret is a name inside a
--       project's encrypted vault blob (no per-secret row), and the ADR-0039
--       and OAuth tables are append-mostly. Written by the credential use
--       recorder (internal/credentialuse), coalesced to one write per
--       credential per hour: an in-process map drops a repeat use inside the
--       hour without a statement, and the upsert's WHERE drops a write a
--       second replica already made inside the hour.
--
--       credential_id per kind:
--         pat                   auth_core__token.id
--         toolkit_secret        "<project id>:<secret name>"
--         mcp_oauth_token       elitea_auth.mcp_oauth_tokens.id
--         provider_oauth_grant  elitea_identity.provider_oauth_grants.id
--         credential_reference  elitea_identity.credential_references.id
--         credential_grant      "<agent binding id>/<toolkit id>/<reference id>",
--                               the live-grant key of credential_use_grants
--                               (one live grant per key), known at
--                               redemption without another read.
--
--   elitea_identity.credential_review_state
--       What the daily main.credential_staleness routine knows of each
--       credential: when it first saw it (the age of a credential with no
--       created_at, such as a toolkit secret), when it flagged it stale,
--       when it notified the owner (once per flag) and when it revoked it.
--       A revoked personal access token is deleted from auth_core__token, so
--       this row is what keeps the evidence.
--
--   elitea_lifecycle.permission_trimming_candidates
--       The weekly main.permission_trimming routine's unused grants: one row
--       per (project, agent, item). notified_at is set once the owners were
--       told; a candidate that is used again is resolved, and one that turns
--       up again later is a new candidate (notified again).
--
--   elitea_lifecycle.permission_trimming_observed
--       When the routine first saw each granted item (candidate or not). It
--       is the grant age of the items with no trustworthy timestamp of their
--       own (an egress allowlist entry, a tool added to an edited selection):
--       an item granted for less than the window is too new to judge and is
--       never a candidate. A removed item's row is deleted, so a re-grant
--       starts over.
--
-- Permissions: credentials.inventory.read and credentials.inventory.manage
-- for super_admin and admin (administration mode); agents.trimming.manage for
-- the project admin (default mode). 0060 returns early on an existing
-- database, so the grants are here and in 001_initial.sql.
--
-- Every statement is guarded or idempotent. No BEGIN/COMMIT: the ledgered
-- runner wraps the file.

CREATE SCHEMA IF NOT EXISTS elitea_identity;
CREATE SCHEMA IF NOT EXISTS elitea_lifecycle;

CREATE TABLE IF NOT EXISTS elitea_identity.credential_last_use (
    kind           text NOT NULL CHECK (kind IN (
                       'pat', 'toolkit_secret', 'mcp_oauth_token', 'provider_oauth_grant',
                       'credential_reference', 'credential_grant')),
    credential_id  text NOT NULL CHECK (length(credential_id) BETWEEN 1 AND 256),
    project_id     bigint CHECK (project_id IS NULL OR project_id > 0),
    last_used_at   timestamptz NOT NULL,
    PRIMARY KEY (kind, credential_id)
);

CREATE TABLE IF NOT EXISTS elitea_identity.credential_review_state (
    kind           text NOT NULL CHECK (kind IN (
                       'pat', 'toolkit_secret', 'mcp_oauth_token', 'provider_oauth_grant',
                       'credential_reference', 'credential_grant')),
    credential_id  text NOT NULL CHECK (length(credential_id) BETWEEN 1 AND 256),
    project_id     bigint CHECK (project_id IS NULL OR project_id > 0),
    owner_id       bigint CHECK (owner_id IS NULL OR owner_id > 0),
    first_seen_at  timestamptz NOT NULL DEFAULT clock_timestamp(),
    flagged_at     timestamptz,
    notified_at    timestamptz,
    revoked_at     timestamptz,
    revoked_by     bigint CHECK (revoked_by IS NULL OR revoked_by > 0),
    revoke_reason  text NOT NULL DEFAULT '' CHECK (length(revoke_reason) <= 500),
    PRIMARY KEY (kind, credential_id)
);

CREATE INDEX IF NOT EXISTS credential_review_state_flagged
    ON elitea_identity.credential_review_state (flagged_at)
    WHERE flagged_at IS NOT NULL AND revoked_at IS NULL;

CREATE TABLE IF NOT EXISTS elitea_lifecycle.permission_trimming_candidates (
    -- 0 for a platform egress allowlist entry (no project).
    tenant_project_id  bigint NOT NULL CHECK (tenant_project_id >= 0),
    -- 0 for a project or platform egress allowlist entry (no one agent).
    application_id     bigint NOT NULL CHECK (application_id >= 0),
    -- tool, toolkit, credential_grant or egress_host.
    item_kind          text NOT NULL CHECK (item_kind IN ('tool', 'toolkit', 'credential_grant', 'egress_host')),
    -- The item's stable key in the report (see repos/permission_trimming.go).
    item_key           text NOT NULL CHECK (length(item_key) BETWEEN 1 AND 1024),
    first_seen_at      timestamptz NOT NULL DEFAULT clock_timestamp(),
    last_seen_at       timestamptz NOT NULL DEFAULT clock_timestamp(),
    notified_at        timestamptz,
    resolved_at        timestamptz,
    PRIMARY KEY (tenant_project_id, application_id, item_kind, item_key)
);

CREATE INDEX IF NOT EXISTS permission_trimming_candidates_unnotified
    ON elitea_lifecycle.permission_trimming_candidates (tenant_project_id, application_id)
    WHERE notified_at IS NULL AND resolved_at IS NULL;

CREATE TABLE IF NOT EXISTS elitea_lifecycle.permission_trimming_observed (
    tenant_project_id  bigint NOT NULL CHECK (tenant_project_id >= 0),
    application_id     bigint NOT NULL CHECK (application_id >= 0),
    item_kind          text NOT NULL CHECK (item_kind IN ('tool', 'toolkit', 'credential_grant', 'egress_host')),
    item_key           text NOT NULL CHECK (length(item_key) BETWEEN 1 AND 1024),
    first_seen_at      timestamptz NOT NULL DEFAULT clock_timestamp(),
    last_seen_at       timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (tenant_project_id, application_id, item_kind, item_key)
);

DO $$
BEGIN
    IF to_regclass('public.auth_core__role') IS NULL
       OR to_regclass('public.auth_core__role_permission') IS NULL THEN
        RAISE NOTICE '0208: auth_core tables absent, nothing to grant';
        RETURN;
    END IF;

    -- One INSERT per mode, with the mode spelled as a literal: the router's
    -- grant gate (router_permission_grant_gate_test.go) reads the grants of
    -- this directory per `mode = '...'` block.
    INSERT INTO public.auth_core__role_permission (role_id, permission)
    SELECT role.id, grant_row.permission
    FROM public.auth_core__role AS role
    CROSS JOIN (VALUES
        ('credentials.inventory.read'),
        ('credentials.inventory.manage')
    ) AS grant_row(permission)
    WHERE role.mode = 'administration' AND role.name IN ('super_admin', 'admin')
    ON CONFLICT (role_id, permission) DO NOTHING;

    INSERT INTO public.auth_core__role_permission (role_id, permission)
    SELECT role.id, 'agents.trimming.manage'
    FROM public.auth_core__role AS role
    WHERE role.mode = 'default' AND role.name = 'admin'
    ON CONFLICT (role_id, permission) DO NOTHING;

    -- legacyrbac reads the central grants only for a caller with no
    -- per-project rows (shared/0090). A project whose matrix was saved gets
    -- the string on its `admin` role here, as 0204 does. The string never
    -- existed, so no saved matrix withheld it on purpose.
    IF to_regclass('public.auth_core__project_role') IS NULL
       OR to_regclass('public.auth_core__project_role_permission') IS NULL THEN
        RETURN;
    END IF;

    INSERT INTO public.auth_core__project_role_permission (project_id, role_id, permission)
    SELECT DISTINCT saved.project_id, saved.role_id, 'agents.trimming.manage'
    FROM (
        SELECT DISTINCT project_id, role_id
        FROM public.auth_core__project_role_permission
        WHERE role_id IS NOT NULL
    ) AS saved
    JOIN public.auth_core__project_role AS project_role
      ON project_role.id = saved.role_id
     AND project_role.project_id = saved.project_id
    WHERE project_role.name = 'admin'
    ON CONFLICT (project_id, role_id, permission) DO NOTHING;
END
$$;
