package adminaccess

// Review after use (ADR-0059 decision 5, "Review after use"; story R2.9a,
// issue 2523).
//
// Every elevation is reviewed after it ends by someone who was not involved.
// The review is an ATTESTATION: it moves the ledger row from review_pending to
// reviewed and records an outcome and a note in elitea_auth.admin_elevation_items
// (shared 0283). It changes no platform state.
//
//   - THE REVIEWERS ARE FIXED AT THE END. EndElevation records, in its own
//     savepoint, every standing holder of access.breakglass.review (an
//     unsuspended account, an assignable role) other than the holder and the
//     approver. Eligibility for THIS review is read from those rows, never
//     from the current roles: nobody can remove the reviewers and wait.
//   - A PENDING REVIEWER IS PROTECTED. While a review is pending, taking the
//     review permission from one of its reviewers, suspending or deleting
//     them needs the STANDING admin.auth.users.super_admin
//     (GuardPendingReviewer); the change is listed in the review, and the
//     person stays eligible.
//   - `unauthorized` opens a revert list: one entry per flagged change of the
//     elevation (an admin audit row with elevation_flagged), for the platform
//     administrators, and notifies as an activation does
//     (NoticeReviewUnauthorized). The provisional grants of R2.12a and R2.12b
//     stay revoked; they are confirmed only by a `justified` review.
//   - After a sealed sign-in the review records whether a replacement
//     credential was issued.
//   - The `reviewed` event commits with the attestation. It carries the
//     outcome as a reason code, NEVER the note text (decision 8).

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// The two outcomes of a review.
const (
	ReviewOutcomeJustified    = "justified"
	ReviewOutcomeUnauthorized = "unauthorized"
)

// The kinds of the items of a review (shared 0283).
const (
	ReviewItemReviewer       = "reviewer"
	ReviewItemReviewerChange = "reviewer_change"
	ReviewItemReview         = "review"
	ReviewItemRevert         = "revert"
	ReviewItemNote           = "note"
	// ReviewItemOverdue: the review was raised as overdue (R2.9b), at most
	// once a day.
	ReviewItemOverdue = "overdue"
)

// The changes to a pending reviewer that GuardPendingReviewer lists.
const (
	ReviewerChangeRoleRemoved = "role_removed"
	ReviewerChangeSuspended   = "suspended"
	ReviewerChangeDeleted     = "deleted"
)

// MaxReviewNoteLength is the longest review note, in characters.
const MaxReviewNoteLength = 4000

// CodeElevationPendingReviewer is the refusal of a change to a pending
// reviewer by a caller without the standing super_admin permission.
const CodeElevationPendingReviewer = "elevation_pending_reviewer"

var (
	// ErrElevationNotReviewable: the elevation is not awaiting review (it has
	// not ended, or it was reviewed already).
	ErrElevationNotReviewable = errors.New("elevation review refused: the elevation is not awaiting review")
	// ErrReviewerInvolved: the holder and the approver do not review.
	ErrReviewerInvolved = errors.New("elevation review refused: the holder and the approver do not review")
	// ErrReviewerNotEligible: the person is not among the reviewers recorded
	// when the elevation ended.
	ErrReviewerNotEligible = errors.New("elevation review refused: not an eligible reviewer of this elevation")
	// ErrRevertNotFound: no revert entry has the id.
	ErrRevertNotFound = errors.New("revert entry not found")
)

// PendingReviewerRefusal is the refusal of a change to a pending reviewer.
func PendingReviewerRefusal() *Refusal {
	return &Refusal{Status: http.StatusForbidden, Code: CodeElevationPendingReviewer,
		Message: "this person is a reviewer of an elevation awaiting review: only a super_admin can take the " +
			"review permission from them, suspend or delete them, and the change is listed in the review"}
}

// recordReviewers records the eligible reviewers of e, ended at `at`, on sp
// (the end's savepoint): the standing holders of access.breakglass.review but
// the people involved (involved).
func recordReviewers(ctx context.Context, sp pgx.Tx, e ElevationRecord, at time.Time) error {
	if _, err := sp.Exec(ctx, `
INSERT INTO elitea_auth.admin_elevation_items (elevation_id, kind, user_id, created_at)
SELECT $1, '`+ReviewItemReviewer+`', account.id, $3
FROM public.auth_core__user account
WHERE account.suspended = false
  AND account.id <> ALL($2::bigint[])
  AND `+standingHolder("$4")+`
ORDER BY account.id`, e.ID, involved(e), at.UTC(), AccessBreakglassReview); err != nil {
		return fmt.Errorf("record the reviewers: %w", err)
	}
	return nil
}

// involved are the people of the elevation who never review it: the holder,
// the requester and the approver.
func involved(e ElevationRecord) []int64 {
	out := []int64{e.UserID}
	for _, id := range []int64{e.RequesterUserID, e.ApproverUserID} {
		if id > 0 {
			out = append(out, id)
		}
	}
	return out
}

// InvolvedIn reports whether userID is the holder, the requester or the
// approver of the elevation.
func InvolvedIn(e ElevationRecord, userID int64) bool {
	for _, id := range involved(e) {
		if id == userID {
			return true
		}
	}
	return false
}

// ElevationReviewerIDs answers the reviewers recorded for the elevation.
func ElevationReviewerIDs(ctx context.Context, q Querier, id uuid.UUID) ([]int64, error) {
	rows, err := q.Query(ctx, `
SELECT user_id FROM elitea_auth.admin_elevation_items
WHERE elevation_id = $1 AND kind = '`+ReviewItemReviewer+`' ORDER BY user_id`, id)
	if err != nil {
		return nil, fmt.Errorf("read the reviewers: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return nil, fmt.Errorf("read the reviewers: %w", err)
	}
	return ids, nil
}

// ElevationReview is one attestation.
type ElevationReview struct {
	ID             uuid.UUID
	ReviewerUserID int64
	// Outcome is ReviewOutcomeJustified or ReviewOutcomeUnauthorized.
	Outcome string
	// Note is the free text; it lives in the items table only.
	Note string
	// ReplacementIssued is required for a sealed elevation and refused for
	// any other.
	ReplacementIssued *bool
	At                time.Time
	// Source, AuthenticationRef and CorrelationID are the event's.
	Source            string
	AuthenticationRef string
	CorrelationID     string
}

// ReviewResult is what a review wrote.
type ReviewResult struct {
	Elevation ElevationRecord
	// Reverts is the number of revert entries an `unauthorized` outcome
	// opened.
	Reverts int
}

// ReviewElevation records the attestation, on tx: the ledger row moves to
// reviewed, the review item is written, an `unauthorized` outcome opens the
// revert list and writes its notices, and the `reviewed` event is appended.
// Any failure rolls the whole review back.
func ReviewElevation(ctx context.Context, tx pgx.Tx, appender securityaudit.Appender, review ElevationReview) (ReviewResult, error) {
	switch {
	case review.Outcome != ReviewOutcomeJustified && review.Outcome != ReviewOutcomeUnauthorized:
		return ReviewResult{}, fmt.Errorf("%w: unknown outcome %q", ErrElevationInvalid, review.Outcome)
	case utf8.RuneCountInString(review.Note) > MaxReviewNoteLength:
		return ReviewResult{}, fmt.Errorf("%w: the note is longer than %d characters", ErrElevationInvalid, MaxReviewNoteLength)
	case review.At.IsZero():
		return ReviewResult{}, ErrElevationNoTimestamp
	case review.ReviewerUserID <= 0:
		return ReviewResult{}, ErrReviewerNotEligible
	case review.Source != "" && !knownElevationSource(review.Source):
		return ReviewResult{}, fmt.Errorf("%w: unknown event source %q", ErrElevationInvalid, review.Source)
	case appender == nil:
		return ReviewResult{}, ErrElevationNoAppender
	case tx == nil:
		return ReviewResult{}, errors.New("review elevation: no transaction")
	}
	elevation, err := scanElevation(tx.QueryRow(ctx, `
SELECT `+elevationColumns+` FROM elitea_auth.admin_elevations WHERE id = $1 FOR UPDATE`, review.ID))
	if errors.Is(err, pgx.ErrNoRows) {
		return ReviewResult{}, ErrElevationNotFound
	}
	if err != nil {
		return ReviewResult{}, fmt.Errorf("review elevation: %w", err)
	}
	if elevation.ReviewState != ElevationReviewPending {
		return ReviewResult{}, ErrElevationNotReviewable
	}
	if InvolvedIn(elevation, review.ReviewerUserID) {
		return ReviewResult{}, ErrReviewerInvolved
	}
	var eligible bool
	if err := tx.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM elitea_auth.admin_elevation_items
               WHERE elevation_id = $1 AND kind = '`+ReviewItemReviewer+`' AND user_id = $2)`,
		review.ID, review.ReviewerUserID).Scan(&eligible); err != nil {
		return ReviewResult{}, fmt.Errorf("review elevation: read the reviewers: %w", err)
	}
	if !eligible {
		return ReviewResult{}, ErrReviewerNotEligible
	}
	sealed := elevation.Path == ElevationPathSealed
	if sealed != (review.ReplacementIssued != nil) {
		if sealed {
			return ReviewResult{}, fmt.Errorf("%w: the review of a sealed elevation says whether a replacement "+
				"credential was issued", ErrElevationInvalid)
		}
		return ReviewResult{}, fmt.Errorf("%w: only the review of a sealed elevation records a replacement "+
			"credential", ErrElevationInvalid)
	}

	at := review.At.UTC()
	elevation, err = scanElevation(tx.QueryRow(ctx, `
UPDATE elitea_auth.admin_elevations SET review_state = '`+ElevationReviewed+`'
WHERE id = $1 AND review_state = '`+ElevationReviewPending+`'
RETURNING `+elevationColumns, review.ID))
	if err != nil {
		return ReviewResult{}, fmt.Errorf("review elevation: %w", err)
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO elitea_auth.admin_elevation_items
    (elevation_id, kind, user_id, outcome, note, replacement_issued, created_at)
VALUES ($1, '`+ReviewItemReview+`', $2, $3, NULLIF($4, ''), $5, $6)`,
		review.ID, review.ReviewerUserID, review.Outcome, strings.TrimSpace(review.Note), review.ReplacementIssued, at); err != nil {
		return ReviewResult{}, fmt.Errorf("review elevation: record the review: %w", err)
	}
	result := ReviewResult{Elevation: elevation}
	if review.Outcome == ReviewOutcomeUnauthorized {
		result.Reverts, err = openRevertList(ctx, tx, review.ID, at)
		if err != nil {
			return ReviewResult{}, err
		}
		if _, err := WriteElevationNotices(ctx, tx, ElevationNotice{
			Kind: NoticeReviewUnauthorized, Elevation: elevation, ActorUserID: review.ReviewerUserID, At: at,
		}); err != nil {
			return ReviewResult{}, err
		}
	}
	reasons := []string{"outcome:" + review.Outcome}
	if review.ReplacementIssued != nil {
		reasons = append(reasons, "replacement_issued:"+strconv.FormatBool(*review.ReplacementIssued))
	}
	source := review.Source
	if source == "" {
		source = securityaudit.ElevationSourceConsole
	}
	if err := AppendElevationEvent(ctx, tx, appender, ElevationEvent{
		Action:            securityaudit.ElevationReviewed,
		Source:            source,
		Elevation:         elevation,
		ActorUserID:       review.ReviewerUserID,
		AuthenticationRef: review.AuthenticationRef,
		CorrelationID:     review.CorrelationID,
		Reasons:           reasons,
	}); err != nil {
		return ReviewResult{}, err
	}
	return result, nil
}

// openRevertList writes one revert entry per flagged admin audit row of the
// elevation.
func openRevertList(ctx context.Context, tx pgx.Tx, id uuid.UUID, at time.Time) (int, error) {
	present, err := relationPresent(ctx, tx, "centry.audit_events")
	if err != nil || !present {
		return 0, err
	}
	tag, err := tx.Exec(ctx, `
INSERT INTO elitea_auth.admin_elevation_items (elevation_id, kind, audit_event_id, action, created_at)
SELECT $1, '`+ReviewItemRevert+`', row.id, row.action, $2
FROM centry.audit_events row
WHERE row.elevation_id = $1 AND row.elevation_flagged IS TRUE
ORDER BY row.timestamp, row.id`, id, at)
	if err != nil {
		return 0, fmt.Errorf("review elevation: open the revert list: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// GuardPendingReviewer applies the pending-reviewer rule to a change of
// targetID (ReviewerChange*) on tx, the write's transaction: when the target
// is a recorded reviewer of an elevation awaiting review, a caller without the
// standing super_admin permission is refused, and a super_admin's change is
// listed in each such review. A role removal calls it only when the roles
// taken grant access.breakglass.review (RolesGrant).
func GuardPendingReviewer(ctx context.Context, tx ExecQuerier, caller Caller, targetID int64, change string, at time.Time) (*Refusal, error) {
	if tx == nil || targetID <= 0 {
		return nil, nil
	}
	present, err := relationPresent(ctx, tx, "elitea_auth.admin_elevation_items")
	if err != nil || !present {
		return nil, err
	}
	rows, err := tx.Query(ctx, `
SELECT item.elevation_id
FROM elitea_auth.admin_elevation_items item
JOIN elitea_auth.admin_elevations elevation ON elevation.id = item.elevation_id
WHERE item.kind = '`+ReviewItemReviewer+`' AND item.user_id = $1
  AND elevation.review_state = '`+ElevationReviewPending+`'
ORDER BY item.elevation_id
FOR SHARE OF elevation`, targetID)
	if err != nil {
		return nil, fmt.Errorf("read the pending reviews of the person: %w", err)
	}
	pending, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, fmt.Errorf("read the pending reviews of the person: %w", err)
	}
	if len(pending) == 0 {
		return nil, nil
	}
	if !caller.SuperAdmin {
		return PendingReviewerRefusal(), nil
	}
	if at.IsZero() {
		at = time.Now()
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO elitea_auth.admin_elevation_items (elevation_id, kind, user_id, actor_user_id, action, created_at)
SELECT elevation_id, '`+ReviewItemReviewerChange+`', $2, $3, $4, $5 FROM unnest($1::uuid[]) AS elevation_id`,
		pending, targetID, caller.UserID, change, at.UTC()); err != nil {
		return nil, fmt.Errorf("list the change in the pending reviews: %w", err)
	}
	return nil, nil
}

// ExecQuerier is a transaction: it reads and writes.
type ExecQuerier interface {
	Querier
	Execer
}

// RolesGrant reports whether one of the central roles grants permission in
// mode.
func RolesGrant(ctx context.Context, q Querier, mode string, roles []string, permission string) (bool, error) {
	permissions, err := CentralRolePermissions(ctx, q, mode, roles)
	if err != nil {
		return false, err
	}
	for _, p := range permissions {
		if p == permission {
			return true, nil
		}
	}
	return false, nil
}

// RolesTakePermission reports whether a write that takes the central roles
// `taken` from userID in mode and gives `given` leaves the person without
// permission: a taken role grants it, and neither a given role nor another
// role the person keeps does.
func RolesTakePermission(ctx context.Context, q Querier, mode string, userID int64,
	taken, given []string, permission string,
) (bool, error) {
	takes, err := RolesGrant(ctx, q, mode, taken, permission)
	if err != nil || !takes {
		return false, err
	}
	gives, err := RolesGrant(ctx, q, mode, given, permission)
	if err != nil || gives {
		return false, err
	}
	var kept bool
	if err := q.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1
    FROM public.auth_core__user_role assignment
    JOIN public.auth_core__role role ON role.id = assignment.role_id AND role.mode = $2
    JOIN public.auth_core__role_permission grant_row
      ON grant_row.role_id = role.id AND grant_row.permission = $4
    WHERE assignment.user_id = $1 AND NOT (role.name = ANY($3::text[])))`,
		userID, mode, taken, permission).Scan(&kept); err != nil {
		return false, fmt.Errorf("read the roles the person keeps: %w", err)
	}
	return !kept, nil
}

/* ── the reads ─────────────────────────────────────────────────────────── */

// ReviewItem is one row of elitea_auth.admin_elevation_items. A zero value
// is a NULL column.
type ReviewItem struct {
	ID                uuid.UUID
	ElevationID       uuid.UUID
	Kind              string
	UserID            int64
	ActorUserID       int64
	Action            string
	Outcome           string
	Note              string
	ReplacementIssued *bool
	AuditEventID      int64
	CreatedAt         time.Time
	HandledAt         time.Time
	HandledBy         int64
}

const reviewItemColumns = `item.id, item.elevation_id, item.kind, COALESCE(item.user_id, 0), COALESCE(item.actor_user_id, 0),
       COALESCE(item.action, ''), COALESCE(item.outcome, ''), COALESCE(item.note, ''), item.replacement_issued,
       COALESCE(item.audit_event_id, 0), item.created_at, item.handled_at, COALESCE(item.handled_by, 0)`

func scanReviewItem(row pgx.CollectableRow) (ReviewItem, error) {
	var item ReviewItem
	var handled *time.Time
	if err := row.Scan(&item.ID, &item.ElevationID, &item.Kind, &item.UserID, &item.ActorUserID, &item.Action,
		&item.Outcome, &item.Note, &item.ReplacementIssued, &item.AuditEventID, &item.CreatedAt, &handled,
		&item.HandledBy); err != nil {
		return ReviewItem{}, err
	}
	item.CreatedAt = item.CreatedAt.UTC()
	if handled != nil {
		item.HandledAt = handled.UTC()
	}
	return item, nil
}

// ElevationReviewItems answers every item of the elevation's review, oldest
// first.
func ElevationReviewItems(ctx context.Context, q Querier, id uuid.UUID) ([]ReviewItem, error) {
	rows, err := q.Query(ctx, `
SELECT `+reviewItemColumns+` FROM elitea_auth.admin_elevation_items item
WHERE item.elevation_id = $1 ORDER BY item.created_at, item.kind, item.id`, id)
	if err != nil {
		return nil, fmt.Errorf("read the review items: %w", err)
	}
	items, err := pgx.CollectRows(rows, scanReviewItem)
	if err != nil {
		return nil, fmt.Errorf("read the review items: %w", err)
	}
	return items, nil
}

// ElevationAuditRow is one admin audit row made under an elevation.
type ElevationAuditRow struct {
	ID         int64
	At         time.Time
	UserID     int64
	Action     string
	Method     string
	Route      string
	Status     int
	EntityType string
	EntityID   int64
	EntityName string
	Flagged    bool
}

// ElevationAuditRows answers a page of the admin audit rows with the
// elevation's id, flagged rows first, then by time, and the total.
func ElevationAuditRows(ctx context.Context, q Querier, id uuid.UUID, limit, offset int) ([]ElevationAuditRow, int, error) {
	present, err := relationPresent(ctx, q, "centry.audit_events")
	if err != nil || !present {
		return []ElevationAuditRow{}, 0, err
	}
	rows, err := q.Query(ctx, `
SELECT id, timestamp, COALESCE(user_id, 0), action, COALESCE(http_method, ''), COALESCE(http_route, ''),
       COALESCE(status_code, 0), COALESCE(entity_type, ''), COALESCE(entity_id, 0), COALESCE(entity_name, ''),
       COALESCE(elevation_flagged, false), count(*) OVER ()
FROM centry.audit_events
WHERE elevation_id = $1
ORDER BY COALESCE(elevation_flagged, false) DESC, timestamp, id
LIMIT $2 OFFSET $3`, id, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("read the audit rows of the elevation: %w", err)
	}
	total := 0
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (ElevationAuditRow, error) {
		var r ElevationAuditRow
		var status int16
		var entityID int32
		var userID int32
		err := row.Scan(&r.ID, &r.At, &userID, &r.Action, &r.Method, &r.Route, &status, &r.EntityType, &entityID,
			&r.EntityName, &r.Flagged, &total)
		r.At, r.Status, r.EntityID, r.UserID = r.At.UTC(), int(status), int64(entityID), int64(userID)
		return r, err
	})
	if err != nil {
		return nil, 0, fmt.Errorf("read the audit rows of the elevation: %w", err)
	}
	if len(out) == 0 && offset > 0 {
		// A page past the end still answers the total.
		if err := q.QueryRow(ctx, `SELECT count(*) FROM centry.audit_events WHERE elevation_id = $1`, id).Scan(&total); err != nil {
			return nil, 0, fmt.Errorf("count the audit rows of the elevation: %w", err)
		}
	}
	return out, total, nil
}

// RevertEntry is one entry of the revert list, with its change.
type RevertEntry struct {
	ReviewItem
	Change ElevationAuditRow
	// HolderUserID is the person whose elevation made the change.
	HolderUserID int64
}

// RevertList answers a page of the revert list, unhandled entries first, then
// oldest first, and the total. open limits it to the unhandled entries.
func RevertList(ctx context.Context, q Querier, open bool, limit, offset int) ([]RevertEntry, int, error) {
	// Without the admin audit table no entry was ever opened (openRevertList).
	present, err := relationPresent(ctx, q, "centry.audit_events")
	if err != nil || !present {
		return nil, 0, err
	}
	rows, err := q.Query(ctx, `
SELECT `+reviewItemColumns+`, elevation.user_id,
       COALESCE(change.timestamp, item.created_at), COALESCE(change.user_id, 0), COALESCE(change.http_method, ''),
       COALESCE(change.http_route, ''), COALESCE(change.status_code, 0), COALESCE(change.entity_type, ''),
       COALESCE(change.entity_id, 0), COALESCE(change.entity_name, ''), count(*) OVER ()
FROM elitea_auth.admin_elevation_items item
JOIN elitea_auth.admin_elevations elevation ON elevation.id = item.elevation_id
LEFT JOIN centry.audit_events change ON change.id = item.audit_event_id
WHERE item.kind = '`+ReviewItemRevert+`' AND (NOT $1 OR item.handled_at IS NULL)
ORDER BY item.handled_at IS NOT NULL, item.created_at, item.id
LIMIT $2 OFFSET $3`, open, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("read the revert list: %w", err)
	}
	total := 0
	entries, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (RevertEntry, error) {
		var e RevertEntry
		var handled *time.Time
		var status int16
		var entityID, userID int32
		err := row.Scan(&e.ID, &e.ElevationID, &e.Kind, &e.UserID, &e.ActorUserID, &e.Action, &e.Outcome, &e.Note,
			&e.ReplacementIssued, &e.AuditEventID, &e.CreatedAt, &handled, &e.HandledBy, &e.HolderUserID,
			&e.Change.At, &userID, &e.Change.Method, &e.Change.Route, &status, &e.Change.EntityType, &entityID,
			&e.Change.EntityName, &total)
		e.CreatedAt, e.Change.At = e.CreatedAt.UTC(), e.Change.At.UTC()
		if handled != nil {
			e.HandledAt = handled.UTC()
		}
		e.Change.ID, e.Change.Action, e.Change.Flagged = e.AuditEventID, e.Action, true
		e.Change.Status, e.Change.EntityID, e.Change.UserID = int(status), int64(entityID), int64(userID)
		return e, err
	})
	if err != nil {
		return nil, 0, fmt.Errorf("read the revert list: %w", err)
	}
	return entries, total, nil
}

// MarkRevertHandled marks the revert entry handled by `by` at `at`, once. It
// reports whether it changed anything: an entry already handled keeps its
// first mark.
func MarkRevertHandled(ctx context.Context, tx pgx.Tx, id uuid.UUID, by int64, at time.Time) (bool, error) {
	if by <= 0 || at.IsZero() {
		return false, fmt.Errorf("%w: a revert entry is marked handled by a person, at a time", ErrElevationInvalid)
	}
	var handled *time.Time
	err := tx.QueryRow(ctx, `
SELECT handled_at FROM elitea_auth.admin_elevation_items
WHERE id = $1 AND kind = '`+ReviewItemRevert+`' FOR UPDATE`, id).Scan(&handled)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrRevertNotFound
	}
	if err != nil {
		return false, fmt.Errorf("mark the revert entry handled: %w", err)
	}
	if handled != nil {
		return false, nil
	}
	if _, err := tx.Exec(ctx, `
UPDATE elitea_auth.admin_elevation_items SET handled_at = $2, handled_by = $3 WHERE id = $1`,
		id, at.UTC(), by); err != nil {
		return false, fmt.Errorf("mark the revert entry handled: %w", err)
	}
	return true, nil
}
