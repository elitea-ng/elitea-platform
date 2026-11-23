-- 0260_admin_role_grants.sql — an append-only history of central role
-- assignments, and the membership index that reads by user (issue #2499,
-- ADR-0059 R0.10a, decision 8 "Recording role use").
--
-- THE GAP. public.auth_core__user_role (id, user_id, role_id) records who holds
-- a role and nothing else: no actor, no time, no reason. Both assignment routes
-- deleted every row of the mode and inserted the new one, so even the row id
-- said nothing. Nobody could answer "who made this person an administrator,
-- and when".
--
-- THE TABLE. One row per change, written in the same transaction as the
-- assignment change by internal/adminaccess/rolegrants (the single writer):
--
--   user_id        the account whose role changed
--   role_name      the role's NAME and
--   role_mode      its mode, not its id: a deleted custom role must stay
--                  readable here
--   action         granted | revoked
--   actor_user_id  the user who made the change; NULL for bootstrap and
--                  migration, whose source says why there is none
--   occurred_at    timestamptz
--   reason         free text, when the change carried one
--   source         console (a browser session), api (a token), scim,
--                  migration, bootstrap (identity.initial_global_admins at
--                  first sign-in), operator (the operator command) or review
--                  (a confirmed provisional grant)
--   source_ref     a reference within the source, such as a SCIM binding id
--
-- No column is added to auth_core__user_role, by decision. No foreign key to
-- auth_core__user either: the history outlives the account it describes, and
-- the shared corpus also runs on databases without the auth_core tables.
--
-- APPEND-ONLY. A trigger refuses UPDATE and DELETE (row level) and TRUNCATE
-- (statement level). History starts at this migration: assignments made
-- before it have no row.
--
-- THE INDEX. public.auth_core__project_user_role is UNIQUE (project_id,
-- user_id, role_id), so nothing leads on user_id, and the permission resolver
-- (legacyrbac membershipPermissions) reads it by user on every request. A
-- plain CREATE INDEX inside the runner's transaction: the table is small on
-- every known deployment (one row per membership role). Skipped on a database
-- without the table.
--
-- IDEMPOTENT throughout. No BEGIN/COMMIT: the ledgered runner executes each
-- file inside one transaction with its ledger row (migrate/runner.go apply).

CREATE SCHEMA IF NOT EXISTS elitea_auth;

CREATE TABLE IF NOT EXISTS elitea_auth.admin_role_grants (
    id            bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id       integer     NOT NULL,
    role_name     text        NOT NULL,
    role_mode     text        NOT NULL,
    action        text        NOT NULL,
    actor_user_id integer,
    occurred_at   timestamptz NOT NULL DEFAULT now(),
    reason        text,
    source        text        NOT NULL,
    source_ref    text,
    CONSTRAINT admin_role_grants_action_known
        CHECK (action IN ('granted', 'revoked')),
    CONSTRAINT admin_role_grants_source_known
        CHECK (source IN ('console', 'api', 'scim', 'migration', 'bootstrap', 'operator', 'review')),
    CONSTRAINT admin_role_grants_role_present
        CHECK (length(btrim(role_name)) > 0 AND length(btrim(role_mode)) > 0),
    -- A person acted through the console or a token; bootstrap and migration
    -- have no person by definition.
    CONSTRAINT admin_role_grants_actor_matches_source
        CHECK (
            (source IN ('console', 'api') AND actor_user_id IS NOT NULL)
            OR (source IN ('bootstrap', 'migration') AND actor_user_id IS NULL)
            OR source IN ('scim', 'operator', 'review')
        )
);

-- The Access history of one person, newest first (R0.10b reads it).
CREATE INDEX IF NOT EXISTS admin_role_grants_user_idx
    ON elitea_auth.admin_role_grants (user_id, occurred_at DESC, id DESC);

CREATE OR REPLACE FUNCTION elitea_auth.admin_role_grants_append_only()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION 'elitea_auth.admin_role_grants is append-only: % refused', TG_OP
        USING ERRCODE = 'insufficient_privilege';
END;
$$;

DROP TRIGGER IF EXISTS admin_role_grants_append_only ON elitea_auth.admin_role_grants;
CREATE TRIGGER admin_role_grants_append_only
    BEFORE UPDATE OR DELETE ON elitea_auth.admin_role_grants
    FOR EACH ROW EXECUTE FUNCTION elitea_auth.admin_role_grants_append_only();

DROP TRIGGER IF EXISTS admin_role_grants_no_truncate ON elitea_auth.admin_role_grants;
CREATE TRIGGER admin_role_grants_no_truncate
    BEFORE TRUNCATE ON elitea_auth.admin_role_grants
    FOR EACH STATEMENT EXECUTE FUNCTION elitea_auth.admin_role_grants_append_only();

DO $$
BEGIN
    IF to_regclass('public.auth_core__project_user_role') IS NULL THEN
        RAISE NOTICE '0260: auth_core absent, membership user index skipped';
        RETURN;
    END IF;
    CREATE INDEX IF NOT EXISTS auth_core__project_user_role_user_idx
        ON public.auth_core__project_user_role (user_id);
END;
$$;
