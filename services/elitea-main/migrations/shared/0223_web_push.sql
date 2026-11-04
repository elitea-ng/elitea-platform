-- 0223_web_push.sql — web push subscriptions and the push outbox
-- (ADR-0045 decisions 5 and 6; issue #1437).
--
--   centry.push_subscriptions
--       One row per browser that a person opted in to web push: the user,
--       the push service endpoint (https, at most 2 KiB, unique), the
--       browser's keys (p256dh and auth, base64url), the user agent and the
--       last successful send. A person has at most 10 rows: the 11th
--       subscription replaces the row with the oldest last success (Main
--       enforces it in the insert's transaction). A 404 or 410 from the push
--       service deletes the row.
--
--   elitea_runtime.web_push_cursor
--       The outbox cursor: the highest centry.notifications.id that the
--       dispatcher has read. One row. The dispatcher reads the
--       run_finished and agent_approval_required rows above it, adds one
--       delivery per subscription of the row's user, and moves the cursor,
--       in one transaction. The row starts at the newest notification, so an
--       upgrade sends nothing old.
--
--   elitea_runtime.web_push_deliveries
--       One row per (notification, subscription) that waits to be sent. The
--       payload is content-free: the type and the ids. A send that fails
--       with another status than 404 or 410 waits next_attempt_at (backoff)
--       and stops at expires_at (24 hours, the push TTL).
--
-- Why a PostgreSQL outbox and not NATS: Main has no NATS stream of
-- notification events. The bell's event stream reads centry.notifications
-- by id (notification_events), and every writer of the table (Main, the
-- scheduler and the SQL-only writers) inserts rows without a publish. A
-- cursor over the same ids sees every row whoever wrote it.
--
-- One sender: the dispatcher holds a transaction-level advisory lock for a
-- whole pass, so two replicas never send the same delivery.
--
-- No permission: Main reads and writes these tables. No backfill.
-- Idempotent. No BEGIN/COMMIT: the ledgered runner wraps the file.

CREATE SCHEMA IF NOT EXISTS centry;
CREATE SCHEMA IF NOT EXISTS elitea_runtime;

CREATE TABLE IF NOT EXISTS centry.push_subscriptions (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id         integer NOT NULL CHECK (user_id > 0),
    endpoint        text NOT NULL UNIQUE
                    CHECK (endpoint ~ '^https://' AND octet_length(endpoint) <= 2048),
    p256dh          text NOT NULL CHECK (octet_length(p256dh) BETWEEN 1 AND 256),
    auth            text NOT NULL CHECK (octet_length(auth) BETWEEN 1 AND 128),
    user_agent      text NOT NULL DEFAULT '' CHECK (octet_length(user_agent) <= 512),
    created_at      timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at      timestamptz NOT NULL DEFAULT clock_timestamp(),
    last_success_at timestamptz
);

-- A user's subscriptions, the oldest success first (the 10-cap and the
-- dispatcher's fan-out).
CREATE INDEX IF NOT EXISTS push_subscriptions_user
    ON centry.push_subscriptions (user_id, last_success_at NULLS FIRST, created_at);

CREATE TABLE IF NOT EXISTS elitea_runtime.web_push_cursor (
    id                   smallint PRIMARY KEY CHECK (id = 1),
    last_notification_id bigint NOT NULL DEFAULT 0 CHECK (last_notification_id >= 0),
    updated_at           timestamptz NOT NULL DEFAULT clock_timestamp()
);

DO $$
DECLARE
    newest bigint := 0;
BEGIN
    IF to_regclass('centry.notifications') IS NOT NULL THEN
        EXECUTE 'SELECT COALESCE(max(id), 0) FROM centry.notifications' INTO newest;
    END IF;
    INSERT INTO elitea_runtime.web_push_cursor (id, last_notification_id)
    VALUES (1, newest)
    ON CONFLICT (id) DO NOTHING;
END
$$;

CREATE TABLE IF NOT EXISTS elitea_runtime.web_push_deliveries (
    notification_id bigint NOT NULL CHECK (notification_id > 0),
    subscription_id uuid NOT NULL REFERENCES centry.push_subscriptions (id) ON DELETE CASCADE,
    -- {type, notification_id, project_id, conversation_uuid, outcome}: ids
    -- and the outcome only, never content.
    payload         jsonb NOT NULL
                    CHECK (jsonb_typeof(payload) = 'object' AND octet_length(payload::text) <= 2048),
    urgency         text NOT NULL CHECK (urgency IN ('normal', 'high')),
    attempts        integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    last_status     integer,
    next_attempt_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    expires_at      timestamptz NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (notification_id, subscription_id)
);

CREATE INDEX IF NOT EXISTS web_push_deliveries_due
    ON elitea_runtime.web_push_deliveries (next_attempt_at);
CREATE INDEX IF NOT EXISTS web_push_deliveries_subscription
    ON elitea_runtime.web_push_deliveries (subscription_id);

COMMENT ON TABLE centry.push_subscriptions IS
    'Web push subscriptions (ADR-0045 decision 6): one row per opted-in browser. At most 10 per user. A 404 or 410 from the push service deletes the row.';
COMMENT ON TABLE elitea_runtime.web_push_cursor IS
    'The web push outbox cursor (ADR-0045 decision 6): the highest centry.notifications.id the dispatcher has read.';
COMMENT ON TABLE elitea_runtime.web_push_deliveries IS
    'Web push sends that wait (ADR-0045 decision 6): one per notification and subscription. Content-free payload, backoff retry, dropped after 24 hours.';
