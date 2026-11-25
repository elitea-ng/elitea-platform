-- 0277_admin_access_notices.sql — the outbox of the break-glass notices
-- (ADR-0059 decision 5, "Loud" and "The mailer is down"; story R2.5a, issue
-- 2519) and of the notice to the owners of `inherit` tokens (decision 2,
-- "Token owners get 30 days from delivery"; story R3.15b, issue 2619).
--
-- A request that nobody sees expires unseen, and an activation that nobody
-- hears of is not loud. So the step that matters writes its notices as rows
-- here, IN THE TRANSACTION OF THE STEP (adminaccess.WriteElevationNotices),
-- and a sender delivers them afterwards (adminaccess.NoticeSender, the
-- main.access_notice_sender routine). A mailer failure never blocks or rolls
-- back the step: only the security audit event fails closed. A row that
-- cannot be sent stays here, unsent, with its attempts and its last error,
-- and is retried with a growing delay; nothing drops it.
--
-- One row per recipient and channel. The recipient is resolved when the row
-- is WRITTEN, from standing roles, and the address is copied: a person whose
-- role is taken away afterwards still gets the notice of the event that
-- happened while they held it.
--
--   kind              which notice (the CHECK lists them).
--   channel           `email`. `bell` is reserved for the in-application
--                     notice that waits for issue 2485 (a notification with
--                     no project); no writer uses it yet.
--   elevation_id      the elevation the notice is about; NULL for the
--                     `inherit` token notice.
--   recipient_*       who is told, as resolved at write time.
--   include_reason    the recipient may read the reason text in the console
--                     (an approver or a reviewer). The text itself is not
--                     copied here: the sender reads it from the ledger, and
--                     only for such a row. It never goes into a subject.
--   actor_user_id     who acted (the reviewer of an `unauthorized` outcome,
--                     the person behind a critical change); NULL for the
--                     platform.
--   detail            what the kind needs besides the elevation: the action
--                     of a critical change, the tokens of an `inherit` notice
--                     and its sunset date. Ids, names and codes only.
--   dedupe_key        a writer that can run twice for one fact (the `inherit`
--                     pass runs every sweep) names the fact; the partial
--                     unique index keeps one row.
--   next_attempt_at   when the sender may take the row. A sender claims a row
--                     by moving it forward by a lease, under FOR UPDATE SKIP
--                     LOCKED, so two replicas never take the same row at once.
--   attempts,
--   last_error        what the sender tried; the error text is the transport's
--                     message, bounded, with no credential in it.
--   sent_at           when the mailer accepted the message. NULL is pending.
--
-- NO FOREIGN KEY to auth_core__user (as 0269): the shared corpus also runs on
-- databases without the auth_core tables, and the notice outlives the
-- account. The foreign key to the ledger is inside this corpus (0269).
--
-- Every time column is timestamptz. IDEMPOTENT throughout. No BEGIN/COMMIT:
-- the ledgered runner executes each file inside one transaction with its
-- ledger row (migrate/runner.go apply).

CREATE SCHEMA IF NOT EXISTS elitea_auth;

CREATE TABLE IF NOT EXISTS elitea_auth.admin_access_notices (
    id                uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    kind              text        NOT NULL,
    channel           text        NOT NULL DEFAULT 'email',
    elevation_id      uuid        REFERENCES elitea_auth.admin_elevations (id) ON DELETE CASCADE,
    recipient_user_id bigint      NOT NULL,
    recipient_email   text        NOT NULL,
    recipient_name    text        NOT NULL DEFAULT '',
    include_reason    boolean     NOT NULL DEFAULT false,
    actor_user_id     bigint,
    detail            jsonb       NOT NULL DEFAULT '{}'::jsonb,
    dedupe_key        text,
    created_at        timestamptz NOT NULL,
    next_attempt_at   timestamptz NOT NULL,
    attempts          integer     NOT NULL DEFAULT 0,
    last_error        text,
    sent_at           timestamptz,
    CONSTRAINT admin_access_notices_kind_known
        CHECK (kind IN ('request', 'activation', 'critical_change', 'review_overdue',
                        'unreviewed_no_reviewer', 'review_unauthorized', 'inherit_token')),
    CONSTRAINT admin_access_notices_channel_known
        CHECK (channel IN ('email', 'bell')),
    CONSTRAINT admin_access_notices_elevation_present
        CHECK ((kind = 'inherit_token') = (elevation_id IS NULL)),
    CONSTRAINT admin_access_notices_recipient_present
        CHECK (recipient_user_id > 0 AND length(btrim(recipient_email)) > 0),
    CONSTRAINT admin_access_notices_attempts_counted
        CHECK (attempts >= 0)
);

-- The sender's queue: the unsent rows, by when they are due.
CREATE INDEX IF NOT EXISTS admin_access_notices_due_idx
    ON elitea_auth.admin_access_notices (next_attempt_at, created_at)
    WHERE sent_at IS NULL;

-- The notices of one elevation, for its review.
CREATE INDEX IF NOT EXISTS admin_access_notices_elevation_idx
    ON elitea_auth.admin_access_notices (elevation_id)
    WHERE elevation_id IS NOT NULL;

CREATE UNIQUE INDEX IF NOT EXISTS admin_access_notices_dedupe_idx
    ON elitea_auth.admin_access_notices (dedupe_key)
    WHERE dedupe_key IS NOT NULL;

COMMENT ON TABLE elitea_auth.admin_access_notices IS
    'ADR-0059 R2.5a/R3.15b: notice outbox, one row per recipient and channel, written with the event; sent_at NULL is pending.';
