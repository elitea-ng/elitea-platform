-- 0265_token_admin_access.sql — the administration access of a credential row
-- of public.auth_core__token (ADR-0059 decision 7 "Enforcement", the
-- credentials table and the `inherit` rule; "Consequences, Migrations"; story
-- R1.1, issue 2505).
--
-- Today every credential that resolves to a person carries that person's full
-- administration set (finding C8). Two columns make the narrower answers
-- expressible:
--
--   admin_access  `none`     no administration rights (the default);
--                 `scoped`   only the names in admin_scopes;
--                 `inherit`  the owner's set, until the sunset date (R4.2).
--   admin_scopes  the administration permission names of a `scoped` token.
--                 Empty for `none` and `inherit` (a CHECK keeps it so).
--
-- NOTHING READS THEM YET. The resolver (R1.2a, issue 2506) is the first
-- reader; until it merges these columns are written and not enforced, and no
-- decision changes. They describe administration rights only, never the token
-- secret: the planned change of the personal access token secret format
-- (issue 2960, a random secret stored as a hash) touches neither column.
--
-- BACKFILL, in this file, once. A personal access token whose owner holds at
-- least one administration-mode permission now (auth_core__user_role ->
-- auth_core__role with mode 'administration' -> auth_core__role_permission; a
-- role without permissions does not count) gets `inherit`, so it resolves what
-- it resolves today. Every other row stays `none`, which is what it resolves
-- today. Two kinds of row on this table are not personal access tokens and
-- stay `none` whatever their owner holds:
--
--   native device session anchors (ADR-0025, shared 0141): written with
--     uuid NULL (internal/nativeauth/store.go, createFamily) and referenced by
--     elitea_auth.native_sessions.token_id. Both tests are applied.
--   project system tokens: every token of a project's system identity, the
--     account `system_user_<project id>@centry.user` that
--     GetActiveProjectSystemPAT (internal/db/queries/auth_pat.sql) joins on.
--
-- NEW ROWS take the column default `none`. The three insert sites (personal
-- access tokens, native device sessions, the project system token) do not
-- name the columns, and a replica of the previous release, which does not
-- know them, writes `none` too during a rolling upgrade.
--
-- RE-RUN. The backfill runs only in the run that adds admin_access. A second
-- run finds the column, changes nothing and raises a notice: otherwise it
-- would widen a token created after the upgrade whose owner was given a role
-- since, which is exactly what `none` exists to prevent.
--
-- GUARD. The shared corpus also runs on databases without the auth_core
-- tables; there the file raises a notice and returns. auth_core__token.expires
-- stays TIMESTAMP without time zone (not in ADR-0059). No BEGIN/COMMIT: the
-- ledgered runner executes each file inside one transaction with its ledger
-- row (migrate/runner.go apply).

DO $$
DECLARE
    already_present boolean;
    system_clause   text := '';
    session_clause  text := '';
    backfilled      bigint;
BEGIN
    IF to_regclass('public.auth_core__token') IS NULL THEN
        RAISE NOTICE '0265: public.auth_core__token absent, no token administration access to add';
        RETURN;
    END IF;

    SELECT EXISTS (
        SELECT 1 FROM pg_attribute
        WHERE attrelid = 'public.auth_core__token'::regclass
          AND attname = 'admin_access' AND attnum > 0 AND NOT attisdropped
    ) INTO already_present;

    ALTER TABLE public.auth_core__token
        ADD COLUMN IF NOT EXISTS admin_access text NOT NULL DEFAULT 'none',
        ADD COLUMN IF NOT EXISTS admin_scopes text[] NOT NULL DEFAULT '{}';

    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                   WHERE conrelid = 'public.auth_core__token'::regclass
                     AND conname = 'auth_core__token_admin_access_check') THEN
        ALTER TABLE public.auth_core__token
            ADD CONSTRAINT auth_core__token_admin_access_check
            CHECK (admin_access IN ('none', 'scoped', 'inherit'));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                   WHERE conrelid = 'public.auth_core__token'::regclass
                     AND conname = 'auth_core__token_admin_scopes_check') THEN
        ALTER TABLE public.auth_core__token
            ADD CONSTRAINT auth_core__token_admin_scopes_check
            CHECK (admin_access = 'scoped' OR cardinality(admin_scopes) = 0);
    END IF;

    COMMENT ON COLUMN public.auth_core__token.admin_access IS
        'Administration access of this credential (ADR-0059 decision 7): none (default), scoped (only admin_scopes) or inherit (the owner''s set until the sunset date). Not read before R1.2a.';
    COMMENT ON COLUMN public.auth_core__token.admin_scopes IS
        'Administration permission names of a scoped token (ADR-0059 decision 7). Empty unless admin_access is scoped.';

    IF already_present THEN
        RAISE NOTICE '0265: admin_access already present, backfill skipped';
        RETURN;
    END IF;

    IF to_regclass('public.auth_core__user_role') IS NULL
       OR to_regclass('public.auth_core__role') IS NULL
       OR to_regclass('public.auth_core__role_permission') IS NULL
       OR to_regclass('public.auth_core__user') IS NULL THEN
        RAISE NOTICE '0265: auth_core role tables absent, every token stays none';
        RETURN;
    END IF;

    -- The two exclusions name tables a database may lack; each is added only
    -- where its table exists (no table, no such row).
    IF to_regclass('elitea_auth.native_sessions') IS NOT NULL THEN
        session_clause := '
          AND NOT EXISTS (SELECT 1 FROM elitea_auth.native_sessions AS device
                          WHERE device.token_id = token.id)';
    END IF;
    IF to_regclass('centry.project') IS NOT NULL THEN
        system_clause := '
          AND NOT EXISTS (SELECT 1
                          FROM public.auth_core__user AS owner
                          JOIN centry.project AS project
                            ON owner.email = (''system_user_'' || project.id::text || ''@centry.user'')
                          WHERE owner.id = token.user_id)';
    END IF;

    EXECUTE '
        UPDATE public.auth_core__token AS token
        SET admin_access = ''inherit''
        WHERE token.uuid IS NOT NULL
          AND token.admin_access = ''none''
          AND EXISTS (SELECT 1
                      FROM public.auth_core__user_role AS assignment
                      JOIN public.auth_core__role AS role
                        ON role.id = assignment.role_id
                       AND role.mode = ''administration''
                      JOIN public.auth_core__role_permission AS role_grant
                        ON role_grant.role_id = role.id
                      WHERE assignment.user_id = token.user_id)'
        || session_clause || system_clause;
    GET DIAGNOSTICS backfilled = ROW_COUNT;
    RAISE NOTICE '0265: % personal access token(s) backfilled to inherit', backfilled;
END
$$;
