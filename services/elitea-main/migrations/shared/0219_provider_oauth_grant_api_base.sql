-- 0219_provider_oauth_grant_api_base.sql — the provider API base of an
-- Atlassian 3LO grant (ADR-0039, issue #1464).
--
-- WHY. Atlassian accepts a 3LO access token only at
-- https://api.atlassian.com/ex/{jira|confluence}/{cloudid}, never at the site
-- URL. Main reads the site's cloudid from the accessible-resources endpoint
-- once, at the consent callback, and keeps the API base with the grant. The
-- credential broker releases it with the access token (material key
-- `api_base`), and the worker calls that base for an OAUTH_ACCESS envelope.
--
--   elitea_identity.provider_oauth_grants.api_base_url
--                             NULL for a provider with no API gateway
--                             (Aha!), and for an Atlassian grant consented
--                             before this column existed: the broker then
--                             denies the use until the user consents again.
--                             Never a token.
--
-- Additive only: one nullable column with a CHECK that NULL passes. An older
-- Main never reads or writes it. Idempotent. No BEGIN/COMMIT: the ledgered
-- runner wraps the file. No data, no permission.

ALTER TABLE elitea_identity.provider_oauth_grants
    ADD COLUMN IF NOT EXISTS api_base_url text
        CHECK (api_base_url IS NULL OR length(api_base_url) BETWEEN 1 AND 2048);
