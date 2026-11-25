-- 0279_admin_elevation_requests.sql — what an elevation REQUEST records
-- before it starts (ADR-0059 decision 5, "Path 1" steps 1 to 3; story R2.2b,
-- issue 2596).
--
-- The request routes write a row of elitea_auth.admin_elevations (shared
-- 0269) when a person asks, and decide it when a second person answers. Two
-- facts of that path had no column:
--
--   duration_minutes    the duration the person asked for, under the cap of
--                       the break_glass section at request time. The expiry
--                       is computed when the elevation STARTS (now plus this
--                       duration, story R2.2c), not at approval, so the
--                       request has to keep it. NULL on a row that started
--                       at insert (the sealed path, a test): its expiry is
--                       already on the row. Frozen at INSERT by 0269's
--                       trigger, which freezes every column outside its four
--                       lifecycle groups.
--   denial_reason_code  why a request was denied, a code the requester reads
--                       on Home (decision 6, the state table): not_justified,
--                       more_information_needed, standing_access_suffices or
--                       other. Never free text. Written with the decision and
--                       frozen after it, so this file adds it to the
--                       DECISION group of the trigger: it replaces the
--                       trigger function on purpose (0269 says a later column
--                       is frozen unless a migration does exactly that). The
--                       function is 0269's with that one name added to
--                       decision_columns.
--
-- Two indexes serve the console reads: the pending queue (undecided,
-- unstarted rows by request time) and the requester's own latest request.
--
-- IDEMPOTENT. Tolerates a database where 0269 has not run (it then does
-- nothing). No BEGIN/COMMIT: the ledgered runner wraps the file.

DO $$
BEGIN
IF to_regclass('elitea_auth.admin_elevations') IS NULL THEN
    RAISE NOTICE '0279: elitea_auth.admin_elevations absent, nothing to change';
    RETURN;
END IF;

ALTER TABLE elitea_auth.admin_elevations
    ADD COLUMN IF NOT EXISTS duration_minutes integer,
    ADD COLUMN IF NOT EXISTS denial_reason_code text;

IF NOT EXISTS (SELECT 1 FROM pg_constraint
               WHERE conrelid = 'elitea_auth.admin_elevations'::regclass
                 AND conname = 'admin_elevations_duration_bounded') THEN
    -- The online ceiling in code (480 minutes); the section's cap is
    -- checked by the request route under it.
    ALTER TABLE elitea_auth.admin_elevations
        ADD CONSTRAINT admin_elevations_duration_bounded
        CHECK (duration_minutes IS NULL OR (duration_minutes BETWEEN 1 AND 480));
END IF;

IF NOT EXISTS (SELECT 1 FROM pg_constraint
               WHERE conrelid = 'elitea_auth.admin_elevations'::regclass
                 AND conname = 'admin_elevations_denial_reason_known') THEN
    -- A denial reason belongs to a denial and is one of the list. The
    -- request route always gives one; the reconciler's request_expired
    -- carries none. (The comparison is written so that NULL cannot pass.)
    ALTER TABLE elitea_auth.admin_elevations
        ADD CONSTRAINT admin_elevations_denial_reason_known
        CHECK (
            (denial_reason_code IS NULL
                OR (decision IS NOT DISTINCT FROM 'denied'
                    AND denial_reason_code IN ('not_justified', 'more_information_needed',
                                               'standing_access_suffices', 'other')))
        );
END IF;
END
$$;

-- The pending queue of the approvers, oldest first.
CREATE INDEX IF NOT EXISTS admin_elevations_pending_idx
    ON elitea_auth.admin_elevations (requested_at)
    WHERE decision IS NULL AND started_at IS NULL;

-- The requester's own latest request (Home).
CREATE INDEX IF NOT EXISTS admin_elevations_requester_idx
    ON elitea_auth.admin_elevations (requester_user_id, requested_at DESC);

-- 0269's function with denial_reason_code in the decision group. Everything
-- else is unchanged, line for line.
CREATE OR REPLACE FUNCTION elitea_auth.admin_elevations_append_only()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    decision_columns constant text[] := ARRAY['decision', 'decided_at', 'level', 'approver_user_id', 'denial_reason_code'];
    start_columns    constant text[] := ARRAY['started_at', 'expires_at'];
    end_columns      constant text[] := ARRAY['ended_at', 'end_reason'];
    old_row  jsonb;
    new_row  jsonb;
    frozen   text;
BEGIN
    IF TG_OP = 'INSERT' THEN
        -- A row starts unreviewed or awaiting review; a review is a later
        -- UPDATE of its own.
        IF NEW.review_state IS NOT NULL AND NEW.review_state <> 'review_pending' THEN
            RAISE EXCEPTION 'elitea_auth.admin_elevations: a row cannot be inserted already reviewed (%)', NEW.review_state
                USING ERRCODE = 'insufficient_privilege';
        END IF;
        -- One open elevation per session: there is no extension, more time is a
        -- new request after this one ends. StartElevation takes the same lock and
        -- checks first; this repeats it for any other writer.
        IF NEW.started_at IS NOT NULL AND NEW.ended_at IS NULL
           AND (TG_OP = 'INSERT' OR OLD.started_at IS NULL) THEN
            PERFORM pg_advisory_xact_lock(hashtextextended('admin_elevations:session:' || NEW.browser_session_ref::text, 0));
            IF EXISTS (
                SELECT 1 FROM elitea_auth.admin_elevations other
                WHERE other.browser_session_ref = NEW.browser_session_ref
                  AND other.id <> NEW.id
                  AND other.started_at IS NOT NULL AND other.ended_at IS NULL
                  AND other.expires_at > NEW.started_at
            ) THEN
                RAISE EXCEPTION 'elitea_auth.admin_elevations: session % already holds an open elevation', NEW.browser_session_ref
                    USING ERRCODE = 'unique_violation';
            END IF;
        END IF;
        RETURN NEW;
    END IF;
    IF TG_OP <> 'UPDATE' THEN
        RAISE EXCEPTION 'elitea_auth.admin_elevations is append-only: % refused', TG_OP
            USING ERRCODE = 'insufficient_privilege';
    END IF;

    old_row := to_jsonb(OLD);
    new_row := to_jsonb(NEW);

    -- Everything outside the four lifecycle groups is frozen at INSERT.
    SELECT string_agg(key, ', ' ORDER BY key) INTO frozen
    FROM jsonb_each(old_row - decision_columns - start_columns - end_columns - 'review_state'::text) AS o(key, value)
    WHERE new_row -> key IS DISTINCT FROM value;
    IF frozen IS NOT NULL THEN
        RAISE EXCEPTION 'elitea_auth.admin_elevations is append-only: an UPDATE of % refused', frozen
            USING ERRCODE = 'insufficient_privilege';
    END IF;

    -- Each lifecycle group is written once: after that it is frozen too.
    IF OLD.decision IS NOT NULL THEN
        SELECT string_agg(c, ', ') INTO frozen FROM unnest(decision_columns) AS c
        WHERE new_row -> c IS DISTINCT FROM old_row -> c;
        IF frozen IS NOT NULL THEN
            RAISE EXCEPTION 'elitea_auth.admin_elevations is append-only: the decision is recorded, an UPDATE of % refused', frozen
                USING ERRCODE = 'insufficient_privilege';
        END IF;
    END IF;
    IF OLD.started_at IS NOT NULL THEN
        SELECT string_agg(c, ', ') INTO frozen FROM unnest(start_columns) AS c
        WHERE new_row -> c IS DISTINCT FROM old_row -> c;
        IF frozen IS NOT NULL THEN
            RAISE EXCEPTION 'elitea_auth.admin_elevations is append-only: the start is recorded, an UPDATE of % refused', frozen
                USING ERRCODE = 'insufficient_privilege';
        END IF;
    END IF;
    IF OLD.ended_at IS NOT NULL THEN
        SELECT string_agg(c, ', ') INTO frozen FROM unnest(end_columns) AS c
        WHERE new_row -> c IS DISTINCT FROM old_row -> c;
        IF frozen IS NOT NULL THEN
            RAISE EXCEPTION 'elitea_auth.admin_elevations is append-only: the end is recorded, an UPDATE of % refused', frozen
                USING ERRCODE = 'insufficient_privilege';
        END IF;
    END IF;

    -- The review state only moves forward.
    IF NEW.review_state IS DISTINCT FROM OLD.review_state AND NOT (
        (OLD.review_state IS NULL AND NEW.review_state = 'review_pending')
        OR (OLD.review_state = 'review_pending' AND NEW.review_state IN ('reviewed', 'unreviewed_no_reviewer'))
    ) THEN
        RAISE EXCEPTION 'elitea_auth.admin_elevations: the review state cannot move from % to %',
            coalesce(OLD.review_state, 'none'), coalesce(NEW.review_state, 'none')
            USING ERRCODE = 'insufficient_privilege';
    END IF;
    -- One open elevation per session: there is no extension, more time is a
    -- new request after this one ends. StartElevation takes the same lock and
    -- checks first; this repeats it for any other writer.
    IF NEW.started_at IS NOT NULL AND NEW.ended_at IS NULL
       AND (TG_OP = 'INSERT' OR OLD.started_at IS NULL) THEN
        PERFORM pg_advisory_xact_lock(hashtextextended('admin_elevations:session:' || NEW.browser_session_ref::text, 0));
        IF EXISTS (
            SELECT 1 FROM elitea_auth.admin_elevations other
            WHERE other.browser_session_ref = NEW.browser_session_ref
              AND other.id <> NEW.id
              AND other.started_at IS NOT NULL AND other.ended_at IS NULL
              AND other.expires_at > NEW.started_at
        ) THEN
            RAISE EXCEPTION 'elitea_auth.admin_elevations: session % already holds an open elevation', NEW.browser_session_ref
                USING ERRCODE = 'unique_violation';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;

COMMENT ON COLUMN elitea_auth.admin_elevations.duration_minutes IS
    'ADR-0059 R2.2b: the duration the request asked for; the start computes the expiry from it. Frozen at insert.';
COMMENT ON COLUMN elitea_auth.admin_elevations.denial_reason_code IS
    'ADR-0059 R2.2b: the reason code of a denial, written with the decision. Never free text.';
