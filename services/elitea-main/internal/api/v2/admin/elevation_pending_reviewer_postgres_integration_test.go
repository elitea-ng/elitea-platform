package admin_test

// A pending reviewer is protected (ADR-0059 R2.9a, issue 2523): while the
// review of an elevation is pending, taking the review permission from one
// of its recorded reviewers, suspending or deleting them needs the standing
// admin.auth.users.super_admin; the change is listed in the review, and the
// person can still submit the outcome.

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

const pendingReviewerRole = "r29a_auditor"

// pendingReviewEnvironment gives the plain target a role that holds
// access.breakglass.review and makes them the recorded reviewer of an ended
// elevation of the matrix editor, approved by the super_admin.
func pendingReviewEnvironment(t *testing.T) (escalationEnvironment, uuid.UUID) {
	t.Helper()
	env := newEscalationEnvironment(t)
	ctx := context.Background()
	seedReviewRole(t, env, pendingReviewerRole)
	giveRole(t, env, escalationPlainTarget, pendingReviewerRole)
	now := time.Now().UTC()
	var id uuid.UUID
	if err := env.pool.QueryRow(ctx, `
INSERT INTO elitea_auth.admin_elevations
    (user_id, browser_session_ref, level, path, reason_code, reason, requester_user_id, approver_user_id,
     requested_at, decision, decided_at, started_at, expires_at, ended_at, end_reason, review_state)
VALUES ($1, 920101, 'admin', 'approved', 'incident', 'R2.9a: a pending review', $1, $2, $3, 'approved', $3, $3, $4, $5,
        'holder', 'review_pending')
RETURNING id`, env.ids[escalationMatrix], env.ids[escalationSuper], now.Add(-2*time.Hour), now.Add(-time.Hour),
		now.Add(-90*time.Minute)).Scan(&id); err != nil {
		t.Fatalf("seed an ended elevation: %v", err)
	}
	if _, err := env.pool.Exec(ctx, `
INSERT INTO elitea_auth.admin_elevation_items (elevation_id, kind, user_id, created_at)
VALUES ($1, 'reviewer', $2, $3)`, id, env.ids[escalationPlainTarget], now.Add(-90*time.Minute)); err != nil {
		t.Fatalf("record the reviewer: %v", err)
	}
	return env, id
}

// seedReviewRole creates an administration role that holds
// access.breakglass.review.
func seedReviewRole(t *testing.T, env escalationEnvironment, name string) {
	t.Helper()
	for _, statement := range []string{
		`INSERT INTO public.auth_core__role (name, mode) VALUES ('` + name + `', 'administration')`,
		`INSERT INTO public.auth_core__role_permission (role_id, permission)
			SELECT id, '` + adminaccess.AccessBreakglassReview + `' FROM public.auth_core__role
			WHERE name = '` + name + `' AND mode = 'administration'`,
	} {
		if _, err := env.pool.Exec(context.Background(), statement); err != nil {
			t.Fatalf("seed %q: %v", statement, err)
		}
	}
}

func reviewerChanges(t *testing.T, env escalationEnvironment, id uuid.UUID) []string {
	t.Helper()
	rows, err := env.pool.Query(context.Background(), `
SELECT action FROM elitea_auth.admin_elevation_items
WHERE elevation_id = $1 AND kind = 'reviewer_change' AND user_id = $2 ORDER BY created_at`,
		id, env.ids[escalationPlainTarget])
	if err != nil {
		t.Fatal(err)
	}
	actions, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return actions
}

// An admin who removes the reviewer's role gets 403; a super_admin can, the
// removal is listed in the review, and that person still submits the outcome.
func TestAPendingReviewerKeepsTheReviewThroughARoleRemoval(t *testing.T) {
	env, id := pendingReviewEnvironment(t)
	target := env.ids[escalationPlainTarget]

	expectRefusal(t, adminDo(t, env.as(escalationAdmin), http.MethodDelete,
		modesRemove(target, pendingReviewerRole), nil), adminaccess.CodeElevationPendingReviewer)
	expectRoles(t, env, escalationPlainTarget, pendingReviewerRole)
	if got := reviewerChanges(t, env, id); len(got) != 0 {
		t.Fatalf("a refused removal was listed: %v", got)
	}

	expectStatus(t, adminDo(t, env.as(escalationSuper), http.MethodDelete,
		modesRemove(target, pendingReviewerRole), nil), http.StatusOK)
	if got := reviewerChanges(t, env, id); len(got) != 1 || got[0] != adminaccess.ReviewerChangeRoleRemoved {
		t.Fatalf("reviewer changes = %v, want one role_removed", got)
	}

	appender, err := securityaudit.NewPostgresAppender("elitea-main/pending-reviewer-test")
	if err != nil {
		t.Fatal(err)
	}
	if err := pgx.BeginFunc(context.Background(), env.pool, func(tx pgx.Tx) error {
		_, err := adminaccess.ReviewElevation(context.Background(), tx, appender, adminaccess.ElevationReview{
			ID: id, ReviewerUserID: int64(target), Outcome: adminaccess.ReviewOutcomeJustified, At: time.Now(),
		})
		return err
	}); err != nil {
		t.Fatalf("the removed reviewer could not submit the outcome: %v", err)
	}
}

// Suspending and deleting a pending reviewer need the super_admin too.
func TestSuspendingOrDeletingAPendingReviewerNeedsTheSuperAdmin(t *testing.T) {
	env, id := pendingReviewEnvironment(t)
	target := env.ids[escalationPlainTarget]
	suspend := fmt.Sprintf("/u/admin/user_suspend/administration/%d", target)

	expectRefusal(t, adminDo(t, env.as(escalationAdmin), http.MethodPut, suspend, map[string]any{"suspended": true}),
		adminaccess.CodeElevationPendingReviewer)
	expectRefusal(t, adminDo(t, env.as(escalationAdmin), http.MethodPost, usersAction, map[string]any{
		"action": "delete", "users": []map[string]any{{"id": target}},
	}), adminaccess.CodeElevationPendingReviewer)

	expectStatus(t, adminDo(t, env.as(escalationSuper), http.MethodPut, suspend, map[string]any{"suspended": true}),
		http.StatusOK)
	expectStatus(t, adminDo(t, env.as(escalationSuper), http.MethodPost, usersAction, map[string]any{
		"action": "delete", "users": []map[string]any{{"id": target}},
	}), http.StatusOK)
	got := reviewerChanges(t, env, id)
	if len(got) != 2 || got[0] != adminaccess.ReviewerChangeSuspended || got[1] != adminaccess.ReviewerChangeDeleted {
		t.Fatalf("reviewer changes = %v, want suspended then deleted", got)
	}
}

// Removing one of two roles that grant the review permission leaves the
// person a reviewer: no super_admin is needed and nothing is listed.
func TestRemovingARoleTheReviewerHoldsTwiceIsNoReviewerChange(t *testing.T) {
	env, id := pendingReviewEnvironment(t)
	target := env.ids[escalationPlainTarget]
	const second = "r29a_auditor_2"
	seedReviewRole(t, env, second)
	giveRole(t, env, escalationPlainTarget, second)
	// The admin holds the permission, so the subset rule lets the removal
	// through to the pending-reviewer rule.
	giveRole(t, env, escalationAdmin, pendingReviewerRole)

	expectStatus(t, adminDo(t, env.as(escalationAdmin), http.MethodDelete,
		modesRemove(target, pendingReviewerRole), nil), http.StatusOK)
	if got := reviewerChanges(t, env, id); len(got) != 0 {
		t.Fatalf("a removal that kept the permission was listed: %v", got)
	}
}
