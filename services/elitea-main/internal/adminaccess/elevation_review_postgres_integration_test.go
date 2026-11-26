package adminaccess_test

// The reviewers of an elevation and its attestation on the real schema
// (ADR-0059 R2.9a, issue 2523): shared 0269 and 0283.

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
)

func reviewersOf(t *testing.T, pool *pgxpool.Pool, e adminaccess.ElevationRecord) []int64 {
	t.Helper()
	ids, err := adminaccess.ElevationReviewerIDs(t.Context(), pool, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

// The eligible reviewers are recorded when the elevation ends, on each end
// path: the holder, the approver and the expiry the reconciler records. The
// holder and the approver are never among them, a suspended auditor is not,
// and an auditor who gets the role after the end is not.
func TestTheReviewersAreRecordedOnEveryEndPath(t *testing.T) {
	pool := newElevationDatabase(t)
	// Only the people this test seeds hold review permissions.
	if _, err := pool.Exec(t.Context(), `UPDATE public.auth_core__user SET suspended = true`); err != nil {
		t.Fatal(err)
	}
	auditor := seedPerson(t, pool, adminaccess.RoleSecurityAuditor)
	suspended := seedPerson(t, pool, adminaccess.RoleSecurityAuditor)
	if _, err := pool.Exec(t.Context(), `UPDATE public.auth_core__user SET suspended = true WHERE id = $1`, suspended); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	want := func(e adminaccess.ElevationRecord) {
		t.Helper()
		got := reviewersOf(t, pool, e)
		if !slices.Contains(got, auditor) || slices.Contains(got, suspended) ||
			slices.Contains(got, e.UserID) || slices.Contains(got, e.ApproverUserID) {
			t.Fatalf("reviewers of %s = %v, want the auditor %d and neither the holder nor the approver", e.EndReason, got, auditor)
		}
	}

	byHolder := startAt(t, pool, now.Add(-time.Minute), time.Hour)
	ended, _ := endElevation(t, pool, adminaccess.ElevationEnd{ID: byHolder.ID, Reason: adminaccess.ElevationEndHolder, At: now})
	want(ended)

	byApprover := startAt(t, pool, now.Add(-time.Minute), time.Hour)
	ended, _ = endElevation(t, pool, adminaccess.ElevationEnd{ID: byApprover.ID, Reason: adminaccess.ElevationEndApprover,
		ActorUserID: byApprover.ApproverUserID, At: now})
	want(ended)

	lapsed := startAt(t, pool, now.Add(-2*time.Hour), time.Hour)
	if result := reconcile(t, newReconciler(t, pool, auditAppender(t), now)); result.Expired != 1 {
		t.Fatalf("pass = %+v, want one expiry", result)
	}
	want(readElevation(t, pool, lapsed.ID))

	// A role given after the end does not make a reviewer of THIS review.
	late := seedPerson(t, pool, adminaccess.RoleSecurityAuditor)
	if slices.Contains(reviewersOf(t, pool, ended), late) {
		t.Fatal("an auditor named after the end is a reviewer")
	}
	var reviewErr error
	_ = pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error {
		_, reviewErr = adminaccess.ReviewElevation(t.Context(), tx, auditAppender(t), adminaccess.ElevationReview{
			ID: ended.ID, ReviewerUserID: late, Outcome: adminaccess.ReviewOutcomeJustified, At: now,
		})
		return reviewErr
	})
	if !errors.Is(reviewErr, adminaccess.ErrReviewerNotEligible) {
		t.Fatalf("a late auditor's review: %v, want %v", reviewErr, adminaccess.ErrReviewerNotEligible)
	}
}

// The items are append-only: no delete, and a review cannot be rewritten.
func TestTheReviewItemsAreAppendOnly(t *testing.T) {
	pool := newElevationDatabase(t)
	auditor := seedPerson(t, pool, adminaccess.RoleSecurityAuditor)
	now := time.Now().UTC().Truncate(time.Microsecond)
	elevation := startAt(t, pool, now.Add(-time.Minute), time.Hour)
	ended, _ := endElevation(t, pool, adminaccess.ElevationEnd{ID: elevation.ID, Reason: adminaccess.ElevationEndHolder, At: now})
	if err := pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error {
		_, err := adminaccess.ReviewElevation(t.Context(), tx, auditAppender(t), adminaccess.ElevationReview{
			ID: ended.ID, ReviewerUserID: auditor, Outcome: adminaccess.ReviewOutcomeJustified, Note: "ok", At: now,
		})
		return err
	}); err != nil {
		t.Fatalf("review: %v", err)
	}
	for name, statement := range map[string]string{
		"delete":         `DELETE FROM elitea_auth.admin_elevation_items WHERE elevation_id = $1`,
		"rewrite review": `UPDATE elitea_auth.admin_elevation_items SET outcome = 'unauthorized' WHERE elevation_id = $1 AND kind = 'review'`,
		"handle review":  `UPDATE elitea_auth.admin_elevation_items SET handled_at = now(), handled_by = 1 WHERE elevation_id = $1 AND kind = 'review'`,
	} {
		if _, err := pool.Exec(t.Context(), statement, ended.ID); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}
