-- 0269_admin_elevations.sql — the elevation ledger of break-glass access
-- (ADR-0059 decision 5, "It is an elevation, not a role", "Bound to one
-- session"; story R2.1a, issue 2515).
--
-- WHAT AN ELEVATION IS. A time-boxed addition to the administration set of ONE
-- browser session, recorded with a reason. It is not a role: a role
-- assignment (public.auth_core__user_role) has no expiry, grantor or reason,
-- and a copied role row would miss every later grant migration that names
-- super_admin or admin. So the level is a VALUE (admin | super_admin) and the
-- resolver (R2.1b) joins the level's role by name at read time.
--
-- ONE ROW PER ELEVATION, from its request to its review:
--
--   id                   uuid, the elevation id that centry.audit_events
--                        .elevation_id (shared 0268) and every security event
--                        of the elevation carry
--   user_id              the person elevated (auth_core__user.id)
--   browser_session_ref  the `ref` of the browser session the elevation is
--                        bound to (browser_sessions.ref, shared 0200; a Form
--                        session's ref, shared 0266, comes from the same
--                        sequence). The principal carries it as
--                        BrowserSessionRef. Never 0: a session without a
--                        server-side row can hold no elevation
--   level                admin | super_admin; set with the decision (the
--                        approver's level), then frozen
--   path                 approved (a second person approved), self (approval
--                        was off, the system approved at the admin level) or
--                        sealed (the sealed credential)
--   reason_code          incident | identity_outage | recovery | other
--   reason               the reason text (never copied into a chained event)
--   ticket               optional
--   requester_user_id    who asked
--   approver_user_id     who approved or denied on the approved path; NULL on
--                        the self and sealed paths
--   requested_at         timestamptz
--   decision             approved | denied | request_expired, with decided_at
--   started_at,          set together when the elevation starts. Access is
--   expires_at           the PREDICATE started_at IS NOT NULL AND
--                        ended_at IS NULL AND expires_at > now(): no job has to
--                        run for access to stop, and no column exists that a
--                        job must flip
--   ended_at,            set together, once: holder, approver, operator,
--   end_reason           expired, logout, session_revoked, user_suspended,
--                        user_deleted or scim_deactivated
--   review_state         review_pending (set by the end), then reviewed or
--                        unreviewed_no_reviewer
--
-- APPEND-ONLY, BY TRIGGER. DELETE and TRUNCATE are refused. An UPDATE may only
-- fill what the row's lifecycle has not reached yet, each group once:
--
--   the decision   decision, decided_at, level, approver_user_id: while
--                  decision IS NULL
--   the start      started_at, expires_at: while started_at IS NULL
--   the end        ended_at, end_reason: while ended_at IS NULL
--   the review     review_state, forward only: NULL -> review_pending ->
--                  reviewed | unreviewed_no_reviewer
--
-- Every other column is frozen at INSERT: the person, the session reference,
-- the path, the reason code and text, the ticket, the requester and the
-- request time. The trigger compares the whole row (to_jsonb) minus the
-- columns named above, so a column a later migration adds is frozen unless
-- that migration replaces the function on purpose. A started row is
-- therefore frozen in its level and its expiry: there is no extension, more
-- time is a new request.
--
-- NO FOREIGN KEY to browser_sessions.ref: the session sweeper deletes old
-- session rows, and the ledger must outlive them. None to auth_core__user
-- either (as in 0117 and 0260): the ledger outlives the account, and the
-- shared corpus also runs on databases without the auth_core tables. Nothing
-- here touches auth_core, so the file needs no to_regclass guard.
--
-- The table is empty until break-glass is used. The writers are the ledger
-- functions of internal/adminaccess (StartElevation, EndElevation); no route
-- calls them before R2.2c and R2.7b.
--
-- IDEMPOTENT throughout. No BEGIN/COMMIT: the ledgered runner executes each
-- file inside one transaction with its ledger row (migrate/runner.go apply).

CREATE SCHEMA IF NOT EXISTS elitea_auth;

CREATE TABLE IF NOT EXISTS elitea_auth.admin_elevations (
    id                  uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id             bigint      NOT NULL,
    browser_session_ref bigint      NOT NULL,
    level               text,
    path                text        NOT NULL,
    reason_code         text        NOT NULL,
    reason              text        NOT NULL,
    ticket              text,
    requester_user_id   bigint      NOT NULL,
    approver_user_id    bigint,
    requested_at        timestamptz NOT NULL,
    decision            text,
    decided_at          timestamptz,
    started_at          timestamptz,
    expires_at          timestamptz,
    ended_at            timestamptz,
    end_reason          text,
    review_state        text,
    CONSTRAINT admin_elevations_session_present
        CHECK (browser_session_ref > 0),
    CONSTRAINT admin_elevations_people_present
        CHECK (user_id > 0 AND requester_user_id > 0 AND (approver_user_id IS NULL OR approver_user_id > 0)),
    CONSTRAINT admin_elevations_level_known
        CHECK (level IS NULL OR level IN ('admin', 'super_admin')),
    CONSTRAINT admin_elevations_path_known
        CHECK (path IN ('approved', 'self', 'sealed')),
    CONSTRAINT admin_elevations_reason_code_known
        CHECK (reason_code IN ('incident', 'identity_outage', 'recovery', 'other')),
    CONSTRAINT admin_elevations_reason_present
        CHECK (length(btrim(reason)) > 0),
    CONSTRAINT admin_elevations_decision_known
        CHECK (decision IS NULL OR decision IN ('approved', 'denied', 'request_expired')),
    CONSTRAINT admin_elevations_decision_dated
        CHECK ((decision IS NULL) = (decided_at IS NULL) AND (decided_at IS NULL OR decided_at >= requested_at)),
    -- An approval carries a level; nothing else does. (Every comparison with
    -- a nullable column below is written so that NULL cannot pass a CHECK.)
    CONSTRAINT admin_elevations_level_with_approval
        CHECK ((decision IS NOT DISTINCT FROM 'approved') = (level IS NOT NULL)),
    -- Nobody approves the own request (ADR-0059 decision 5, path 1 step 2).
    CONSTRAINT admin_elevations_approver_not_requester
        CHECK (approver_user_id IS NULL OR (approver_user_id <> requester_user_id AND approver_user_id <> user_id)),
    -- On the approved path a second person approves or denies, and is
    -- recorded exactly then; the self and sealed paths have no approver by
    -- definition.
    CONSTRAINT admin_elevations_approver_matches_path
        CHECK (
            (path = 'approved'
                AND coalesce(decision IN ('approved', 'denied'), false) = (approver_user_id IS NOT NULL))
            OR (path IN ('self', 'sealed') AND approver_user_id IS NULL)
        ),
    -- The super_admin level always needs a super_admin approver or the sealed
    -- path: approval switched off gives the admin level only.
    CONSTRAINT admin_elevations_self_is_admin_level
        CHECK (path <> 'self' OR level IS DISTINCT FROM 'super_admin'),
    CONSTRAINT admin_elevations_start_complete
        CHECK ((started_at IS NULL) = (expires_at IS NULL)),
    CONSTRAINT admin_elevations_start_approved
        CHECK (started_at IS NULL OR (decision IS NOT DISTINCT FROM 'approved' AND started_at >= decided_at)),
    -- The ceilings in code (ADR-0059 decision 5, owner decision 5): 480
    -- minutes online, 240 sealed. A deployment setting can only lower them, so
    -- no row may exceed them.
    CONSTRAINT admin_elevations_expiry_bounded
        CHECK (
            expires_at IS NULL
            OR (expires_at > started_at
                AND expires_at <= started_at + CASE WHEN path = 'sealed'
                                                    THEN interval '240 minutes'
                                                    ELSE interval '480 minutes' END)
        ),
    CONSTRAINT admin_elevations_end_complete
        CHECK ((ended_at IS NULL) = (end_reason IS NULL)),
    CONSTRAINT admin_elevations_end_after_start
        CHECK (ended_at IS NULL OR (started_at IS NOT NULL AND ended_at >= started_at)),
    CONSTRAINT admin_elevations_end_reason_known
        CHECK (end_reason IS NULL OR end_reason IN (
            'holder', 'approver', 'operator', 'expired', 'logout', 'session_revoked',
            'user_suspended', 'user_deleted', 'scim_deactivated')),
    CONSTRAINT admin_elevations_review_state_known
        CHECK (review_state IS NULL OR review_state IN ('review_pending', 'reviewed', 'unreviewed_no_reviewer')),
    CONSTRAINT admin_elevations_review_after_end
        CHECK (review_state IS NULL OR ended_at IS NOT NULL)
);

-- The resolver (R2.1b) reads the open elevation of one session on every
-- administration request.
CREATE INDEX IF NOT EXISTS admin_elevations_open_session_idx
    ON elitea_auth.admin_elevations (browser_session_ref)
    WHERE started_at IS NOT NULL AND ended_at IS NULL;

-- The reconciler (R2.16) finds the started, unended elevations past expiry.
CREATE INDEX IF NOT EXISTS admin_elevations_open_expiry_idx
    ON elitea_auth.admin_elevations (expires_at)
    WHERE started_at IS NOT NULL AND ended_at IS NULL;

-- The history of one person, newest first.
CREATE INDEX IF NOT EXISTS admin_elevations_user_idx
    ON elitea_auth.admin_elevations (user_id, requested_at DESC);

CREATE OR REPLACE FUNCTION elitea_auth.admin_elevations_append_only()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    decision_columns constant text[] := ARRAY['decision', 'decided_at', 'level', 'approver_user_id'];
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
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS admin_elevations_append_only ON elitea_auth.admin_elevations;
CREATE TRIGGER admin_elevations_append_only
    BEFORE INSERT OR UPDATE OR DELETE ON elitea_auth.admin_elevations
    FOR EACH ROW EXECUTE FUNCTION elitea_auth.admin_elevations_append_only();

DROP TRIGGER IF EXISTS admin_elevations_no_truncate ON elitea_auth.admin_elevations;
CREATE TRIGGER admin_elevations_no_truncate
    BEFORE TRUNCATE ON elitea_auth.admin_elevations
    FOR EACH STATEMENT EXECUTE FUNCTION elitea_auth.admin_elevations_append_only();
