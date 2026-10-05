-- Remember the signed deliveries an inbound pipeline trigger has admitted, so a
-- replay of one starts no second run (PR #1027 review).
--
-- WHAT WAS MISSING. A signing trigger verifies that a delivery is authentic;
-- nothing recorded that it had already been USED.
--
--   * `standard_webhooks_hmac` (a GitLab signing token) bounds a delivery's
--     timestamp to five minutes either side of the server clock. Inside that
--     window the same signed request could be POSTed again and again, and
--     each copy was admitted as a separate run. GitLab's own retry of a
--     delivery that timed out (same `webhook-id`) double-ran the same way.
--   * `hmac_sha256` (GitHub) signs no timestamp at all, so a captured
--     delivery stayed valid for the life of the secret.
--
-- WHAT THE KEY IS. `delivery_key` is a SHA-256 hex digest of the SIGNED
-- identity of the delivery: the `webhook-id` for Standard Webhooks (the
-- specification's idempotency key, and part of the signed content), and the
-- raw body for `hmac_sha256` (the only thing GitHub signs; `X-GitHub-Delivery`
-- is not signed, so an attacker could change it freely). It is scoped by
-- `token_id`, so a rotation starts a fresh set.
--
-- WHAT A ROW HOLDS. The run the first copy admitted. A replay is answered with
-- that run's ids (202, idempotent) instead of a second run. A row whose
-- `execution_id` is still NULL is a delivery being admitted right now; one
-- whose admission failed is deleted, so the sender's retry is a first try.
--
-- Rows are pruned by age from the inbound path itself; `received_at` is
-- indexed for that delete. No permission, so no shared sibling.
CREATE TABLE IF NOT EXISTS pipeline_trigger_deliveries (
    token_id varchar(64) NOT NULL,
    delivery_key char(64) NOT NULL,
    received_at timestamptz NOT NULL DEFAULT now(),
    execution_id varchar(255),
    conversation_uuid uuid,
    version_id integer,

    CONSTRAINT pipeline_trigger_deliveries_pkey PRIMARY KEY (token_id, delivery_key)
);

CREATE INDEX IF NOT EXISTS pipeline_trigger_deliveries_received_at_idx
    ON pipeline_trigger_deliveries (received_at);
