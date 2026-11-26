-- 0285_admin_elevation_notes.sql — the notes on an elevation's record and the
-- acknowledgement of an elevation nobody independent could review (issue
-- #2604, ADR-0059 decision 5 "Review after use", story R2.9c).
--
-- THE GAP. 0283 made `note` an item kind that names a person (user_id NOT
-- NULL), but the ADR gives the note to "the operator": a person with workload
-- access who runs `elitea-main break-glass note <elevation id>` on the
-- server, and who is no account of the platform. And a global administrator
-- must be able to acknowledge an `unreviewed_no_reviewer` elevation so the
-- posture list (R2.14b) stops repeating it, without the record ever saying a
-- review happened.
--
-- THE CHANGE. Two columns of elitea_auth.admin_elevation_items:
--
--   note_source      who wrote a note: `operator` (the process command; no
--                    person) or `console` (a person, user_id set). Only a
--                    note has one, and every note has one.
--   acknowledgement  the note is the acknowledgement of a terminal review,
--                    written through the console by a standing global
--                    administrator. At most one per elevation (a unique
--                    partial index), so "acknowledged" is a fact with one
--                    note id, never a counter.
--
-- The elevation row is not touched: its review_state stays
-- unreviewed_no_reviewer. The note text stays here; its security audit event
-- (note_added) carries the note's id and length only.
--
-- No row of kind `note` exists before this migration (nothing wrote one), so
-- the constraints are checked on an empty set. The append-only trigger of
-- 0283 is unchanged: a note is never edited.
--
-- Idempotent: ADD COLUMN IF NOT EXISTS, constraints dropped before they are
-- added, CREATE ... IF NOT EXISTS.

ALTER TABLE elitea_auth.admin_elevation_items
    ADD COLUMN IF NOT EXISTS note_source text,
    ADD COLUMN IF NOT EXISTS acknowledgement boolean NOT NULL DEFAULT false;

-- A reviewer, a reviewer change and a review still name a person; a note
-- names one when a person wrote it (note_source console) and none when the
-- operator did.
ALTER TABLE elitea_auth.admin_elevation_items
    DROP CONSTRAINT IF EXISTS admin_elevation_items_reviewer_names_a_person;
ALTER TABLE elitea_auth.admin_elevation_items
    ADD CONSTRAINT admin_elevation_items_reviewer_names_a_person
        CHECK (kind NOT IN ('reviewer', 'reviewer_change', 'review') OR user_id IS NOT NULL);

ALTER TABLE elitea_auth.admin_elevation_items
    DROP CONSTRAINT IF EXISTS admin_elevation_items_note_known;
ALTER TABLE elitea_auth.admin_elevation_items
    ADD CONSTRAINT admin_elevation_items_note_known
        CHECK ((kind = 'note') = (note_source IS NOT NULL)
               AND (kind <> 'note'
                    OR (note IS NOT NULL AND length(btrim(note)) > 0
                        AND ((note_source = 'operator' AND user_id IS NULL)
                             OR (note_source = 'console' AND user_id IS NOT NULL)))));

ALTER TABLE elitea_auth.admin_elevation_items
    DROP CONSTRAINT IF EXISTS admin_elevation_items_acknowledgement_is_a_console_note;
ALTER TABLE elitea_auth.admin_elevation_items
    ADD CONSTRAINT admin_elevation_items_acknowledgement_is_a_console_note
        CHECK (NOT acknowledgement OR (kind = 'note' AND note_source = 'console'));

CREATE UNIQUE INDEX IF NOT EXISTS admin_elevation_items_one_acknowledgement_idx
    ON elitea_auth.admin_elevation_items (elevation_id) WHERE acknowledgement;

COMMENT ON COLUMN elitea_auth.admin_elevation_items.note_source IS
    'ADR-0059 R2.9c: who wrote a note: operator (the process command, no person) or console (user_id).';
COMMENT ON COLUMN elitea_auth.admin_elevation_items.acknowledgement IS
    'ADR-0059 R2.9c: the note acknowledges an unreviewed_no_reviewer elevation; at most one per elevation. The review state is unchanged.';
