-- 0071_token_project_binding.sql — the optional project binding of an access
-- token (ADR-0018, spec-llm-project-scope §3).
--
-- WHAT THIS ADDS. An access token may name one project. The binding decides
-- which project pays for a /llm call and whose provider credentials the call
-- spends. The binding is set when the token is created, and it is never
-- editable. The bearer string does not change, so every token that exists today
-- keeps validating byte for byte.
--
-- WHY A SIDE TABLE, NOT A COLUMN. `public.auth_core__token` belongs to the
-- legacy runtime's `auth_core` plugin. This corpus has never issued DDL against
-- it, and `internal/db/schema/auth_core_baseline.sql` mirrors it for sqlc
-- type-checking only. Two migration systems that own one table definition is
-- the failure 0064 and 0065 both describe from the other direction. A Go-owned
-- side table in `elitea_identity` references the legacy runtime table without
-- altering it.
--
-- ON DELETE CASCADE removes the binding when the token row goes. No application
-- code deletes it, so no delete path can forget.
--
-- THE FOREIGN KEY IS GUARDED, and the guard is load-bearing. elitea-migrate can
-- run before `001_initial.sql` creates auth_core — 0060, 0061 and 0062 all open
-- with the same check. A bare REFERENCES clause raises there and turns this
-- file into a hard failure on a database it has nothing to say to. The table
-- itself is still created in that case, because the read path LEFT JOINs it on
-- every credential validation and a missing relation is 42P01 for every
-- request.
--
-- NO BACKFILL. Existing tokens stay unbound (spec §3.3). A backfill would move
-- live traffic onto a different budget, and no token owner asked for that.
--
-- IDEMPOTENT throughout: IF NOT EXISTS on the schema, the table and the index,
-- and a pg_constraint probe before the ALTER TABLE.
--
-- No BEGIN/COMMIT: the ledgered runner executes each file inside one
-- transaction with its ledger row (migrate/runner.go apply).

CREATE SCHEMA IF NOT EXISTS elitea_identity;

CREATE TABLE IF NOT EXISTS elitea_identity.token_project_binding (
    -- token_id is the PRIMARY KEY, so one token binds to at most one project.
    token_id   integer PRIMARY KEY,
    -- project_id carries no foreign key. centry.project is a legacy-owned table
    -- as well, and membership is verified in the creating transaction instead.
    project_id integer NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- The revocation sweep asks "which bindings name this project", so project_id
-- must lead an index of its own.
CREATE INDEX IF NOT EXISTS ix_token_project_binding_project
    ON elitea_identity.token_project_binding (project_id);

DO $$
BEGIN
    IF to_regclass('public.auth_core__token') IS NULL THEN
        RAISE NOTICE '0070: auth_core absent, token binding foreign key skipped';
        RETURN;
    END IF;
    IF EXISTS (
        SELECT 1
        FROM pg_constraint
        WHERE conname = 'token_project_binding_token_id_fkey'
          AND conrelid = 'elitea_identity.token_project_binding'::regclass
    ) THEN
        RETURN;
    END IF;
    ALTER TABLE elitea_identity.token_project_binding
        ADD CONSTRAINT token_project_binding_token_id_fkey
        FOREIGN KEY (token_id)
        REFERENCES public.auth_core__token (id)
        ON DELETE CASCADE;
END
$$;
