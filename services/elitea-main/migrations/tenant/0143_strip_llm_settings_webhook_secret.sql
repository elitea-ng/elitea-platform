-- Remove the plaintext `webhook_secret` from every agent version's
-- `llm_settings` (legacy issue 6656, review finding).
--
-- WHAT IS THERE. The agent editor had a "Webhook secret" model setting. It
-- wrote the value the person typed into `application_versions.llm_settings`
-- under `webhook_secret`, in plaintext. No server code read it, so it
-- authenticated nothing; the agent's webhook is now the inbound trigger, whose
-- secret lives in the vault. But the stored values are still there, and
-- `llm_settings` is returned as-is by every version read, so anybody holding
-- `models.applications.version.details` — and every fork or export recipient —
-- could read them. People may have reused the same value as the real secret
-- of a GitHub or GitLab webhook.
--
-- WHAT THIS DOES. Deletes the key, and nothing else. A row whose
-- `llm_settings` is not a JSON object is left alone: the `-` operator means
-- something else on an array, and raises on a scalar.
--
-- WHAT IT DOES NOT DO. It does not touch `applications.webhook_secret`. That
-- column belongs to the legacy runtime, which stores a vault REFERENCE there
-- (`unsecret(...)` in legacy predict.py), not a plaintext value, and no Go
-- code reads or returns it. It cannot un-leak a value someone already read:
-- operators should tell users to rotate any secret they reused.
--
-- The column is `jsonb` in the bootstrap schema, but a database carried over
-- from the legacy runtime may hold it as `json`, so the type is read first and
-- the matching statement runs.
--
-- Data only: idempotent, no table and no permission, so no shared sibling.
-- The guard is 0134's pattern: a tenant schema without the table or the
-- column is skipped.
DO $$
DECLARE
    column_type text;
BEGIN
    IF to_regclass('application_versions') IS NULL THEN
        RETURN;
    END IF;

    SELECT format_type(attribute.atttypid, attribute.atttypmod)
      INTO column_type
      FROM pg_attribute AS attribute
     WHERE attribute.attrelid = to_regclass('application_versions')
       AND attribute.attname = 'llm_settings'
       AND NOT attribute.attisdropped;

    IF column_type = 'jsonb' THEN
        UPDATE application_versions
           SET llm_settings = llm_settings - 'webhook_secret'
         WHERE jsonb_typeof(llm_settings) = 'object'
           AND llm_settings ? 'webhook_secret';
    ELSIF column_type = 'json' THEN
        UPDATE application_versions
           SET llm_settings = (llm_settings::jsonb - 'webhook_secret')::json
         WHERE json_typeof(llm_settings) = 'object'
           AND llm_settings::jsonb ? 'webhook_secret';
    END IF;
END
$$;
