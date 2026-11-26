-- 0283_admin_elevation_items.sql — what the review of an elevation is made of
-- (ADR-0059 decision 5, "Review after use"; story R2.9a, issue 2523).
--
-- ONE ROW PER ITEM of one elevation's review, by kind:
--
--   reviewer         a person eligible to review THIS elevation, recorded in
--                    the transaction that ended it: the standing holders of
--                    access.breakglass.review, minus the holder and the
--                    approver. Eligibility is read from these rows, never
--                    from the current roles, so a reviewer whose role is
--                    taken away afterwards can still review.
--   reviewer_change  while the review was pending, a super_admin took the
--                    review permission from an eligible reviewer, or
--                    suspended or deleted them (action: role_removed,
--                    suspended, deleted). Listed in the review.
--   review           the attestation: outcome (justified, unauthorized), the
--                    note, and for a sealed elevation whether a replacement
--                    credential was issued. One per elevation.
--   revert           after an `unauthorized` outcome, one entry per flagged
--                    change of the elevation (audit_event_id: the admin audit
--                    row of centry.audit_events), a queue for the platform
--                    administrators; handled_at/handled_by mark it done.
--   note             a later note on the review (R2.9c).
--
-- The note texts are here and nowhere else: a chained security event carries
-- the ids and codes of a review, never its text (decision 8).
--
-- APPEND-ONLY, by trigger: no DELETE, no TRUNCATE, and the one UPDATE is
-- marking a revert entry handled, once. user ids carry no foreign key: the
-- record outlives a deleted account.
--
-- Every time column is timestamptz. Idempotent. No BEGIN/COMMIT: the ledgered
-- runner wraps the file.

CREATE TABLE IF NOT EXISTS elitea_auth.admin_elevation_items (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    elevation_id        uuid NOT NULL REFERENCES elitea_auth.admin_elevations (id),
    kind                text NOT NULL,
    user_id             bigint,
    actor_user_id       bigint,
    action              text,
    outcome             text,
    note                text,
    replacement_issued  boolean,
    audit_event_id      bigint,
    created_at          timestamptz NOT NULL,
    handled_at          timestamptz,
    handled_by          bigint,

    CONSTRAINT admin_elevation_items_kind_known
        CHECK (kind IN ('reviewer', 'reviewer_change', 'review', 'revert', 'note')),
    CONSTRAINT admin_elevation_items_reviewer_names_a_person
        CHECK (kind NOT IN ('reviewer', 'reviewer_change', 'review', 'note') OR user_id IS NOT NULL),
    CONSTRAINT admin_elevation_items_reviewer_change_known
        CHECK (kind <> 'reviewer_change'
               OR (actor_user_id IS NOT NULL AND action IN ('role_removed', 'suspended', 'deleted'))),
    CONSTRAINT admin_elevation_items_outcome_known
        CHECK ((kind = 'review') = (outcome IS NOT NULL)
               AND (outcome IS NULL OR outcome IN ('justified', 'unauthorized'))),
    CONSTRAINT admin_elevation_items_revert_names_a_change
        CHECK ((kind = 'revert') = (audit_event_id IS NOT NULL)),
    CONSTRAINT admin_elevation_items_handled_is_a_revert
        CHECK (handled_at IS NULL OR kind = 'revert'),
    CONSTRAINT admin_elevation_items_handled_together
        CHECK ((handled_at IS NULL) = (handled_by IS NULL)),
    CONSTRAINT admin_elevation_items_note_bounded
        CHECK (note IS NULL OR char_length(note) <= 4000)
);

CREATE UNIQUE INDEX IF NOT EXISTS admin_elevation_items_one_reviewer_idx
    ON elitea_auth.admin_elevation_items (elevation_id, user_id) WHERE kind = 'reviewer';
CREATE UNIQUE INDEX IF NOT EXISTS admin_elevation_items_one_review_idx
    ON elitea_auth.admin_elevation_items (elevation_id) WHERE kind = 'review';
CREATE UNIQUE INDEX IF NOT EXISTS admin_elevation_items_one_revert_idx
    ON elitea_auth.admin_elevation_items (elevation_id, audit_event_id) WHERE kind = 'revert';
CREATE INDEX IF NOT EXISTS admin_elevation_items_elevation_idx
    ON elitea_auth.admin_elevation_items (elevation_id, created_at);
-- The pending-reviewer check reads the open reviews a person is eligible for.
CREATE INDEX IF NOT EXISTS admin_elevation_items_reviewer_idx
    ON elitea_auth.admin_elevation_items (user_id) WHERE kind = 'reviewer';
-- The revert queue: the unhandled entries.
CREATE INDEX IF NOT EXISTS admin_elevation_items_open_revert_idx
    ON elitea_auth.admin_elevation_items (created_at) WHERE kind = 'revert' AND handled_at IS NULL;

CREATE OR REPLACE FUNCTION elitea_auth.admin_elevation_items_append_only()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.handled_at IS NOT NULL THEN
            RAISE EXCEPTION 'elitea_auth.admin_elevation_items: an item cannot be inserted handled'
                USING ERRCODE = 'insufficient_privilege';
        END IF;
        RETURN NEW;
    END IF;
    IF TG_OP = 'UPDATE' AND OLD.kind = 'revert' AND OLD.handled_at IS NULL AND NEW.handled_at IS NOT NULL
       AND (to_jsonb(NEW) - 'handled_at' - 'handled_by') = (to_jsonb(OLD) - 'handled_at' - 'handled_by') THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'elitea_auth.admin_elevation_items is append-only: % refused', TG_OP
        USING ERRCODE = 'insufficient_privilege';
END;
$$;

DROP TRIGGER IF EXISTS admin_elevation_items_append_only ON elitea_auth.admin_elevation_items;
CREATE TRIGGER admin_elevation_items_append_only
    BEFORE INSERT OR UPDATE OR DELETE ON elitea_auth.admin_elevation_items
    FOR EACH ROW EXECUTE FUNCTION elitea_auth.admin_elevation_items_append_only();

DROP TRIGGER IF EXISTS admin_elevation_items_no_truncate ON elitea_auth.admin_elevation_items;
CREATE TRIGGER admin_elevation_items_no_truncate
    BEFORE TRUNCATE ON elitea_auth.admin_elevation_items
    FOR EACH STATEMENT EXECUTE FUNCTION elitea_auth.admin_elevation_items_append_only();

-- An elevation that ended before this file has no recorded reviewers: they
-- are recorded now, from the standing roles of today, as EndElevation would
-- have. A database without the auth_core tables has no reviewer to record.
DO $$
BEGIN
IF to_regclass('public.auth_core__user') IS NULL OR to_regclass('public.auth_core__user_role') IS NULL
   OR to_regclass('public.auth_core__role') IS NULL OR to_regclass('public.auth_core__role_permission') IS NULL
   OR to_regclass('elitea_auth.admin_role_seeds') IS NULL THEN
    RAISE NOTICE '0283: no auth_core tables, no reviewers to record';
    RETURN;
END IF;
INSERT INTO elitea_auth.admin_elevation_items (elevation_id, kind, user_id, created_at)
SELECT elevation.id, 'reviewer', account.id, elevation.ended_at
FROM elitea_auth.admin_elevations elevation
JOIN public.auth_core__user account
  ON account.suspended = false
 AND account.id <> ALL (ARRAY[elevation.user_id, COALESCE(elevation.requester_user_id, 0),
                              COALESCE(elevation.approver_user_id, 0)])
WHERE elevation.review_state = 'review_pending'
  AND NOT EXISTS (SELECT 1 FROM elitea_auth.admin_elevation_items item
                  WHERE item.elevation_id = elevation.id AND item.kind = 'reviewer')
  AND EXISTS (
        SELECT 1
        FROM public.auth_core__user_role assignment
        JOIN public.auth_core__role role ON role.id = assignment.role_id AND role.mode = 'administration'
        JOIN public.auth_core__role_permission grant_row
          ON grant_row.role_id = role.id AND grant_row.permission = 'access.breakglass.review'
        WHERE assignment.user_id = account.id
          AND NOT EXISTS (SELECT 1 FROM elitea_auth.admin_role_seeds seed
                          WHERE seed.role_id = role.id AND seed.marker = 'unassignable'))
ON CONFLICT (elevation_id, user_id) WHERE kind = 'reviewer' DO NOTHING;
END
$$;

COMMENT ON TABLE elitea_auth.admin_elevation_items IS
    'ADR-0059 R2.9a: the reviewers, the review, the revert list and the notes of an elevation. Append-only; a revert entry is marked handled once.';
