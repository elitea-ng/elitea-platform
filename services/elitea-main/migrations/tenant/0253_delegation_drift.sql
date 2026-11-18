-- 0253_delegation_drift.sql — drift of a delegation grant's callee
-- (ADR-0044 amendment 1, A1.2 rule 3; delegation epic 2156 story S4).
--
-- A callee X that holds more than its grant lists makes the grant `stale`
-- (the state exists since 0252). This records WHAT changed, remembers the
-- closure of an approved edge as of the last check, and keeps one row per
-- notification so the owners and editors of the caller are told exactly once
-- per edge and digest, even when the notification itself is deleted from the
-- bell.
--
--   drift            {callee_version_id, digest, added: [item]}: the items the
--                    callee holds now that the grant does not cover. NULL on a
--                    grant that is not stale.
--   closure          {application id: digest of the grant into it}: the
--                    callees X reaches through approved grants, as of the last
--                    check. NULL until the first check. A callee or digest that
--                    appears later is closure growth: a notification, no state
--                    change.
--   delegation_drift_notices
--                    the dedup record. (caller, callee, edge kind, notice kind,
--                    digest) is the key: `stale` keys on the digest of the
--                    callee's current set, `closure` on the closure's key.
--
-- Guarded (idempotent): the runner pins search_path to the tenant schema.
-- No BEGIN/COMMIT.
ALTER TABLE delegation_grants ADD COLUMN IF NOT EXISTS drift jsonb;
ALTER TABLE delegation_grants ADD COLUMN IF NOT EXISTS closure jsonb;

CREATE TABLE IF NOT EXISTS delegation_drift_notices (
    caller_entity_id integer NOT NULL,
    callee_entity_id integer NOT NULL,
    edge_kind text NOT NULL,
    notice_kind text NOT NULL CONSTRAINT delegation_drift_notices_kind_check CHECK (notice_kind IN ('stale', 'closure')),
    digest text NOT NULL,
    notified_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (caller_entity_id, callee_entity_id, edge_kind, notice_kind, digest)
);
