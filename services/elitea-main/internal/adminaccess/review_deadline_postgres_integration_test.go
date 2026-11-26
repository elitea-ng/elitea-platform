package adminaccess_test

// The review deadline on the real schema, with a test clock (ADR-0059
// R2.9b, issue 2603): the daily overdue marker and the terminal state
// unreviewed_no_reviewer.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

const (
	testReviewOnline = 72 * time.Hour
	testReviewSealed = 24 * time.Hour
)

// deadlinePass runs one reconciler pass at now with the default review times.
func deadlinePass(t *testing.T, pool *pgxpool.Pool, now time.Time) adminaccess.ReconcileResult {
	t.Helper()
	reconciler := newReconciler(t, pool, auditAppender(t), now).
		WithReviewTimes(func(context.Context) (time.Duration, time.Duration, error) {
			return testReviewOnline, testReviewSealed, nil
		})
	return reconcile(t, reconciler)
}

// endedElevation is an elevation of path started half an hour before endAt
// and ended by its holder at endAt, so its review is pending from endAt.
func endedElevation(t *testing.T, pool *pgxpool.Pool, path string, endAt time.Time, holderRoles ...string) adminaccess.ElevationRecord {
	t.Helper()
	start := validElevationStart()
	start.UserID = seedPerson(t, pool, holderRoles...)
	start.Path = path
	start.ApproverUserID = 0
	if path == adminaccess.ElevationPathApproved {
		start.ApproverUserID = seedPerson(t, pool, adminaccess.RoleAdmin)
	}
	at := endAt.Add(-30 * time.Minute)
	start.BrowserSessionRef = seedLiveSession(t, pool, start.UserID, at)
	start.At, start.Duration = at, time.Hour
	started := startElevation(t, pool, start)
	ended, changed := endElevation(t, pool, adminaccess.ElevationEnd{
		ID: started.ID, Reason: adminaccess.ElevationEndHolder, ActorUserID: started.UserID, At: endAt,
	})
	if !changed || ended.ReviewState != adminaccess.ElevationReviewPending {
		t.Fatalf("premise: the end left %q (changed %v)", ended.ReviewState, changed)
	}
	return ended
}

func overdueItems(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(t.Context(), `
SELECT count(*) FROM elitea_auth.admin_elevation_items WHERE elevation_id = $1 AND kind = $2`,
		id, adminaccess.ReviewItemOverdue).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func noticesOfKind(t *testing.T, pool *pgxpool.Pool, id uuid.UUID, kind adminaccess.NoticeKind) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(t.Context(), `
SELECT count(*) FROM elitea_auth.admin_access_notices WHERE elevation_id = $1 AND kind = $2`,
		id, string(kind)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func eventsOfAction(t *testing.T, pool *pgxpool.Pool, id uuid.UUID, action securityaudit.ElevationAction) int {
	t.Helper()
	n := 0
	for _, row := range elevationEvents(t, pool, id.String()) {
		if row.action == string(action) {
			n++
		}
	}
	return n
}

// The online review is raised once its 72 hours have passed, the sealed one
// once its 24 hours have, and each with its notice rows.
func TestAnOverdueReviewIsRaisedAfterItsReviewTime(t *testing.T) {
	pool := newNoticeDatabase(t)
	seedPerson(t, pool, adminaccess.RoleSecurityAuditor)
	base := time.Now().UTC().Truncate(time.Microsecond)
	online := endedElevation(t, pool, adminaccess.ElevationPathApproved, base)
	sealed := endedElevation(t, pool, adminaccess.ElevationPathSealed, base)

	if result := deadlinePass(t, pool, base.Add(23*time.Hour)); result.Overdue != 0 || result.Unreviewed != 0 {
		t.Fatalf("pass at 23 h = %+v, want nothing", result)
	}
	if result := deadlinePass(t, pool, base.Add(25*time.Hour)); result.Overdue != 1 {
		t.Fatalf("pass at 25 h = %+v, want the sealed review raised", result)
	}
	if overdueItems(t, pool, sealed.ID) != 1 || overdueItems(t, pool, online.ID) != 0 {
		t.Fatal("at 25 h, want the sealed review raised and the online one not")
	}
	if noticesOfKind(t, pool, sealed.ID, adminaccess.NoticeReviewOverdue) == 0 {
		t.Fatal("the overdue sealed review wrote no notice row")
	}
	deadlinePass(t, pool, base.Add(71*time.Hour))
	if overdueItems(t, pool, online.ID) != 0 {
		t.Fatal("the online review was raised before its 72 hours")
	}
	deadlinePass(t, pool, base.Add(73*time.Hour))
	if overdueItems(t, pool, online.ID) != 1 {
		t.Fatal("at 73 h, want the online review raised once")
	}
	if noticesOfKind(t, pool, online.ID, adminaccess.NoticeReviewOverdue) == 0 {
		t.Fatal("the overdue online review wrote no notice row")
	}
	if state := readElevation(t, pool, online.ID).ReviewState; state != adminaccess.ElevationReviewPending {
		t.Fatalf("an overdue review with a reviewer is %q, want it still pending", state)
	}
}

// With two administrators, no auditor and an elevation one approved for the
// other, the deadline moves it to unreviewed_no_reviewer, announced once:
// one `unreviewed` event and its notice rows. Nothing is raised the next
// day, and a review of it is refused.
func TestNoReviewerMakesTheReviewTerminal(t *testing.T) {
	pool := newNoticeDatabase(t)
	base := time.Now().UTC().Truncate(time.Microsecond)
	elevation := endedElevation(t, pool, adminaccess.ElevationPathApproved, base, adminaccess.RoleAdmin)
	if ids := reviewersOf(t, pool, elevation); len(ids) != 0 {
		t.Fatalf("premise: reviewers %v, want none", ids)
	}

	result := deadlinePass(t, pool, base.Add(73*time.Hour))
	if result.Unreviewed != 1 || result.Overdue != 0 {
		t.Fatalf("pass at the deadline = %+v, want one move to unreviewed_no_reviewer", result)
	}
	if state := readElevation(t, pool, elevation.ID).ReviewState; state != adminaccess.ElevationUnreviewedNoReviewer {
		t.Fatalf("state %q, want %q", state, adminaccess.ElevationUnreviewedNoReviewer)
	}
	if n := eventsOfAction(t, pool, elevation.ID, securityaudit.ElevationUnreviewed); n != 1 {
		t.Fatalf("%d unreviewed events, want 1", n)
	}
	notices := noticesOfKind(t, pool, elevation.ID, adminaccess.NoticeUnreviewedNoReviewer)
	if notices == 0 {
		t.Fatal("the move wrote no notice row")
	}

	for _, at := range []time.Duration{97 * time.Hour, 121 * time.Hour} {
		if result := deadlinePass(t, pool, base.Add(at)); result.Overdue != 0 || result.Unreviewed != 0 {
			t.Fatalf("pass at %s = %+v, want nothing", at, result)
		}
	}
	if overdueItems(t, pool, elevation.ID) != 0 ||
		eventsOfAction(t, pool, elevation.ID, securityaudit.ElevationUnreviewed) != 1 ||
		noticesOfKind(t, pool, elevation.ID, adminaccess.NoticeUnreviewedNoReviewer) != notices ||
		noticesOfKind(t, pool, elevation.ID, adminaccess.NoticeReviewOverdue) != 0 {
		t.Fatal("a terminal review was raised or announced again")
	}

	auditor := seedPerson(t, pool, adminaccess.RoleSecurityAuditor)
	var reviewErr error
	_ = pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error {
		_, reviewErr = adminaccess.ReviewElevation(t.Context(), tx, auditAppender(t), adminaccess.ElevationReview{
			ID: elevation.ID, ReviewerUserID: auditor, Outcome: adminaccess.ReviewOutcomeJustified,
			At: base.Add(122 * time.Hour),
		})
		return reviewErr
	})
	if !errors.Is(reviewErr, adminaccess.ErrElevationNotReviewable) {
		t.Fatalf("a review of a terminal elevation: %v, want %v", reviewErr, adminaccess.ErrElevationNotReviewable)
	}
}

// An overdue review with an eligible reviewer is raised once on each of three
// days however many passes run, and reaches the terminal state on the first
// pass after its last reviewer is suspended.
func TestAnOverdueReviewIsRaisedOnceADay(t *testing.T) {
	pool := newNoticeDatabase(t)
	auditor := seedPerson(t, pool, adminaccess.RoleSecurityAuditor)
	base := time.Now().UTC().Truncate(time.Microsecond)
	elevation := endedElevation(t, pool, adminaccess.ElevationPathApproved, base)

	for day, passes := range [][]time.Duration{
		{73 * time.Hour, 73*time.Hour + time.Minute, 80 * time.Hour},
		{97 * time.Hour, 97*time.Hour + 5*time.Minute, 120 * time.Hour},
		{121 * time.Hour, 122 * time.Hour, 144 * time.Hour},
	} {
		for _, at := range passes {
			deadlinePass(t, pool, base.Add(at))
		}
		if n := overdueItems(t, pool, elevation.ID); n != day+1 {
			t.Fatalf("after day %d: raised %d times, want %d", day+1, n, day+1)
		}
	}
	if eventsOfAction(t, pool, elevation.ID, securityaudit.ElevationUnreviewed) != 0 {
		t.Fatal("a review with a reviewer was recorded unreviewed")
	}

	if _, err := pool.Exec(t.Context(), `UPDATE public.auth_core__user SET suspended = true WHERE id = $1`, auditor); err != nil {
		t.Fatal(err)
	}
	if result := deadlinePass(t, pool, base.Add(145*time.Hour)); result.Unreviewed != 1 {
		t.Fatalf("pass after the last reviewer left = %+v, want the terminal move", result)
	}
}
