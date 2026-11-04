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
--       service deletes the row. A 401 or 403 (the push service refused the
--       VAPID token) counts in rejected_sends; the third one with no success
--       between deletes the row.
--
--   elitea_runtime.web_push_queue
--       The outbox: one row per run_finished or agent_approval_required
--       notification of a user who has a subscription. An AFTER INSERT
--       trigger on centry.notifications writes it in the notification's own
--       transaction, so the row commits exactly when the notification does,
--       whoever wrote it and in whatever order the writers commit. The
--       dispatcher removes queue rows and adds one delivery per subscription
--       of the row's user, in one short transaction. The trigger fires on
--       new rows only, so an upgrade sends nothing old.
--
--   elitea_runtime.web_push_deliveries
--       One row per (notification, subscription) that waits to be sent. The
--       payload is content-free: the type and the ids. A send that fails
--       with a 429, a 5xx or no answer waits next_attempt_at (backoff) and
--       stops at expires_at (24 hours, the push TTL).
--
-- Why a PostgreSQL outbox and not NATS: Main has no NATS stream of
-- notification events, and every writer of centry.notifications (Main, the
-- scheduler and the SQL-only writers) inserts rows without a publish. The
-- trigger sees every row whoever wrote it. Not a cursor over
-- centry.notifications.id: an id is taken at insert and rows commit in
-- another order, so a cursor skips a row whose transaction commits after a
-- higher id was read.
--
-- Replicas share the sends: a sender claims due deliveries (FOR UPDATE SKIP
-- LOCKED) by moving next_attempt_at past a lease and stamping lease_id,
-- sends with no transaction open, and records the outcomes under the same
-- lease_id. A sender that dies mid-send leaves its claim to expire: those
-- deliveries go out again after the lease (at least once).
--
-- No permission: Main reads and writes these tables; the trigger runs as
-- the notification's writer, the same database role. No backfill.
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
    last_success_at timestamptz,
    -- 401 and 403 answers since the last success.
    rejected_sends  integer NOT NULL DEFAULT 0 CHECK (rejected_sends >= 0)
);

-- A user's subscriptions, the oldest success first (the 10-cap, the
-- dispatcher's fan-out and the outbox trigger).
CREATE INDEX IF NOT EXISTS push_subscriptions_user
    ON centry.push_subscriptions (user_id, last_success_at NULLS FIRST, created_at);

CREATE TABLE IF NOT EXISTS elitea_runtime.web_push_queue (
    notification_id bigint PRIMARY KEY CHECK (notification_id > 0),
    created_at      timestamptz NOT NULL DEFAULT clock_timestamp()
);

-- The outbox row of a new notification, only for a user who has a
-- subscription (everyone else's rows would only be drained).
CREATE OR REPLACE FUNCTION elitea_runtime.web_push_enqueue() RETURNS trigger
LANGUAGE plpgsql AS $body$
BEGIN
    IF EXISTS (SELECT 1 FROM centry.push_subscriptions WHERE user_id = NEW.user_id) THEN
        INSERT INTO elitea_runtime.web_push_queue (notification_id) VALUES (NEW.id)
        ON CONFLICT (notification_id) DO NOTHING;
    END IF;
    RETURN NULL;
END
$body$;

-- Guarded on centry.notifications: several suites apply the shared corpus to
-- a bare database that has no legacy-era schema (as 0144).
DO $$
BEGIN
    IF to_regclass('centry.notifications') IS NULL THEN
        RAISE NOTICE 'centry.notifications is absent; the web push outbox trigger is skipped';
        RETURN;
    END IF;
    DROP TRIGGER IF EXISTS web_push_enqueue ON centry.notifications;
    CREATE TRIGGER web_push_enqueue AFTER INSERT ON centry.notifications
        FOR EACH ROW
        WHEN (NEW.event_type IN ('run_finished', 'agent_approval_required'))
        EXECUTE FUNCTION elitea_runtime.web_push_enqueue();
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
    -- The claim of the sender that is sending it; NULL when unclaimed.
    lease_id        uuid,
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
    'Web push subscriptions (ADR-0045 decision 6): one row per opted-in browser. At most 10 per user. A 404 or 410 from the push service deletes the row, and so does the third 401 or 403 with no success between.';
COMMENT ON TABLE elitea_runtime.web_push_queue IS
    'The web push outbox (ADR-0045 decision 6): run_finished and agent_approval_required notifications of subscribed users, written by a trigger in the notification transaction.';
COMMENT ON TABLE elitea_runtime.web_push_deliveries IS
    'Web push sends that wait (ADR-0045 decision 6): one per notification and subscription. Content-free payload, claimed under a lease, backoff retry, dropped after 24 hours.';
