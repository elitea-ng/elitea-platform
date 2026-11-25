-- 0280_admin_access_notice_webhook.sql — the out-of-band webhook channel of
-- the break-glass notices (ADR-0059 decision 5, "Loud"; story R2.5b, issue
-- 2598).
--
-- E-mail can be down at the moment break-glass is used, so a deployment that
-- configures the notice webhook gets a second channel in the same outbox
-- (shared 0277): one row of channel `webhook` per event, written with the
-- e-mail rows IN THE TRANSACTION OF THE EVENT, and delivered by the same
-- sender with its own retries. A webhook row has no recipient person: the
-- webhook is the operator's endpoint, configured from a Secret at start, and
-- its payload names no person. So:
--
--   channel             gains `webhook`;
--   recipient_user_id   may be NULL, for a webhook row only;
--   recipient_email     is empty on a webhook row, and required on every
--                       other row, as before;
--   kind                a webhook row is one of the five events the webhook
--                       reports (an activation, a critical change, an overdue
--                       review, the move to unreviewed_no_reviewer, an
--                       `unauthorized` review): never a request, which goes
--                       to the approvers, nor the `inherit` token notice.
--
-- The webhook URL itself is not stored anywhere: it carries the receiver's
-- token, so it is a credential, read from the environment of Main only.
--
-- IDEMPOTENT: each constraint is dropped if present and added again, so a
-- re-run leaves the same table. No BEGIN/COMMIT: the ledgered runner wraps
-- the file.

DO $$
BEGIN
IF to_regclass('elitea_auth.admin_access_notices') IS NULL THEN
    RAISE NOTICE '0280: elitea_auth.admin_access_notices absent, nothing to change';
    RETURN;
END IF;

ALTER TABLE elitea_auth.admin_access_notices
    ALTER COLUMN recipient_user_id DROP NOT NULL;

ALTER TABLE elitea_auth.admin_access_notices
    DROP CONSTRAINT IF EXISTS admin_access_notices_channel_known,
    DROP CONSTRAINT IF EXISTS admin_access_notices_recipient_present,
    DROP CONSTRAINT IF EXISTS admin_access_notices_webhook_kind;

ALTER TABLE elitea_auth.admin_access_notices
    ADD CONSTRAINT admin_access_notices_channel_known
        CHECK (channel IN ('email', 'bell', 'webhook')),
    -- The comparisons are written so that NULL cannot pass.
    ADD CONSTRAINT admin_access_notices_recipient_present
        CHECK (CASE WHEN channel = 'webhook'
                    THEN recipient_user_id IS NULL AND recipient_email = ''
                    ELSE COALESCE(recipient_user_id > 0 AND length(btrim(recipient_email)) > 0, false)
               END),
    ADD CONSTRAINT admin_access_notices_webhook_kind
        CHECK (channel <> 'webhook'
               OR kind IN ('activation', 'critical_change', 'review_overdue',
                           'unreviewed_no_reviewer', 'review_unauthorized'));
END
$$;

COMMENT ON COLUMN elitea_auth.admin_access_notices.recipient_user_id IS
    'ADR-0059 R2.5a/R2.5b: the person told; NULL on a webhook row, which names no person.';
