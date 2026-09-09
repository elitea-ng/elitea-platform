-- 0123_webhook_delivery_blocked_status.sql — adds 'blocked' to
-- webhook_deliveries.status (SSRF hardening for #876's outbound webhooks).
--
-- WHY THIS FILE EXISTS. internal/api/webhook/dispatcher.go now runs every
-- destination through a DestinationGuard (ssrf.go) both when a webhook is
-- created/updated (Handler.validateDestination, 400 on refusal — so an
-- unchecked row should never reach this table) and again at SEND time
-- (dialContext, re-resolving DNS immediately before every dial and pinning
-- the connection to the resolved IP literal — defence in depth against a row
-- smuggled in some other way, a hostname repointed after creation, or DNS
-- rebinding between the two checks). A destination the guard refuses at send
-- time is logged as a delivery, the same as a success or an ordinary
-- failure, so an operator watching the "Recent deliveries" panel can SEE that
-- a webhook is misconfigured rather than watching it silently do nothing.
--
-- 'blocked' is a THIRD outcome, distinct from 'failed': a failed delivery is
-- one the destination itself rejected or timed out on, which retrying or
-- redelivering might fix; a blocked delivery refuses identically every time
-- (the destination is loopback, private, link-local or multicast, and stays
-- that way until the webhook's `url` is corrected), so
-- internal/api/webhook/dispatcher.go's attempt() does not retry it and the
-- web client's "Redeliver" action is disabled for it (WebhookDeliveriesPanel.
-- tsx). webhook_deliveries_status_check (0122) is CHECK, not an enum, for the
-- same reason 0122's own header gives — pipeline_schedules.last_result uses
-- the identical discipline — so the fix is a DROP + ADD CONSTRAINT rather
-- than an ALTER TYPE.

ALTER TABLE public.webhook_deliveries
    DROP CONSTRAINT IF EXISTS webhook_deliveries_status_check;

ALTER TABLE public.webhook_deliveries
    ADD CONSTRAINT webhook_deliveries_status_check
        CHECK (status IN ('pending', 'success', 'failed', 'blocked'));
