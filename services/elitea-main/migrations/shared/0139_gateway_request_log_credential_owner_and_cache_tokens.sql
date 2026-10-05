-- 0139_gateway_request_log_credential_owner_and_cache_tokens.sql
--
-- Three columns for gateway.llm_request_logs (0099): who OWNS the provider
-- credential that served a request, and the cache-read and cache-write token
-- counts the provider reported.
--
-- ## Which history this belongs to
--
-- SHARED, for the reason 0100 gives: `gateway` is one schema in the shared
-- database, and 0099 creates this table in this same history. Nothing here
-- claims an object another component owns.
--
-- ## credential_owner (legacy issue 6709)
--
-- A request is served either by a credential the CALLING project configured
-- (a client-owned model) or by a credential the platform published from its
-- public project with `shared = true` (an ELITEA-managed model). The two must
-- be told apart on the Usage and Analytics pages, and nothing recorded the
-- difference: the log kept the model name, and the same model name can be
-- served from either scope.
--
-- The gateway writes one of three values:
--
--   'project'   a credential of the calling project served the request.
--   'platform'  a shared credential of the public project served it.
--   ''          unknown: the request failed before a credential served it,
--               or the row predates this migration.
--
-- It is a CLASSIFICATION the gateway assigns, never a credential id, a name
-- or a secret. The column is not a place a caller can put a string.
--
-- ## cache_read_tokens and cache_write_tokens (legacy issue 6709)
--
-- Providers bill prompt-cache reads and writes at their own rates. The log
-- recorded only prompt and completion counts, so a project that leaned on the
-- cache could not see how much of its prompt it read from it. The gateway takes
-- both counts from the provider's usage block. They are SUBSETS of
-- prompt_tokens, not additions to it: summing all three counts a cached token
-- twice. Zero is the honest value for a provider that reports no cache usage.
--
-- ## Not backfilled
--
-- Nothing on an existing row records the owner of the credential or the
-- cache split, so there is nothing to backfill. The defaults ('' and 0) are
-- the honest values for a row written before this file.
--
-- IDEMPOTENCE, as in 0099 and 0100: every statement is guarded, because dev and
-- dump-loaded databases reach this file in several different states. The
-- defaults are constants, so PostgreSQL adds each column without a table
-- rewrite.
--
-- No BEGIN/COMMIT: the ledgered runner wraps each file in one transaction with
-- its ledger row (migrate/runner.go apply).

ALTER TABLE gateway.llm_request_logs ADD COLUMN IF NOT EXISTS credential_owner VARCHAR(16) NOT NULL DEFAULT '';
ALTER TABLE gateway.llm_request_logs ADD COLUMN IF NOT EXISTS cache_read_tokens BIGINT NOT NULL DEFAULT 0;
ALTER TABLE gateway.llm_request_logs ADD COLUMN IF NOT EXISTS cache_write_tokens BIGINT NOT NULL DEFAULT 0;

COMMENT ON COLUMN gateway.llm_request_logs.credential_owner IS
    'Who owns the credential that served the request: project, platform, or empty when unknown.';
COMMENT ON COLUMN gateway.llm_request_logs.cache_read_tokens IS
    'Prompt tokens the provider read from its cache. A subset of prompt_tokens.';
COMMENT ON COLUMN gateway.llm_request_logs.cache_write_tokens IS
    'Prompt tokens the provider wrote to its cache. A subset of prompt_tokens.';
