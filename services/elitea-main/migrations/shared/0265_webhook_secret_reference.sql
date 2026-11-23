-- 0265_webhook_secret_reference.sql — webhook signing secrets move to the
-- project vault (issue 2963).
--
-- 0122 stored each outbound webhook's signing secret in public.webhooks.secret
-- as plain text. This migration adds the column that holds the vault
-- REFERENCE instead:
--
--   public.webhooks.secret_ref
--       the `{{secret.NAME}}` reference to a hidden secret in the webhook's
--       project vault, or '' when the webhook has no signing secret. The
--       dispatcher resolves it per delivery; no read route returns it or the
--       value it names.
--
-- THE PLAINTEXT IS MOVED IN GO, NOT HERE. A vault value is sealed with the
-- project's Fernet key, which is itself wrapped with SECRETS_MASTER_KEY, so SQL
-- cannot write a value the readers can open. elitea-main's start-up pass
-- (repos.WebhooksRepo.MoveLegacySecretsToVault) seals every non-empty
-- `secret` into the vault, writes the reference here and blanks `secret`, one
-- row per transaction. It is idempotent, and until it reaches a row the
-- dispatcher keeps signing that row with its plaintext, so no delivery goes out
-- unsigned during an upgrade. The `secret` column stays (0122 is released) and
-- is empty for every row the pass has reached.
--
-- NOT NULL DEFAULT '' so a replica of the previous release, which does not
-- name the column, keeps inserting rows during a rolling upgrade.

ALTER TABLE public.webhooks
    ADD COLUMN IF NOT EXISTS secret_ref text NOT NULL DEFAULT '';
