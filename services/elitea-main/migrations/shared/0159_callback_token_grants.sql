-- 0159_callback_token_grants.sql — what a provider callback token was minted
-- FOR (ADR-0027 P4, Inventory `investigate`).
--
-- WHY. A provider callback token (v2auth.CallbackTokenMinter) is an ordinary
-- auth_core__token row with an expiry and a project binding. Nothing on the
-- row says which invocation asked for it, and the name is not evidence: any
-- user can create a personal access token with any name, bound to a project,
-- with an expiry. So a route that wants to treat a callback bearer
-- differently from a PAT needs a record only the minting facade writes.
--
-- The one route that does today is test_tool. The native Inventory engine's
-- `investigate` agent calls the SOURCE toolkits' read-only tools through
-- `POST /api/v2/elitea_core/test_tool/prompt_lib/{p}/{toolkit}` with the
-- callback bearer of its invocation. test_tool takes
-- `models.applications.tool.patch` (an edit of the toolkit), so a user who may
-- chat but not edit toolkits got 403 on every source call. With a row here
-- naming the token, the tool and the source toolkits, that one call takes
-- `models.applications.tool.execute` instead (inventory.SourceToolGate).
--
-- WHY A SIDE TABLE, NOT A COLUMN: auth_core__token is pylon's, for the reason
-- 0071's header gives. ON DELETE CASCADE removes the row when the token goes
-- (a revoked grant, the expiry sweep), so no delete path can forget it.
--
-- source_toolkit_ids is a SNAPSHOT of the invoking toolkit's `sources` taken
-- when the token is minted. The token lives minutes; a source removed from
-- the toolkit after that is refused by the next invocation's snapshot.
--
-- No permission, no backfill (no existing token was minted with a purpose).
-- Idempotent throughout. No BEGIN/COMMIT: the ledgered runner wraps the file.

CREATE SCHEMA IF NOT EXISTS elitea_identity;

CREATE TABLE IF NOT EXISTS elitea_identity.callback_token_grant (
    -- One purpose per token: a callback token is minted per invocation.
    token_id           integer PRIMARY KEY,
    -- The facade that minted it ('inventory') and the tool it was minted for
    -- ('investigate').
    provider           text NOT NULL,
    tool               text NOT NULL,
    -- The project the token is bound to, restated so the read can require
    -- both to agree.
    project_id         integer NOT NULL,
    -- The invoking toolkit (Inventory: configuration.application_id).
    owner_toolkit_id   integer NOT NULL,
    -- The source toolkits the invocation may call tools of.
    source_toolkit_ids integer[] NOT NULL DEFAULT '{}',
    created_at         timestamptz NOT NULL DEFAULT now()
);

DO $$
BEGIN
    IF to_regclass('public.auth_core__token') IS NULL THEN
        RAISE NOTICE '0159: auth_core absent, callback grant foreign key skipped';
        RETURN;
    END IF;
    IF EXISTS (
        SELECT 1
        FROM pg_constraint
        WHERE conname = 'callback_token_grant_token_id_fkey'
          AND conrelid = 'elitea_identity.callback_token_grant'::regclass
    ) THEN
        RETURN;
    END IF;
    ALTER TABLE elitea_identity.callback_token_grant
        ADD CONSTRAINT callback_token_grant_token_id_fkey
        FOREIGN KEY (token_id)
        REFERENCES public.auth_core__token (id)
        ON DELETE CASCADE;
END
$$;
