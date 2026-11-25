-- 0278_token_lifecycle_inherit_notice.sql — when the owner of an `inherit`
-- token was told that its administration access ends (ADR-0059 decision 2,
-- "Token owners get 30 days from delivery", decision 7, "`inherit`"; story
-- R3.15b, issue 2619).
--
-- An `inherit` token ends at the later of the sunset date and 30 days after
-- its owner's notice was delivered in this deployment. The ADR says the
-- delivery time is recorded and names no column; this is it:
--
--   inherit_notice_delivered_at  when the mailer accepted the notice that
--                                listed this token (the sender stamps it in
--                                the transaction that marks the outbox row of
--                                0277 sent). NULL while nothing was
--                                delivered: a failed send leaves it NULL and
--                                the next pass retries. Written once; a later
--                                notice never moves it. Story R4.2 (issue
--                                2548) reads it to end the token.
--
-- `notified_at` of 0125 is the EXPIRY notice's and is not reused.
--
-- ADD COLUMN with no default is a catalogue-only change. Additive only.
-- Idempotent. 0125 creates the table on every database (the foreign key is
-- the part it guards), so this file needs no guard. No BEGIN/COMMIT: the
-- ledgered runner wraps the file.

ALTER TABLE elitea_identity.token_lifecycle
    ADD COLUMN IF NOT EXISTS inherit_notice_delivered_at timestamptz;

COMMENT ON COLUMN elitea_identity.token_lifecycle.inherit_notice_delivered_at IS
    'ADR-0059 R3.15b: when the inherit-token notice listing this token was delivered; the token ends at the later of the sunset date and this plus 30 days.';
