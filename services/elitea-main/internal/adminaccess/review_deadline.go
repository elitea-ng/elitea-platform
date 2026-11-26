package adminaccess

// The review deadline (ADR-0059 decision 5, "Review after use", "No eligible
// reviewer"; owner decision 9; story R2.9b, issue 2603): a pass of the
// elevation reconciler, not a loop of its own.
//
// A review is overdue once its elevation ended longer ago than the review
// time of its path (break_glass review_hours_online, 72 hours by default;
// review_hours_sealed, 24 hours, for the sealed credential). Then, on every
// pass:
//
//   - while a reviewer recorded at the end (R2.9a) still has an account that
//     is not suspended, the review stays review_pending and is raised once a
//     day: an `overdue` item on the elevation's review record and the notice
//     rows (and the webhook row) of NoticeReviewOverdue. The time of the last
//     such item, compared with the injected clock, is the "once a day";
//   - when none has, the elevation moves to the terminal state
//     unreviewed_no_reviewer: the `unreviewed` admin.elevation event and the
//     NoticeUnreviewedNoReviewer notices, written once with the move. Nothing
//     is raised for it afterwards, and a review of it is refused (the state
//     check of ReviewElevation): only the acknowledgement of R2.9c is left.
//
// The check runs on every pass, so an elevation whose last reviewer leaves
// after it was first raised reaches the terminal state too. Each elevation is
// its own transaction, behind FOR UPDATE SKIP LOCKED, as in the other passes.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// ReviewOverdueInterval is how often an overdue review is raised again.
const ReviewOverdueInterval = 24 * time.Hour

// reviewDue is the predicate "the review of elevation is pending and past
// its deadline at $1", with the online review time $2 and the sealed one $3.
const reviewDue = `elevation.review_state = '` + ElevationReviewPending + `'
  AND elevation.ended_at + CASE WHEN elevation.path = '` + ElevationPathSealed + `'
                               THEN make_interval(secs => $3) ELSE make_interval(secs => $2) END
      <= $1::timestamptz`

// activeReviewer is the predicate "a reviewer recorded for elevation still
// has an account that is not suspended".
const activeReviewer = `EXISTS (
        SELECT 1 FROM elitea_auth.admin_elevation_items reviewer
        JOIN public.auth_core__user account ON account.id = reviewer.user_id AND account.suspended = false
        WHERE reviewer.elevation_id = elevation.id AND reviewer.kind = '` + ReviewItemReviewer + `')`

// raisedSince is the predicate "the review of elevation was raised after $4".
const raisedSince = `EXISTS (
        SELECT 1 FROM elitea_auth.admin_elevation_items raised
        WHERE raised.elevation_id = elevation.id AND raised.kind = '` + ReviewItemOverdue + `'
          AND raised.created_at > $4)`

// reviewDeadlines raises the overdue reviews and records the ones nobody can
// do, one transaction each.
func (r *ElevationReconciler) reviewDeadlines(ctx context.Context, now time.Time) (overdue, unreviewed int, err error) {
	online, sealed, err := r.reviewTimes(ctx)
	if err == nil && (online <= 0 || sealed <= 0) {
		err = fmt.Errorf("review times %s and %s are not both positive", online, sealed)
	}
	if err != nil {
		return 0, 0, fmt.Errorf("elevation reconciler: read the review times: %w", err)
	}
	since := now.Add(-ReviewOverdueInterval)
	// Only the elevations a pass writes: past the deadline, and either with
	// nobody left to review or not raised in the last day.
	ids, err := r.candidates(ctx, `
SELECT elevation.id FROM elitea_auth.admin_elevations elevation
WHERE `+reviewDue+` AND (NOT `+activeReviewer+` OR NOT `+raisedSince+`)
ORDER BY elevation.ended_at, elevation.id LIMIT $5`, now, online.Seconds(), sealed.Seconds(), since, r.batch)
	if err != nil {
		return 0, 0, fmt.Errorf("elevation reconciler: list the overdue reviews: %w", err)
	}
	var errs []error
	for _, id := range ids {
		var moved bool
		changed, err := r.inTx(ctx, func(tx pgx.Tx) (bool, error) {
			var changed bool
			var err error
			moved, changed, err = applyReviewDeadline(ctx, tx, r.appender, id, now, online, sealed, since)
			return changed, err
		})
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("elevation reconciler: review deadline of %s: %w", id, err))
		case changed && moved:
			unreviewed++
		case changed:
			overdue++
		}
	}
	return overdue, unreviewed, errors.Join(errs...)
}

// applyReviewDeadline applies the deadline to one elevation on tx: moved
// reports the move to unreviewed_no_reviewer, changed that anything was
// written.
func applyReviewDeadline(ctx context.Context, tx pgx.Tx, appender securityaudit.Appender, id uuid.UUID,
	now time.Time, online, sealed time.Duration, since time.Time,
) (moved, changed bool, err error) {
	var hasReviewer, raised bool
	err = tx.QueryRow(ctx, `
SELECT `+activeReviewer+`, `+raisedSince+`
FROM elitea_auth.admin_elevations elevation
WHERE elevation.id = $5 AND `+reviewDue+`
FOR UPDATE OF elevation SKIP LOCKED`, now, online.Seconds(), sealed.Seconds(), since, id).Scan(&hasReviewer, &raised)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	if hasReviewer {
		if raised {
			return false, false, nil
		}
		elevation, err := ReadElevation(ctx, tx, id)
		if err != nil {
			return false, false, err
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO elitea_auth.admin_elevation_items (elevation_id, kind, created_at)
VALUES ($1, '`+ReviewItemOverdue+`', $2)`, id, now); err != nil {
			return false, false, fmt.Errorf("record the overdue review: %w", err)
		}
		if _, err := WriteElevationNotices(ctx, tx, ElevationNotice{
			Kind: NoticeReviewOverdue, Elevation: elevation, At: now,
		}); err != nil {
			return false, false, err
		}
		return false, true, nil
	}
	elevation, err := scanElevation(tx.QueryRow(ctx, `
UPDATE elitea_auth.admin_elevations SET review_state = '`+ElevationUnreviewedNoReviewer+`'
WHERE id = $1 AND review_state = '`+ElevationReviewPending+`'
RETURNING `+elevationColumns, id))
	if err != nil {
		return false, false, fmt.Errorf("record the review nobody can do: %w", err)
	}
	if _, err := WriteElevationNotices(ctx, tx, ElevationNotice{
		Kind: NoticeUnreviewedNoReviewer, Elevation: elevation, At: now,
	}); err != nil {
		return false, false, err
	}
	if err := AppendElevationEvent(ctx, tx, appender, ElevationEvent{
		Action:    securityaudit.ElevationUnreviewed,
		Source:    securityaudit.ElevationSourceSystem,
		Elevation: elevation,
	}); err != nil {
		return false, false, err
	}
	return true, true, nil
}
