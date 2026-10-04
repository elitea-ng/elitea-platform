-- Let an inbound pipeline trigger be a GitLab webhook (legacy issue 6664).
--
-- WHAT WAS MISSING. 0138 gave a trigger two credential modes: the bearer
-- secret (`token`) and a raw-body HMAC (`hmac_sha256`, what GitHub sends). A
-- GitLab webhook can send neither the way 0138 reads them:
--
--   * with a SECRET TOKEN, GitLab sends the secret verbatim in
--     `X-Gitlab-Token`. That is the `token` mode with a fourth carrier, and
--     the carrier is a code change only. What the schema has to allow is the
--     `gitlab` PROVIDER, so the URL a GitLab trigger is given ends in
--     `/gitlab` and survives a re-read;
--   * with a SIGNING TOKEN, GitLab signs per the Standard Webhooks
--     specification: HMAC-SHA256 of `webhook-id.webhook-timestamp.body`, sent
--     as `webhook-signature: v1,<base64>`. That is a third MODE,
--     `standard_webhooks_hmac`, and its signature header is always
--     `webhook-signature`.
--
-- WHY DROP AND RE-ADD. A CHECK constraint cannot be altered in place. Each one
-- is dropped and re-created with the wider vocabulary inside this one
-- statement, so no committed state lets an unknown value in.
--
-- The signature-header rule widens from "hmac_sha256 needs a header" to
-- "every mode except token needs a header", which is the rule the Go side
-- states (authmode.go modeSigns).
--
-- Same guard as 0138: a tenant schema that has not yet reached 0133 has no
-- table here, and an unguarded ALTER on a missing relation raises 42P01.
DO $$
BEGIN
    IF to_regclass('pipeline_triggers') IS NULL THEN
        RETURN;
    END IF;

    ALTER TABLE pipeline_triggers
        DROP CONSTRAINT IF EXISTS pipeline_triggers_auth_mode_check;
    ALTER TABLE pipeline_triggers
        ADD CONSTRAINT pipeline_triggers_auth_mode_check
        CHECK (auth_mode IN ('token', 'hmac_sha256', 'standard_webhooks_hmac'));

    ALTER TABLE pipeline_triggers
        DROP CONSTRAINT IF EXISTS pipeline_triggers_provider_check;
    ALTER TABLE pipeline_triggers
        ADD CONSTRAINT pipeline_triggers_provider_check
        CHECK (provider IN ('custom', 'github', 'gitlab'));

    ALTER TABLE pipeline_triggers
        DROP CONSTRAINT IF EXISTS pipeline_triggers_signature_header_check;
    ALTER TABLE pipeline_triggers
        ADD CONSTRAINT pipeline_triggers_signature_header_check
        CHECK (auth_mode = 'token' OR signature_header <> '');
END
$$;
