-- Give an inbound pipeline trigger a PROVIDER SIGNATURE mode.
--
-- WHAT WAS MISSING. 0133 gives every trigger exactly one credential shape: a
-- bearer secret, presented in one of three carriers and compared against the
-- `token_hash` digest beside the row. That shape cannot be produced by the
-- senders people actually have. A GitHub repository webhook sends no
-- `Authorization` header and cannot be made to: it signs the RAW BODY with a
-- shared secret and sends `X-Hub-Signature-256: sha256=<hex>`. The same is
-- true of most managed webhook producers. So a pipeline could not be started
-- from a repository webhook at all without an intermediary that re-signs the
-- call, which is the whole thing the trigger exists to remove.
--
-- WHY THREE COLUMNS AND NOT ONE jsonb BLOB. This table has no json column to
-- extend, and each of these three is read on the inbound path — the one path
-- in this service where a wrong read is worst. A blob would put the mode
-- behind a cast and an `->>`, where a typo reads as NULL and NULL would have
-- to mean "token" to stay compatible; a column with a DEFAULT says the same
-- thing once, in the schema, and cannot be misspelled at the read.
--
--   * `auth_mode` — how the inbound call is authenticated. `token` is every
--     existing row and the default for every new one, so this migration
--     changes NO trigger's behaviour on its own.
--   * `signature_header` — the header the provider signs into. Empty for
--     `token`. Stored per trigger rather than derived from `provider`,
--     because a generic HMAC sender is a real case (`provider = 'custom'`
--     with the sender's own header name) and deriving it would make that
--     case unreachable.
--   * `provider` — the URL SUFFIX and nothing else. A GitHub webhook's
--     configuration UI is happier with a URL that ends in `/github`, and the
--     suffix has to survive a re-read to be rendered again. It is NOT what
--     decides the validation: the stored `auth_mode` is (package rule 1 —
--     nothing the caller sends selects what happens).
--
-- WHY `CHECK` ON BOTH VOCABULARIES. Each value has one branch in Go, and a
-- value with no branch would fall through to "not a signature mode", i.e. to
-- the bearer path — a mode-confusion failure that turns a stricter setting
-- into a weaker one. The database refuses the value instead.
--
-- WHERE THE SECRET IS. Still the vault. An HMAC cannot be verified from a
-- digest, so the inbound path DOES open the hidden bucket for a
-- `hmac_sha256` trigger — stated here because 0133's header says the inbound
-- path never opens the vault, and that sentence is now true of the `token`
-- mode only. A vault that will not open answers "the trigger could not be
-- checked" (503), never the credential refusal: a deployment fault must not
-- tell a sender their secret is wrong.
--
-- The guard is 0134's: a tenant schema that has not yet reached 0133 has no
-- table here, and an unguarded ALTER on a missing relation raises 42P01 and
-- fails the whole chain.
DO $$
BEGIN
    IF to_regclass('pipeline_triggers') IS NULL THEN
        RETURN;
    END IF;

    ALTER TABLE pipeline_triggers
        ADD COLUMN IF NOT EXISTS auth_mode varchar(32) NOT NULL DEFAULT 'token';
    ALTER TABLE pipeline_triggers
        ADD COLUMN IF NOT EXISTS signature_header varchar(128) NOT NULL DEFAULT '';
    ALTER TABLE pipeline_triggers
        ADD COLUMN IF NOT EXISTS provider varchar(32) NOT NULL DEFAULT 'custom';

    -- Added separately from the columns, and guarded, so a re-run is a no-op
    -- rather than a duplicate-object error.
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = to_regclass('pipeline_triggers')
          AND conname = 'pipeline_triggers_auth_mode_check'
    ) THEN
        ALTER TABLE pipeline_triggers
            ADD CONSTRAINT pipeline_triggers_auth_mode_check
            CHECK (auth_mode IN ('token', 'hmac_sha256'));
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = to_regclass('pipeline_triggers')
          AND conname = 'pipeline_triggers_provider_check'
    ) THEN
        ALTER TABLE pipeline_triggers
            ADD CONSTRAINT pipeline_triggers_provider_check
            CHECK (provider IN ('custom', 'github'));
    END IF;

    -- A signature mode without a header to read it from is unusable: the
    -- inbound path would have nothing to look at and would refuse every call.
    -- The CHECK makes that state unstorable rather than leaving it to the
    -- writer to remember.
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = to_regclass('pipeline_triggers')
          AND conname = 'pipeline_triggers_signature_header_check'
    ) THEN
        ALTER TABLE pipeline_triggers
            ADD CONSTRAINT pipeline_triggers_signature_header_check
            CHECK (auth_mode <> 'hmac_sha256' OR signature_header <> '');
    END IF;
END
$$;
