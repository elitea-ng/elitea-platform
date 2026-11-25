package adminaccess

// Elevation requests: request, approve, deny and expire (ADR-0059 decision 5,
// "Path 1: elevation while single sign-on works", steps 1 to 3, and
// "Switching approval off"; story R2.2b, issue 2596).
//
// A request is a row of the ledger (elevation.go) that has not started: the
// person, the session that asked, the reason code and text, the optional
// ticket and the duration asked for (shared 0279). A second person decides
// it once; the trigger of shared 0269 freezes the decision after that. The
// start (R2.2c) reads an approved request and computes the expiry from its
// duration then.
//
// Every step writes its `admin.elevation` event (R2.3) on the caller's
// transaction, under one savepoint with the row, and a request writes its
// notice rows (R2.5a) under the same savepoint: the row, its event and its
// notices commit together or not at all.
//
// THE RULES, each checked here in the transaction and not only by the route:
//
//   - the requester holds access.breakglass.request through a STANDING role
//     and asks from a live session of theirs; one undecided request per
//     person at a time;
//   - the approver holds access.breakglass.approve through a standing role,
//     is not the requester and not the person to be elevated, and is under
//     no active elevation, in any session;
//   - the level is the approver's: super_admin when the approver holds
//     admin.auth.users.super_admin through a standing role, admin otherwise.
//     It is recorded on the row at approval;
//   - an answer after the request lifetime is refused, and the expiry is
//     recorded then (the reconciler of R2.16 records it otherwise);
//   - with approval switched off (the EFFECTIVE policy, after its delay), the
//     request is approved by the system at the admin level on the path
//     `self`, and its `approved` event names no actor and carries
//     `approver:none`. The super_admin level is never given that way.
//
// No request lengthens an elevation: there is no extension, more time is a
// new request.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// The reason codes of a denial (shared 0279's CHECK holds the same list).
const (
	ElevationDenialNotJustified           = "not_justified"
	ElevationDenialMoreInformationNeeded  = "more_information_needed"
	ElevationDenialStandingAccessSuffices = "standing_access_suffices"
	ElevationDenialOther                  = "other"
)

// ElevationReasonCodes are the reason codes of a request, in the form's order.
func ElevationReasonCodes() []string {
	return []string{ElevationReasonIncident, ElevationReasonIdentityOutage, ElevationReasonRecovery, ElevationReasonOther}
}

// ElevationDenialReasonCodes are the reason codes of a denial.
func ElevationDenialReasonCodes() []string {
	return []string{ElevationDenialNotJustified, ElevationDenialMoreInformationNeeded,
		ElevationDenialStandingAccessSuffices, ElevationDenialOther}
}

// The bounds of a request's free text, in characters.
const (
	MaxElevationReasonLength = 2000
	MaxElevationTicketLength = 200
)

// ApproverNone is the reason code of the `approved` event of a request that
// approval-off approved: no approver was involved.
const ApproverNone = "approver:none"

// The refusals of a request and of an answer.
var (
	// ErrElevationRequester: the person does not hold
	// access.breakglass.request through a standing role (an elevation that
	// gives it does not count).
	ErrElevationRequester = errors.New("elevation request refused: the person does not hold access.breakglass.request through a standing role")
	// ErrElevationRequestPending: the person already has an undecided
	// request within its lifetime.
	ErrElevationRequestPending = errors.New("elevation request refused: the person already has a pending request")
	// ErrElevationRequestDecided: the request was decided already (or never
	// needed an answer: approval was off, or it is a sealed elevation).
	ErrElevationRequestDecided = errors.New("the elevation request was decided already")
	// ErrElevationRequestExpired: the request lifetime has passed; nobody
	// can answer it any more.
	ErrElevationRequestExpired = errors.New("the elevation request expired unanswered")
	// ErrElevationOwnRequest: nobody answers the own request, or a request to
	// elevate themselves.
	ErrElevationOwnRequest = errors.New("nobody answers their own elevation request")
	// ErrElevationApproverElevated: the approver is under an active
	// elevation.
	ErrElevationApproverElevated = errors.New("a person under an elevation cannot answer an elevation request")
	// ErrElevationApproverNotEligible: the approver does not hold
	// access.breakglass.approve through a standing role, or is suspended.
	// ErrElevationPersonNotEligible refuses an approval of a person who can
	// no longer hold it: suspended, without a standing
	// access.breakglass.request, or with the request's session ended.
	ErrElevationPersonNotEligible   = errors.New("the person of the request can no longer hold an elevation")
	ErrElevationApproverNotEligible = errors.New("the approver does not hold access.breakglass.approve through a standing role")
	// ErrElevationDenialReasonCode: a denial without a known reason code.
	ErrElevationDenialReasonCode = fmt.Errorf("%w: unknown denial reason code", ErrElevationInvalid)
	// ErrElevationDurationOverCap: the duration is over the cap of the
	// break_glass section.
	ErrElevationDurationOverCap = fmt.Errorf("%w: the duration is over the cap", ErrElevationInvalid)
)

// ElevationRequest is what a person asks for.
type ElevationRequest struct {
	// UserID is the person asking, to be elevated.
	UserID int64
	// BrowserSessionRef is the principal's BrowserSessionRef: the session
	// that asks, which is the one the start (R2.2c) binds. Never 0.
	BrowserSessionRef int64
	ReasonCode        string
	Reason            string
	Ticket            string
	// Duration is the duration asked for, in whole minutes, at most Cap.
	Duration time.Duration
	// Cap is the cap of the break_glass section at At
	// (BreakGlassSettings.OnlineMax); never above MaxElevationDurationOnline.
	Cap time.Duration
	// ApprovalRequired is the EFFECTIVE approval policy at At
	// (BreakGlassPolicy.ApprovalRequired). false approves the request at
	// the admin level on the path `self`.
	ApprovalRequired bool
	// RequestLifetime is the request lifetime of the section: an undecided
	// request of the person within it refuses a second one.
	RequestLifetime time.Duration
	// At is the request time, from the caller's clock. Required.
	At time.Time
	// Source is the events' source ("" is the console); CorrelationID ties
	// them to the HTTP request.
	Source        string
	CorrelationID string
}

// Validate checks the request without touching the database.
func (r ElevationRequest) Validate() error {
	if r.BrowserSessionRef <= 0 {
		return ErrElevationNoSession
	}
	if r.UserID <= 0 {
		return fmt.Errorf("%w: a person is missing", ErrElevationInvalid)
	}
	if !knownElevationReasonCode(r.ReasonCode) {
		return fmt.Errorf("%w %q", ErrElevationReasonCode, r.ReasonCode)
	}
	if strings.TrimSpace(r.Reason) == "" {
		return fmt.Errorf("%w: no reason text", ErrElevationInvalid)
	}
	if utf8.RuneCountInString(r.Reason) > MaxElevationReasonLength {
		return fmt.Errorf("%w: the reason text is longer than %d characters", ErrElevationInvalid, MaxElevationReasonLength)
	}
	if utf8.RuneCountInString(strings.TrimSpace(r.Ticket)) > MaxElevationTicketLength {
		return fmt.Errorf("%w: the ticket is longer than %d characters", ErrElevationInvalid, MaxElevationTicketLength)
	}
	if r.At.IsZero() {
		return ErrElevationNoTimestamp
	}
	if r.RequestLifetime <= 0 {
		return fmt.Errorf("%w: no request lifetime", ErrElevationInvalid)
	}
	if r.Cap <= 0 || r.Cap > MaxElevationDurationOnline {
		return fmt.Errorf("%w: the cap %s is outside (0, %s]", ErrElevationInvalid, r.Cap, MaxElevationDurationOnline)
	}
	if r.Duration <= 0 || r.Duration%time.Minute != 0 {
		return fmt.Errorf("%w: the duration %s is not a positive number of minutes", ErrElevationInvalid, r.Duration)
	}
	if r.Duration > r.Cap {
		return fmt.Errorf("%w: %s is over %s", ErrElevationDurationOverCap, r.Duration, r.Cap)
	}
	if r.Source != "" && !knownElevationSource(r.Source) {
		return fmt.Errorf("%w: unknown event source %q", ErrElevationInvalid, r.Source)
	}
	return nil
}

// ElevationRequested is what RequestElevation wrote.
type ElevationRequested struct {
	Elevation ElevationRecord
	// Notices is the number of request notice rows written: one per holder
	// of access.breakglass.approve other than the requester. 0 when approval
	// was off, which needs no answer and tells nobody to give one.
	Notices int
}

// RequestElevation writes one request in the caller's transaction, with its
// `requested` event and its notice rows, under one savepoint. With approval
// off it also decides it: approved by the system at the admin level on the
// path `self`, with the `approved` event.
//
// It refuses, before it writes: an invalid request (Validate); a person who
// is not an unsuspended account (ErrElevationPerson); a session reference
// that is not a live session of the person (ErrElevationSession); a person
// who does not hold access.breakglass.request through a standing role
// (ErrElevationRequester); a person with an open request
// (ErrElevationRequestPending): an undecided one within the lifetime, or an
// approval not started within the lifetime from its decision, under a
// transaction lock on the person.
func RequestElevation(ctx context.Context, tx pgx.Tx, appender securityaudit.Appender, request ElevationRequest) (ElevationRequested, error) {
	if err := request.Validate(); err != nil {
		return ElevationRequested{}, err
	}
	if appender == nil {
		return ElevationRequested{}, ErrElevationNoAppender
	}
	if tx == nil {
		return ElevationRequested{}, errors.New("request elevation: no transaction")
	}
	source := request.Source
	if source == "" {
		source = securityaudit.ElevationSourceConsole
	}
	at := request.At.UTC()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('admin_elevations:requester:' || $1::bigint::text, 0))`,
		request.UserID); err != nil {
		return ElevationRequested{}, fmt.Errorf("request elevation: lock the person: %w", err)
	}
	if err := requesterEligible(ctx, tx, request.UserID, request.BrowserSessionRef, at); err != nil {
		return ElevationRequested{}, err
	}
	// One open request at a time: an undecided request within its lifetime,
	// or an approval that has not started within the lifetime from its
	// decision. The second half bounds a person to one request per lifetime
	// with approval off, where every request is decided at insert.
	var pending bool
	if err := tx.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1 FROM elitea_auth.admin_elevations
    WHERE requester_user_id = $1 AND started_at IS NULL
      AND (   (decision IS NULL AND requested_at > $2::timestamptz - make_interval(secs => $3))
           OR (decision = 'approved' AND decided_at > $2::timestamptz - make_interval(secs => $3))))`,
		request.UserID, at, request.RequestLifetime.Seconds()).Scan(&pending); err != nil {
		return ElevationRequested{}, fmt.Errorf("request elevation: read the open requests: %w", err)
	}
	if pending {
		return ElevationRequested{}, ErrElevationRequestPending
	}
	path := ElevationPathApproved
	if !request.ApprovalRequired {
		path = ElevationPathSelf
	}
	var result ElevationRequested
	err := withSavepoint(ctx, tx, func(sp pgx.Tx) error {
		elevation, err := scanElevation(sp.QueryRow(ctx, `
INSERT INTO elitea_auth.admin_elevations
    (user_id, browser_session_ref, path, reason_code, reason, ticket, requester_user_id, requested_at, duration_minutes)
VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''), $1, $7, $8)
RETURNING `+elevationColumns,
			request.UserID, request.BrowserSessionRef, path, request.ReasonCode, request.Reason,
			strings.TrimSpace(request.Ticket), at, int64(request.Duration/time.Minute)))
		if err != nil {
			return fmt.Errorf("request elevation: %w", err)
		}
		if err := AppendElevationEvent(ctx, sp, appender, ElevationEvent{
			Action:            securityaudit.ElevationRequested,
			Source:            source,
			Elevation:         elevation,
			ActorUserID:       request.UserID,
			AuthenticationRef: sessionAuthenticationRef(request.BrowserSessionRef),
			CorrelationID:     request.CorrelationID,
		}); err != nil {
			return err
		}
		if request.ApprovalRequired {
			result.Elevation = elevation
			result.Notices, err = WriteElevationNotices(ctx, sp, ElevationNotice{
				Kind: NoticeRequest, Elevation: elevation, At: at,
			})
			return err
		}
		// Approval is off: the system approves at the admin level, and the
		// event says that nobody approved.
		elevation, err = scanElevation(sp.QueryRow(ctx, `
UPDATE elitea_auth.admin_elevations
SET decision = 'approved', decided_at = $2, level = $3
WHERE id = $1 AND decision IS NULL
RETURNING `+elevationColumns, elevation.ID, at, ElevationLevelAdmin))
		if err != nil {
			return fmt.Errorf("request elevation: approve with approval off: %w", err)
		}
		result.Elevation = elevation
		return AppendElevationEvent(ctx, sp, appender, ElevationEvent{
			Action:        securityaudit.ElevationApproved,
			Source:        securityaudit.ElevationSourceSystem,
			Elevation:     elevation,
			CorrelationID: request.CorrelationID,
			Reasons:       []string{ApproverNone},
		})
	})
	if err != nil {
		return ElevationRequested{}, err
	}
	return result, nil
}

// ElevationAnswer is an approval or a denial of a request.
type ElevationAnswer struct {
	ID uuid.UUID
	// ApproverUserID is who answers.
	ApproverUserID int64
	// ApproverSessionRef is the approver's browser session, recorded on the
	// event.
	ApproverSessionRef int64
	// Approve: true approves, false denies.
	Approve bool
	// DenialReasonCode is the reason code of a denial (ElevationDenial*).
	DenialReasonCode string
	// RequestLifetime is the request lifetime of the section at At.
	RequestLifetime time.Duration
	// At is the answer's time, from the caller's clock. Required.
	At            time.Time
	Source        string
	CorrelationID string
}

func (a ElevationAnswer) validate() error {
	switch {
	case a.ID == uuid.Nil:
		return fmt.Errorf("%w: no request", ErrElevationInvalid)
	case a.ApproverUserID <= 0:
		return fmt.Errorf("%w: no approver", ErrElevationInvalid)
	case a.At.IsZero():
		return ErrElevationNoTimestamp
	case a.RequestLifetime <= 0:
		return fmt.Errorf("%w: no request lifetime", ErrElevationInvalid)
	case a.Source != "" && !knownElevationSource(a.Source):
		return fmt.Errorf("%w: unknown event source %q", ErrElevationInvalid, a.Source)
	case a.Approve && a.DenialReasonCode != "":
		return fmt.Errorf("%w: an approval has no denial reason", ErrElevationInvalid)
	case !a.Approve && !slices.Contains(ElevationDenialReasonCodes(), a.DenialReasonCode):
		return fmt.Errorf("%w %q", ErrElevationDenialReasonCode, a.DenialReasonCode)
	}
	return nil
}

// AnswerElevationRequest approves or denies one request in the caller's
// transaction, with its `approved` or `denied` event, under one savepoint.
// The level of an approval is the approver's, read from the standing roles
// here.
//
// It refuses, in this order: an unknown request (ErrElevationNotFound); one
// decided already (ErrElevationRequestDecided, or ErrElevationRequestExpired
// for a recorded expiry); one past its lifetime (ErrElevationRequestExpired):
// THE EXPIRY IS THEN WRITTEN ON tx, with its event, and the caller commits
// it, so the request reads as expired at once; the own request
// (ErrElevationOwnRequest); an approver under an active elevation
// (ErrElevationApproverElevated); an approver who does not hold
// access.breakglass.approve through a standing role or is suspended
// (ErrElevationApproverNotEligible); and, for an approval only, a person who
// can no longer hold it (ErrElevationPersonNotEligible: suspended, session
// ended, request role removed). The returned record is the row as it
// stands after the call, also on a refusal of a row that exists.
func AnswerElevationRequest(ctx context.Context, tx pgx.Tx, appender securityaudit.Appender, answer ElevationAnswer) (ElevationRecord, error) {
	if err := answer.validate(); err != nil {
		return ElevationRecord{}, err
	}
	if appender == nil {
		return ElevationRecord{}, ErrElevationNoAppender
	}
	if tx == nil {
		return ElevationRecord{}, errors.New("answer elevation request: no transaction")
	}
	source := answer.Source
	if source == "" {
		source = securityaudit.ElevationSourceConsole
	}
	at := answer.At.UTC()
	request, err := scanElevation(tx.QueryRow(ctx, `
SELECT `+elevationColumns+` FROM elitea_auth.admin_elevations WHERE id = $1 FOR UPDATE`, answer.ID))
	if errors.Is(err, pgx.ErrNoRows) {
		return ElevationRecord{}, ErrElevationNotFound
	}
	if err != nil {
		return ElevationRecord{}, fmt.Errorf("answer elevation request: read the request: %w", err)
	}
	switch {
	case request.Decision == ElevationDecisionRequestExpired:
		return request, ErrElevationRequestExpired
	case request.Decision != "" || !request.StartedAt.IsZero() || request.Path != ElevationPathApproved:
		return request, ErrElevationRequestDecided
	}
	if !at.Before(request.RequestedAt.Add(answer.RequestLifetime)) {
		expired, changed, err := expireRequest(ctx, tx, appender, request.ID, at, answer.RequestLifetime)
		if err != nil {
			return request, fmt.Errorf("answer elevation request: record the expiry: %w", err)
		}
		if changed {
			request = expired
		}
		return request, ErrElevationRequestExpired
	}
	if answer.ApproverUserID == request.RequesterUserID || answer.ApproverUserID == request.UserID {
		return request, ErrElevationOwnRequest
	}
	// The person must still be able to hold what is approved: an account
	// suspended, a request role removed or a session ended since the request
	// refuses the approval. A denial needs none of it.
	if answer.Approve {
		if err := requesterEligible(ctx, tx, request.UserID, request.BrowserSessionRef, at); err != nil {
			if errors.Is(err, ErrElevationPerson) || errors.Is(err, ErrElevationSession) || errors.Is(err, ErrElevationRequester) {
				return request, fmt.Errorf("%w (%s)", ErrElevationPersonNotEligible, err.Error())
			}
			return request, fmt.Errorf("answer elevation request: %w", err)
		}
	}
	var elevated, eligible, superAdmin bool
	if err := tx.QueryRow(ctx, `
SELECT
    EXISTS (
        SELECT 1 FROM elitea_auth.admin_elevations
        WHERE user_id = $1 AND started_at IS NOT NULL AND ended_at IS NULL AND expires_at > $2),
    EXISTS (SELECT 1 FROM public.auth_core__user account
            WHERE account.id = $1 AND account.suspended = false AND `+standingHolder("$3")+`),
    EXISTS (SELECT 1 FROM public.auth_core__user account
            WHERE account.id = $1 AND account.suspended = false AND `+standingHolder("$4")+`)`,
		answer.ApproverUserID, at, AccessBreakglassApprove, SuperAdminPermission).Scan(&elevated, &eligible, &superAdmin); err != nil {
		return request, fmt.Errorf("answer elevation request: read the approver: %w", err)
	}
	switch {
	case elevated:
		return request, ErrElevationApproverElevated
	case !eligible:
		return request, ErrElevationApproverNotEligible
	}
	var decided ElevationRecord
	err = withSavepoint(ctx, tx, func(sp pgx.Tx) error {
		var err error
		action := securityaudit.ElevationApproved
		var reasons []string
		if answer.Approve {
			level := ElevationLevelAdmin
			if superAdmin {
				level = ElevationLevelSuperAdmin
			}
			decided, err = scanElevation(sp.QueryRow(ctx, `
UPDATE elitea_auth.admin_elevations
SET decision = 'approved', decided_at = $2, level = $3, approver_user_id = $4
WHERE id = $1 AND decision IS NULL
RETURNING `+elevationColumns, request.ID, at, level, answer.ApproverUserID))
		} else {
			action = securityaudit.ElevationDenied
			reasons = []string{"denial_reason:" + answer.DenialReasonCode}
			decided, err = scanElevation(sp.QueryRow(ctx, `
UPDATE elitea_auth.admin_elevations
SET decision = 'denied', decided_at = $2, approver_user_id = $3, denial_reason_code = $4
WHERE id = $1 AND decision IS NULL
RETURNING `+elevationColumns, request.ID, at, answer.ApproverUserID, answer.DenialReasonCode))
		}
		if err != nil {
			return fmt.Errorf("answer elevation request: %w", err)
		}
		return AppendElevationEvent(ctx, sp, appender, ElevationEvent{
			Action:            action,
			Source:            source,
			Elevation:         decided,
			ActorUserID:       answer.ApproverUserID,
			AuthenticationRef: sessionAuthenticationRef(answer.ApproverSessionRef),
			CorrelationID:     answer.CorrelationID,
			Reasons:           reasons,
		})
	})
	if err != nil {
		return request, err
	}
	return decided, nil
}

// requesterEligible refuses, in this order, a person who is not an
// unsuspended account (ErrElevationPerson), a session reference that is not a
// live session of the person (ErrElevationSession) and a person who does not
// hold access.breakglass.request through a standing role
// (ErrElevationRequester).
func requesterEligible(ctx context.Context, q Querier, userID, sessionRef int64, at time.Time) error {
	var person, session, requester bool
	if err := q.QueryRow(ctx, `
SELECT
    EXISTS (SELECT 1 FROM public.auth_core__user WHERE id = $2 AND suspended = false),
    `+liveSessionPredicate+`,
    EXISTS (SELECT 1 FROM public.auth_core__user account WHERE account.id = $2 AND `+standingHolder("$4")+`)`,
		sessionRef, userID, at, AccessBreakglassRequest).Scan(&person, &session, &requester); err != nil {
		return fmt.Errorf("read the person and the session: %w", err)
	}
	switch {
	case !person:
		return ErrElevationPerson
	case !session:
		return ErrElevationSession
	case !requester:
		return ErrElevationRequester
	}
	return nil
}

// liveSessionPredicate is "$1 names a live session of the person $2 at $3":
// a browser_sessions row of the person that is not revoked, ended or expired,
// or a form_sessions row of the person that has not expired. The ledger has
// no foreign key to either table, so this is the check that a request is
// never bound to another person's session. OFFSET 0 keeps the form_sessions
// row selection a fence the planner does not flatten, so the record of no
// other session is ever cast.
const liveSessionPredicate = `(EXISTS (
        SELECT 1 FROM elitea_auth.browser_sessions
        WHERE ref = $1 AND user_id = $2
          AND revoked_at IS NULL AND ended_at IS NULL AND expires_at > $3)
    OR EXISTS (
        SELECT 1 FROM (
            SELECT record FROM elitea_auth.form_sessions
            WHERE ref = $1 AND expires_at > $3
            OFFSET 0) session
        WHERE convert_from(session.record, 'UTF8')::jsonb ->> 'user_id' = $2::bigint::text))`

/* ── reads ─────────────────────────────────────────────────────────────── */

// RequestStateAt is the state of a row at now with the request lifetime: an
// undecided request past its lifetime reads as request_expired before the
// reconciler records it, so no reader shows an answerable request that
// cannot be answered.
func RequestStateAt(e ElevationRecord, now time.Time, lifetime time.Duration) string {
	state := e.State(now)
	if state == ElevationStateRequested && lifetime > 0 && !now.Before(e.RequestedAt.Add(lifetime)) {
		return ElevationStateRequestExpired
	}
	return state
}

// RequestDeadline is when an undecided request expires.
func RequestDeadline(e ElevationRecord, lifetime time.Duration) time.Time {
	return e.RequestedAt.Add(lifetime)
}

// LatestElevationRequestOf reads the person's latest request on the online
// path (approved or self), false when there is none.
func LatestElevationRequestOf(ctx context.Context, q Querier, userID int64) (ElevationRecord, bool, error) {
	request, err := scanElevation(q.QueryRow(ctx, `
SELECT `+elevationColumns+` FROM elitea_auth.admin_elevations
WHERE requester_user_id = $1 AND path IN ('approved', 'self')
ORDER BY requested_at DESC, id DESC LIMIT 1`, userID))
	if errors.Is(err, pgx.ErrNoRows) {
		return ElevationRecord{}, false, nil
	}
	if err != nil {
		return ElevationRecord{}, false, fmt.Errorf("read the latest elevation request: %w", err)
	}
	return request, true, nil
}

// pendingRequestsWhere is the queue of the approver $3 at $1 with the
// request lifetime $2: undecided requests within their lifetime that are not
// the approver's own.
const pendingRequestsWhere = `
WHERE decision IS NULL AND started_at IS NULL AND path = 'approved'
  AND requested_at > $1::timestamptz - make_interval(secs => $2)
  AND requester_user_id <> $3 AND user_id <> $3`

// PendingElevationRequests reads the queue of the approver excludeUserID:
// the undecided requests within their lifetime at now that are not theirs,
// oldest first, at most limit, and the size of the whole queue.
func PendingElevationRequests(ctx context.Context, q Querier, now time.Time, lifetime time.Duration, excludeUserID int64, limit int) ([]ElevationRecord, int, error) {
	var total int
	if err := q.QueryRow(ctx, `SELECT count(*) FROM elitea_auth.admin_elevations`+pendingRequestsWhere,
		now.UTC(), lifetime.Seconds(), excludeUserID).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count the pending elevation requests: %w", err)
	}
	rows, err := q.Query(ctx, `
SELECT `+elevationColumns+` FROM elitea_auth.admin_elevations`+pendingRequestsWhere+`
ORDER BY requested_at, id LIMIT $4`, now.UTC(), lifetime.Seconds(), excludeUserID, limit)
	if err != nil {
		return nil, 0, fmt.Errorf("read the pending elevation requests: %w", err)
	}
	pending, err := collectElevations(rows)
	if err != nil {
		return nil, 0, err
	}
	return pending, total, nil
}

// ElevationHistory reads the ledger newest first: requests and elevations of
// every path, limit rows from offset, and the total.
func ElevationHistory(ctx context.Context, q Querier, limit, offset int) ([]ElevationRecord, int, error) {
	var total int
	if err := q.QueryRow(ctx, `SELECT count(*) FROM elitea_auth.admin_elevations`).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count the elevation history: %w", err)
	}
	rows, err := q.Query(ctx, `
SELECT `+elevationColumns+` FROM elitea_auth.admin_elevations
ORDER BY requested_at DESC, id DESC LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("read the elevation history: %w", err)
	}
	history, err := collectElevations(rows)
	if err != nil {
		return nil, 0, err
	}
	return history, total, nil
}

// HoldsStandingPermission reports whether the unsuspended account userID
// holds permission through a standing administration role (an elevation
// never counts).
func HoldsStandingPermission(ctx context.Context, q Querier, userID int64, permission string) (bool, error) {
	var held bool
	if err := q.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM public.auth_core__user account
               WHERE account.id = $1 AND account.suspended = false AND `+standingHolder("$2")+`)`,
		userID, permission).Scan(&held); err != nil {
		return false, fmt.Errorf("read a standing permission: %w", err)
	}
	return held, nil
}

func collectElevations(rows pgx.Rows) ([]ElevationRecord, error) {
	defer rows.Close()
	var out []ElevationRecord
	for rows.Next() {
		elevation, err := scanElevation(rows)
		if err != nil {
			return nil, fmt.Errorf("read an elevation: %w", err)
		}
		out = append(out, elevation)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the elevations: %w", err)
	}
	return out, nil
}
