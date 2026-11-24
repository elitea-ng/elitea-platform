package adminaccess

// The elevation ledger of break-glass access (ADR-0059 decision 5, "It is an
// elevation, not a role", "Bound to one session"; story R2.1a, issue 2515).
//
// An elevation adds the administration permissions of the level's role to ONE
// browser session for a short time. Its row in elitea_auth.admin_elevations
// (shared 0269) is append-only by trigger: the person, the session reference,
// the reason, the level and the expiry never change once written; only the
// end and the review are filled in later, each once.
//
// Access is a predicate, never a job: an elevation is active while it has
// started, has no end, and its expiry is in the future (ElevationRecord.ActiveAt;
// the resolver of R2.1b puts the same predicate in its query). Nothing has to
// run for access to stop.
//
// StartElevation and EndElevation are the writers. Both run in the caller's
// transaction and write the step's `admin.elevation` security audit event
// (elevation_event.go; R2.3, issue 2517) on it, under one savepoint with the
// row: the row and its event commit together or not at all, and a failed
// append leaves no row even when the caller commits after the error. The
// first callers are R2.2c (issue 2597) and R2.7b (issue 2600).
//
// The resolver (R2.1b) must match an elevation on BOTH user_id and
// browser_session_ref, and must tolerate more than one open row for a
// session (a writer outside StartElevation's lock under REPEATABLE READ could
// produce one): the union of the levels' sets, never a pick of one row.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// The levels of an elevation. A level is the NAME of an administration role:
// the resolver joins it to the role at read time, so a later grant migration
// that names the role reaches an active elevation too. The table's CHECK
// constraint holds the same list.
const (
	ElevationLevelAdmin      = RoleAdmin
	ElevationLevelSuperAdmin = RoleSuperAdmin
)

// The paths an elevation came by.
const (
	// ElevationPathApproved: a second person approved the request.
	ElevationPathApproved = "approved"
	// ElevationPathSelf: approval was switched off, and the system approved
	// the request at the admin level.
	ElevationPathSelf = "self"
	// ElevationPathSealed: the sealed credential, while single sign-on is
	// down. The operator who issued it chose the level.
	ElevationPathSealed = "sealed"
)

// The reason codes a request gives.
const (
	ElevationReasonIncident       = "incident"
	ElevationReasonIdentityOutage = "identity_outage"
	ElevationReasonRecovery       = "recovery"
	ElevationReasonOther          = "other"
)

// The decisions recorded on a request.
const (
	ElevationDecisionApproved       = "approved"
	ElevationDecisionDenied         = "denied"
	ElevationDecisionRequestExpired = "request_expired"
)

// The reasons an elevation ended.
const (
	ElevationEndHolder          = "holder"
	ElevationEndApprover        = "approver"
	ElevationEndOperator        = "operator"
	ElevationEndExpired         = "expired"
	ElevationEndLogout          = "logout"
	ElevationEndSessionRevoked  = "session_revoked"
	ElevationEndUserSuspended   = "user_suspended"
	ElevationEndUserDeleted     = "user_deleted"
	ElevationEndSCIMDeactivated = "scim_deactivated"
)

// The review states stored on an ended elevation.
const (
	ElevationReviewPending        = "review_pending"
	ElevationReviewed             = "reviewed"
	ElevationUnreviewedNoReviewer = "unreviewed_no_reviewer"
)

// The states of a row, derived from its columns and the clock
// (ElevationRecord.State). Three are stored review states; the others are never a
// column, so that no job has to flip one for access to stop.
const (
	ElevationStateRequested            = "requested"
	ElevationStateApproved             = "approved"
	ElevationStateDenied               = "denied"
	ElevationStateRequestExpired       = "request_expired"
	ElevationStateActive               = "active"
	ElevationStateEnded                = "ended"
	ElevationStateExpired              = "expired"
	ElevationStateReviewPending        = ElevationReviewPending
	ElevationStateReviewed             = ElevationReviewed
	ElevationStateUnreviewedNoReviewer = ElevationUnreviewedNoReviewer
)

// The ceilings in code (ADR-0059 decision 5, owner decision 5). A deployment
// setting (R2.2a) can lower a duration and never raise it above these. The
// table's CHECK constraint holds the same bounds.
const (
	MaxElevationDurationOnline = 480 * time.Minute
	MaxElevationDurationSealed = 240 * time.Minute
)

// Errors of the ledger. Each refusal of StartElevation's input wraps
// ErrElevationInvalid.
var (
	ErrElevationInvalid = errors.New("elevation refused")
	// ErrElevationNoSession: the session reference is 0. A session without a
	// server-side row has no reference and can hold no elevation.
	ErrElevationNoSession   = fmt.Errorf("%w: no browser session reference", ErrElevationInvalid)
	ErrElevationLevel       = fmt.Errorf("%w: unknown level", ErrElevationInvalid)
	ErrElevationPath        = fmt.Errorf("%w: unknown path", ErrElevationInvalid)
	ErrElevationReasonCode  = fmt.Errorf("%w: unknown reason code", ErrElevationInvalid)
	ErrElevationEndReason   = fmt.Errorf("%w: unknown end reason", ErrElevationInvalid)
	ErrElevationNoTimestamp = fmt.Errorf("%w: no time given", ErrElevationInvalid)
	// ErrElevationNoAppender: no security audit appender. The ledger takes no
	// writer call that has no appender to record it with (decision 5, "Fail
	// closed on audit"; R2.3 writes the event).
	ErrElevationNoAppender = errors.New("elevation refused: no security audit appender")
	// ErrSessionAlreadyElevated: the session holds an active elevation. There
	// is no extension; more time is a new request after this one ends.
	ErrSessionAlreadyElevated = errors.New("elevation refused: the session already holds an active elevation")
	ErrElevationNotFound      = errors.New("elevation not found")
	// ErrElevationPerson: the person is not an unsuspended account.
	ErrElevationPerson = errors.New("elevation refused: the person is not an active account")
	// ErrElevationSession: the session reference names no live session of
	// the person.
	ErrElevationSession = errors.New("elevation refused: the session reference is not a live session of the person")
	// ErrElevationApproverLevel: a super_admin-level elevation on the
	// approved path whose approver does not hold the super_admin permission
	// through a standing role. The level is the approver's.
	ErrElevationApproverLevel = errors.New("elevation refused: the approver does not hold the super_admin level")
	// ErrElevationNotExpired: the end reason `expired` before the expiry.
	ErrElevationNotExpired = errors.New("elevation end refused: the elevation has not expired")
	// ErrElevationNotStarted: the row is a request that never started, so it
	// has nothing to end.
	ErrElevationNotStarted = errors.New("elevation has not started")
)

// ElevationRecord is one row of the ledger. A zero time or an empty string is a
// NULL column.
type ElevationRecord struct {
	ID                uuid.UUID
	UserID            int64
	BrowserSessionRef int64
	Level             string
	Path              string
	ReasonCode        string
	Reason            string
	Ticket            string
	RequesterUserID   int64
	ApproverUserID    int64
	RequestedAt       time.Time
	Decision          string
	DecidedAt         time.Time
	StartedAt         time.Time
	ExpiresAt         time.Time
	EndedAt           time.Time
	EndReason         string
	ReviewState       string
}

// ActiveAt reports whether the elevation grants anything at `now`: started,
// not ended, and not past its expiry. It is the predicate of the resolver's
// query, not a stored flag.
func (e ElevationRecord) ActiveAt(now time.Time) bool {
	return !e.StartedAt.IsZero() && e.EndedAt.IsZero() && now.Before(e.ExpiresAt)
}

// State is the row's state at `now`.
func (e ElevationRecord) State(now time.Time) string {
	switch {
	case e.ReviewState != "":
		return e.ReviewState
	case !e.EndedAt.IsZero():
		return ElevationStateEnded
	case !e.StartedAt.IsZero():
		if now.Before(e.ExpiresAt) {
			return ElevationStateActive
		}
		return ElevationStateExpired
	}
	switch e.Decision {
	case ElevationDecisionApproved:
		return ElevationStateApproved
	case ElevationDecisionDenied:
		return ElevationStateDenied
	case ElevationDecisionRequestExpired:
		return ElevationStateRequestExpired
	}
	return ElevationStateRequested
}

// ElevationStart is what starts an elevation.
type ElevationStart struct {
	// UserID is the person elevated.
	UserID int64
	// BrowserSessionRef is the principal's BrowserSessionRef: the session the
	// elevation is bound to. Never 0.
	BrowserSessionRef int64
	Level             string
	Path              string
	ReasonCode        string
	Reason            string
	Ticket            string
	// RequesterUserID is who asked; 0 means UserID.
	RequesterUserID int64
	// ApproverUserID is the second person on the approved path; 0 on the
	// self and sealed paths.
	ApproverUserID int64
	// RequestedAt and DecidedAt default to At.
	RequestedAt time.Time
	DecidedAt   time.Time
	// At is the start, from the caller's clock. Required: the ledger never
	// reads the wall clock itself, so a test clock reaches every row.
	At time.Time
	// Duration is the lifetime from At, at most the path's ceiling.
	Duration time.Duration
	// Source is the source of the `started` event (securityaudit
	// ElevationSource*); "" is the console. A process command passes
	// operator.
	Source string
	// CorrelationID ties the `started` event to the request (the trace id).
	CorrelationID string
}

// Validate checks the start without touching the database.
func (s ElevationStart) Validate() error {
	if s.BrowserSessionRef <= 0 {
		return ErrElevationNoSession
	}
	if s.UserID <= 0 || s.RequesterUserID < 0 || s.ApproverUserID < 0 {
		return fmt.Errorf("%w: a person is missing", ErrElevationInvalid)
	}
	switch s.Level {
	case ElevationLevelAdmin, ElevationLevelSuperAdmin:
	default:
		return fmt.Errorf("%w %q", ErrElevationLevel, s.Level)
	}
	requester := s.requester()
	switch s.Path {
	case ElevationPathApproved:
		if s.ApproverUserID == 0 {
			return fmt.Errorf("%w: the approved path needs an approver", ErrElevationInvalid)
		}
		if s.ApproverUserID == requester || s.ApproverUserID == s.UserID {
			return fmt.Errorf("%w: nobody approves the own elevation", ErrElevationInvalid)
		}
	case ElevationPathSelf, ElevationPathSealed:
		if s.ApproverUserID != 0 {
			return fmt.Errorf("%w: the %s path has no approver", ErrElevationInvalid, s.Path)
		}
		if s.Path == ElevationPathSelf && s.Level == ElevationLevelSuperAdmin {
			return fmt.Errorf("%w: the super_admin level needs a super_admin approver or the sealed path", ErrElevationInvalid)
		}
	default:
		return fmt.Errorf("%w %q", ErrElevationPath, s.Path)
	}
	switch s.ReasonCode {
	case ElevationReasonIncident, ElevationReasonIdentityOutage, ElevationReasonRecovery, ElevationReasonOther:
	default:
		return fmt.Errorf("%w %q", ErrElevationReasonCode, s.ReasonCode)
	}
	if strings.TrimSpace(s.Reason) == "" {
		return fmt.Errorf("%w: no reason text", ErrElevationInvalid)
	}
	if s.At.IsZero() {
		return ErrElevationNoTimestamp
	}
	ceiling := MaxElevationDurationOnline
	if s.Path == ElevationPathSealed {
		ceiling = MaxElevationDurationSealed
	}
	if s.Duration <= 0 || s.Duration > ceiling {
		return fmt.Errorf("%w: duration %s is outside (0, %s]", ErrElevationInvalid, s.Duration, ceiling)
	}
	requested, decided := s.requestedAt(), s.decidedAt()
	if decided.Before(requested) || s.At.Before(decided) {
		return fmt.Errorf("%w: the request, decision and start are out of order", ErrElevationInvalid)
	}
	return nil
}

func (s ElevationStart) requester() int64 {
	if s.RequesterUserID == 0 {
		return s.UserID
	}
	return s.RequesterUserID
}

func (s ElevationStart) requestedAt() time.Time {
	if s.RequestedAt.IsZero() {
		return s.At
	}
	return s.RequestedAt
}

func (s ElevationStart) decidedAt() time.Time {
	if s.DecidedAt.IsZero() {
		return s.At
	}
	return s.DecidedAt
}

// elevationColumns is the SELECT list scanElevation reads.
const elevationColumns = `id, user_id, browser_session_ref, COALESCE(level, ''), path, reason_code, reason,
    COALESCE(ticket, ''), requester_user_id, COALESCE(approver_user_id, 0), requested_at,
    COALESCE(decision, ''), decided_at, started_at, expires_at, ended_at,
    COALESCE(end_reason, ''), COALESCE(review_state, '')`

func scanElevation(row pgx.Row) (ElevationRecord, error) {
	var e ElevationRecord
	var decided, started, expires, ended *time.Time
	if err := row.Scan(&e.ID, &e.UserID, &e.BrowserSessionRef, &e.Level, &e.Path, &e.ReasonCode, &e.Reason,
		&e.Ticket, &e.RequesterUserID, &e.ApproverUserID, &e.RequestedAt,
		&e.Decision, &decided, &started, &expires, &ended,
		&e.EndReason, &e.ReviewState); err != nil {
		return ElevationRecord{}, err
	}
	for _, field := range []struct {
		from *time.Time
		to   *time.Time
	}{{decided, &e.DecidedAt}, {started, &e.StartedAt}, {expires, &e.ExpiresAt}, {ended, &e.EndedAt}} {
		if field.from != nil {
			*field.to = *field.from
		}
	}
	return e, nil
}

// StartElevation writes one started elevation, bound to the session
// reference, in the caller's transaction. The row is approved at insert: the
// sealed path (R2.7b) and the tests of the R2 stories start one directly, and
// the request routes (R2.2b, R2.2c) add the request-first writers beside it.
//
// It refuses, before it writes:
//   - an invalid start (Validate), before it touches the database;
//   - a person who is not an unsuspended account (ErrElevationPerson);
//   - a session reference that does not name a live session OF THAT PERSON
//     (ErrElevationSession): a browser_sessions row of the user that is not
//     revoked, ended or expired at At, or a form_sessions row of the user
//     that has not expired. The ledger has no foreign key to either table, so
//     this is the check that one person's elevation is never bound to
//     another person's session;
//   - on the approved path at the super_admin level, an approver who does not
//     hold admin.auth.users.super_admin through a STANDING administration
//     role (ErrElevationApproverLevel): nobody hands out more than they hold,
//     and an elevation never counts towards it;
//   - a session that already holds an active elevation
//     (ErrSessionAlreadyElevated), under a transaction-scoped lock on the
//     session; the table's trigger repeats the check.
//
// appender writes the `started` admin.elevation event (R2.3, issue 2517) on
// tx, under one savepoint with the INSERT: a failed append aborts the start
// and leaves no row.
func StartElevation(ctx context.Context, tx pgx.Tx, appender securityaudit.Appender, start ElevationStart) (ElevationRecord, error) {
	if err := start.Validate(); err != nil {
		return ElevationRecord{}, err
	}
	source := start.Source
	if source == "" {
		source = securityaudit.ElevationSourceConsole
	}
	if !knownElevationSource(source) {
		return ElevationRecord{}, fmt.Errorf("%w: unknown event source %q", ErrElevationInvalid, source)
	}
	if appender == nil {
		return ElevationRecord{}, ErrElevationNoAppender
	}
	if tx == nil {
		return ElevationRecord{}, errors.New("start elevation: no transaction")
	}
	at := start.At.UTC()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('admin_elevations:session:' || $1::bigint::text, 0))`,
		start.BrowserSessionRef); err != nil {
		return ElevationRecord{}, fmt.Errorf("start elevation: lock the session: %w", err)
	}
	var person, session, approverLevel, elevated bool
	if err := tx.QueryRow(ctx, `
SELECT
    EXISTS (SELECT 1 FROM public.auth_core__user WHERE id = $2 AND suspended = false),
    EXISTS (
        SELECT 1 FROM elitea_auth.browser_sessions
        WHERE ref = $1 AND user_id = $2
          AND revoked_at IS NULL AND ended_at IS NULL AND expires_at > $3)
    OR EXISTS (
        SELECT 1 FROM elitea_auth.form_sessions
        WHERE ref = $1 AND expires_at > $3
          AND convert_from(record, 'UTF8')::jsonb ->> 'user_id' = $2::bigint::text),
    $4::bigint = 0 OR EXISTS (
        SELECT 1
        FROM public.auth_core__user account
        JOIN public.auth_core__user_role assignment ON assignment.user_id = account.id
        JOIN public.auth_core__role role ON role.id = assignment.role_id AND role.mode = $5
        JOIN public.auth_core__role_permission grant_row
          ON grant_row.role_id = role.id AND grant_row.permission = $6
        WHERE account.id = $4 AND account.suspended = false),
    EXISTS (
        SELECT 1 FROM elitea_auth.admin_elevations
        WHERE browser_session_ref = $1
          AND started_at IS NOT NULL AND ended_at IS NULL AND expires_at > $3)`,
		start.BrowserSessionRef, start.UserID, at, start.superAdminApprover(),
		ModeAdministration, SuperAdminPermission).Scan(&person, &session, &approverLevel, &elevated); err != nil {
		return ElevationRecord{}, fmt.Errorf("start elevation: read the person, the session and the approver: %w", err)
	}
	switch {
	case !person:
		return ElevationRecord{}, ErrElevationPerson
	case !session:
		return ElevationRecord{}, ErrElevationSession
	case !approverLevel:
		return ElevationRecord{}, ErrElevationApproverLevel
	case elevated:
		return ElevationRecord{}, ErrSessionAlreadyElevated
	}
	var elevation ElevationRecord
	err := withSavepoint(ctx, tx, func(sp pgx.Tx) error {
		var err error
		elevation, err = scanElevation(sp.QueryRow(ctx, `
INSERT INTO elitea_auth.admin_elevations
    (user_id, browser_session_ref, level, path, reason_code, reason, ticket,
     requester_user_id, approver_user_id, requested_at, decision, decided_at, started_at, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, ''), $8, NULLIF($9::bigint, 0), $10, 'approved', $11, $12, $13)
RETURNING `+elevationColumns,
			start.UserID, start.BrowserSessionRef, start.Level, start.Path, start.ReasonCode, start.Reason,
			strings.TrimSpace(start.Ticket), start.requester(), start.ApproverUserID,
			start.requestedAt().UTC(), start.decidedAt().UTC(), at, at.Add(start.Duration)))
		if err != nil {
			return fmt.Errorf("start elevation: %w", err)
		}
		return AppendElevationEvent(ctx, sp, appender, ElevationEvent{
			Action:            securityaudit.ElevationStarted,
			Source:            source,
			Elevation:         elevation,
			ActorUserID:       start.UserID,
			AuthenticationRef: sessionAuthenticationRef(start.BrowserSessionRef),
			CorrelationID:     start.CorrelationID,
		})
	})
	if err != nil {
		return ElevationRecord{}, err
	}
	return elevation, nil
}

// superAdminApprover is the approver whose standing super_admin permission
// the start must check: the approver of a super_admin-level elevation on the
// approved path, 0 otherwise (the admin level needs no such permission, and
// on the sealed path the operator who issued the credential chose the level).
func (s ElevationStart) superAdminApprover() int64 {
	if s.Path == ElevationPathApproved && s.Level == ElevationLevelSuperAdmin {
		return s.ApproverUserID
	}
	return 0
}

// ElevationEnd is what ends an elevation.
type ElevationEnd struct {
	ID     uuid.UUID
	Reason string
	// At is the end, from the caller's clock. Required.
	At time.Time
	// ActorUserID is who ended it; 0 for the platform or an operator. 0 with
	// the reason `holder` is the elevated person; the reason `approver`
	// requires it.
	ActorUserID int64
	// Source is the source of the event (securityaudit ElevationSource*);
	// "" takes the requested end reason's (defaultEndSource). A process
	// command passes operator.
	Source string
	// AuthenticationRef and CorrelationID are the event's: the session or
	// token row behind the end, and the trace id.
	AuthenticationRef string
	CorrelationID     string
}

// EndElevation records the end of a started elevation, once, and moves it to
// review_pending, in the caller's transaction. It reports whether it wrote
// anything: a second end changes nothing, writes no event, and returns the
// row as it is, with false.
//
// The stored end reason says why ACCESS stopped. An elevation that had
// already expired at At ends at its expiry with the reason `expired`,
// whatever reason the caller gave; the caller's own reason is then a reason
// code of the event (`requested_end_reason:<reason>`), not a ledger column.
// The reason `expired` before the expiry is refused (ErrElevationNotExpired):
// it would record a clean timeout for an elevation that something else ended.
//
// The end and its `admin.elevation` event (R2.3) are written under one
// savepoint: `expired` when the stored end reason is `expired`, `ended`
// otherwise. A failed append leaves the elevation open.
func EndElevation(ctx context.Context, tx pgx.Tx, appender securityaudit.Appender, end ElevationEnd) (ElevationRecord, bool, error) {
	if !knownElevationEndReason(end.Reason) {
		return ElevationRecord{}, false, fmt.Errorf("%w %q", ErrElevationEndReason, end.Reason)
	}
	if end.At.IsZero() {
		return ElevationRecord{}, false, ErrElevationNoTimestamp
	}
	if end.Source != "" && !knownElevationSource(end.Source) {
		return ElevationRecord{}, false, fmt.Errorf("%w: unknown event source %q", ErrElevationInvalid, end.Source)
	}
	if end.Reason == ElevationEndApprover && end.ActorUserID <= 0 {
		// An approver's end names the approver: the event must say who.
		return ElevationRecord{}, false, fmt.Errorf("%w: an approver end names the approver", ErrElevationInvalid)
	}
	if appender == nil {
		return ElevationRecord{}, false, ErrElevationNoAppender
	}
	if tx == nil {
		return ElevationRecord{}, false, errors.New("end elevation: no transaction")
	}
	var elevation ElevationRecord
	err := withSavepoint(ctx, tx, func(sp pgx.Tx) error {
		var err error
		elevation, err = scanElevation(sp.QueryRow(ctx, `
UPDATE elitea_auth.admin_elevations
SET ended_at     = GREATEST(started_at, LEAST($2::timestamptz, expires_at)),
    end_reason   = CASE WHEN $2::timestamptz >= expires_at THEN 'expired' ELSE $3::text END,
    review_state = COALESCE(review_state, 'review_pending')
WHERE id = $1 AND started_at IS NOT NULL AND ended_at IS NULL
  AND ($3::text <> 'expired' OR $2::timestamptz >= expires_at)
RETURNING `+elevationColumns, end.ID, end.At.UTC(), end.Reason))
		if err != nil {
			return err
		}
		action := securityaudit.ElevationEnded
		if elevation.EndReason == ElevationEndExpired {
			action = securityaudit.ElevationExpired
		}
		var reasons []string
		if elevation.EndReason != end.Reason {
			reasons = []string{"requested_end_reason:" + end.Reason}
		}
		// The actor and the source are who ASKED for the end, so they agree
		// with each other: a holder who ends a lapsed elevation is the actor
		// of its `expired` event, from the console, and the reason code
		// says what they asked for.
		source := end.Source
		if source == "" {
			source = defaultEndSource(end.Reason)
		}
		actor := end.ActorUserID
		if actor == 0 && end.Reason == ElevationEndHolder {
			actor = elevation.UserID
		}
		return AppendElevationEvent(ctx, sp, appender, ElevationEvent{
			Action:            action,
			Source:            source,
			Elevation:         elevation,
			ActorUserID:       actor,
			AuthenticationRef: end.AuthenticationRef,
			CorrelationID:     end.CorrelationID,
			Reasons:           reasons,
		})
	})
	if err == nil {
		return elevation, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return ElevationRecord{}, false, fmt.Errorf("end elevation: %w", err)
	}
	current, err := ReadElevation(ctx, tx, end.ID)
	if err != nil {
		return ElevationRecord{}, false, err
	}
	if current.StartedAt.IsZero() {
		return ElevationRecord{}, false, ErrElevationNotStarted
	}
	if current.EndedAt.IsZero() {
		// Not ended, so the expired-before-the-expiry guard refused it.
		return ElevationRecord{}, false, ErrElevationNotExpired
	}
	return current, false, nil
}

// ReadElevation reads one row of the ledger.
func ReadElevation(ctx context.Context, q Querier, id uuid.UUID) (ElevationRecord, error) {
	elevation, err := scanElevation(q.QueryRow(ctx, `
SELECT `+elevationColumns+` FROM elitea_auth.admin_elevations WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return ElevationRecord{}, ErrElevationNotFound
	}
	if err != nil {
		return ElevationRecord{}, fmt.Errorf("read elevation: %w", err)
	}
	return elevation, nil
}
