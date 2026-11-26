package adminaccess_test

// Provisional grants on the real schema (ADR-0059 decision 5; stories R2.12a,
// issue 2526, and R2.12b, issue 2606): shared 0284. The grants are read the
// way the resolver and the project access predicate read them, at a test
// clock: provisionalgrants.RoleRows and projectaccess.MembershipAt.

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/projectaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/provisionalgrants"
)

// holderOf is the holder of e, as a write under the elevation sees it.
func holderOf(e adminaccess.ElevationRecord) adminaccess.Caller {
	return adminaccess.Caller{
		UserID:     e.UserID,
		Elevated:   true,
		Elevations: []auth.ElevationGrant{{ID: e.ID.String(), Level: e.Level, Path: e.Path}},
	}
}

// provisionalRoleInEffect answers whether the person holds role through a
// provisional grant at at.
func provisionalRoleInEffect(t *testing.T, pool *pgxpool.Pool, userID int64, role string, at time.Time) bool {
	t.Helper()
	var n int
	if err := pool.QueryRow(t.Context(), `
SELECT count(*) FROM `+provisionalgrants.RoleRows("$1")+` AS grant_row
WHERE grant_row.user_id = $2 AND grant_row.role_name = $3`, at, userID, role).Scan(&n); err != nil {
		t.Fatalf("read the provisional roles: %v", err)
	}
	return n > 0
}

func standingRoles(t *testing.T, pool *pgxpool.Pool, userID int64) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(t.Context(),
		`SELECT count(*) FROM public.auth_core__user_role WHERE user_id = $1`, userID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func projectAccess(t *testing.T, pool *pgxpool.Pool, projectID, userID int64, at time.Time) bool {
	t.Helper()
	var ok bool
	if err := pool.QueryRow(t.Context(), `SELECT `+projectaccess.MembershipAt(1, 2, "$3"),
		projectID, userID, at).Scan(&ok); err != nil {
		t.Fatalf("read the project access: %v", err)
	}
	return ok
}

func standingMemberships(t *testing.T, pool *pgxpool.Pool, projectID, userID int64) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(t.Context(), `
SELECT count(*) FROM public.auth_core__project_user_role WHERE project_id = $1 AND user_id = $2`,
		projectID, userID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// seedProject is a project with one role, answered as (project, role).
func seedProject(t *testing.T, pool *pgxpool.Pool) (int64, int) {
	t.Helper()
	var project int64
	if err := pool.QueryRow(t.Context(), `
INSERT INTO centry.project (name, owner_id) VALUES ($1, 1) RETURNING id`, "provisional-"+uuid.NewString()).Scan(&project); err != nil {
		t.Fatalf("seed a project: %v", err)
	}
	var role int
	if err := pool.QueryRow(t.Context(), `
INSERT INTO public.auth_core__project_role (project_id, name) VALUES ($1, 'editor') RETURNING id`, project).Scan(&role); err != nil {
		t.Fatalf("seed a project role: %v", err)
	}
	return project, role
}

func grantsOf(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) []adminaccess.ProvisionalGrant {
	t.Helper()
	grants, err := adminaccess.ElevationProvisionalGrants(t.Context(), pool, id)
	if err != nil {
		t.Fatalf("read the grants: %v", err)
	}
	return grants
}

func review(t *testing.T, pool *pgxpool.Pool, r adminaccess.ElevationReview) (adminaccess.ReviewResult, error) {
	t.Helper()
	var result adminaccess.ReviewResult
	err := pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error {
		var err error
		result, err = adminaccess.ReviewElevation(t.Context(), tx, auditAppender(t), r)
		return err
	})
	return result, err
}

func grantIDs(grants []adminaccess.ProvisionalGrant) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(grants))
	for _, grant := range grants {
		ids = append(ids, grant.ID)
	}
	return ids
}

// A role given under an elevation is in effect during it and not a second
// after it expires; it never writes auth_core__user_role. A justified review
// that confirms it makes it standing, written by the reviewer with the source
// `review`.
func TestAProvisionalRoleLastsTheElevationUntilAJustifiedReviewConfirmsIt(t *testing.T) {
	provisionalgrants.EnableForTest(t)
	pool := newElevationDatabase(t)
	auditor := seedPerson(t, pool, adminaccess.RoleSecurityAuditor)
	now := time.Now().UTC().Truncate(time.Microsecond)
	elevation := startAt(t, pool, now.Add(-time.Minute), time.Hour)
	grantee := seedPerson(t, pool)

	given, err := adminaccess.GiveProvisionalRoles(t.Context(), pool, holderOf(elevation), grantee,
		adminaccess.ModeAdministration, []string{adminaccess.RoleAdmin}, now)
	if err != nil || len(given) != 1 {
		t.Fatalf("give: %v %v", given, err)
	}
	if !provisionalRoleInEffect(t, pool, grantee, adminaccess.RoleAdmin, now) {
		t.Fatal("the provisional role is not in effect during the elevation")
	}
	if provisionalRoleInEffect(t, pool, grantee, adminaccess.RoleAdmin, elevation.ExpiresAt.Add(time.Second)) {
		t.Fatal("the provisional role is in effect a second after the elevation expired")
	}
	if n := standingRoles(t, pool, grantee); n != 0 {
		t.Fatalf("the grantee holds %d standing roles, want none", n)
	}

	ended, _ := endElevation(t, pool, adminaccess.ElevationEnd{ID: elevation.ID, Reason: adminaccess.ElevationEndHolder, At: now})
	if provisionalRoleInEffect(t, pool, grantee, adminaccess.RoleAdmin, now) {
		t.Fatal("the provisional role is in effect after the elevation ended")
	}
	grants := grantsOf(t, pool, ended.ID)
	if len(grants) != 1 || grants[0].Kind != adminaccess.ProvisionalKindRole || grants[0].GrantedBy != elevation.UserID {
		t.Fatalf("grants = %+v", grants)
	}
	result, err := review(t, pool, adminaccess.ElevationReview{
		ID: ended.ID, ReviewerUserID: auditor, Outcome: adminaccess.ReviewOutcomeJustified,
		Confirm: grantIDs(grants), At: now,
	})
	if err != nil || result.Confirmed != 1 {
		t.Fatalf("review: %+v %v", result, err)
	}
	if n := standingRoles(t, pool, grantee); n != 1 {
		t.Fatalf("the grantee holds %d standing roles after the confirmation, want 1", n)
	}
	var actor int64
	var source, ref string
	if err := pool.QueryRow(t.Context(), `
SELECT actor_user_id, source, source_ref FROM elitea_auth.admin_role_grants
WHERE user_id = $1 AND role_name = $2 AND action = 'granted'`, grantee, adminaccess.RoleAdmin).Scan(&actor, &source, &ref); err != nil {
		t.Fatalf("read the grant history: %v", err)
	}
	if actor != auditor || source != "review" || ref != grants[0].ID.String() {
		t.Fatalf("history = (%d, %s, %s), want the reviewer %d, review, the grant", actor, source, ref, auditor)
	}
	if confirmed := grantsOf(t, pool, ended.ID); confirmed[0].ConfirmedBy != auditor || confirmed[0].ConfirmedAt.IsZero() {
		t.Fatalf("the grant is not marked confirmed: %+v", confirmed[0])
	}
	found := false
	for _, event := range elevationEvents(t, pool, ended.ID.String()) {
		found = found || (event.action == "reviewed" && strings.Contains(event.raw, `"provisional_confirmed:1"`))
	}
	if !found {
		t.Fatal("the reviewed event does not count the confirmation")
	}
	// The row's one permitted update is the confirmation, once.
	if _, err := pool.Exec(t.Context(), `
UPDATE elitea_auth.admin_provisional_grants SET confirmed_by = $2 WHERE id = $1`, grants[0].ID, grantee); err == nil {
		t.Fatal("a confirmed grant was rewritten")
	}
	if _, err := pool.Exec(t.Context(), `DELETE FROM elitea_auth.admin_provisional_grants WHERE id = $1`, grants[0].ID); err == nil {
		t.Fatal("a grant was deleted")
	}
}

// An unauthorized review confirms nothing (and a confirmation with it is
// refused); the person holds nothing and auth_core__user_role was never
// written.
func TestAnUnauthorizedReviewLeavesTheProvisionalRoleUnconfirmed(t *testing.T) {
	provisionalgrants.EnableForTest(t)
	pool := newElevationDatabase(t)
	auditor := seedPerson(t, pool, adminaccess.RoleSecurityAuditor)
	now := time.Now().UTC().Truncate(time.Microsecond)
	elevation := startAt(t, pool, now.Add(-time.Minute), time.Hour)
	grantee := seedPerson(t, pool)
	if _, err := adminaccess.GiveProvisionalRoles(t.Context(), pool, holderOf(elevation), grantee,
		adminaccess.ModeAdministration, []string{adminaccess.RoleAdmin}, now); err != nil {
		t.Fatal(err)
	}
	ended, _ := endElevation(t, pool, adminaccess.ElevationEnd{ID: elevation.ID, Reason: adminaccess.ElevationEndHolder, At: now})
	grants := grantsOf(t, pool, ended.ID)
	if _, err := review(t, pool, adminaccess.ElevationReview{
		ID: ended.ID, ReviewerUserID: auditor, Outcome: adminaccess.ReviewOutcomeUnauthorized,
		Confirm: grantIDs(grants), At: now,
	}); !errors.Is(err, adminaccess.ErrProvisionalConfirmOutcome) {
		t.Fatalf("an unauthorized confirmation: %v, want %v", err, adminaccess.ErrProvisionalConfirmOutcome)
	}
	if _, err := review(t, pool, adminaccess.ElevationReview{
		ID: ended.ID, ReviewerUserID: auditor, Outcome: adminaccess.ReviewOutcomeUnauthorized, At: now,
	}); err != nil {
		t.Fatalf("review: %v", err)
	}
	if n := standingRoles(t, pool, grantee); n != 0 {
		t.Fatalf("the grantee holds %d standing roles, want none", n)
	}
	if grantsOf(t, pool, ended.ID)[0].ConfirmedBy != 0 {
		t.Fatal("an unauthorized review confirmed the grant")
	}
	var history int
	if err := pool.QueryRow(t.Context(),
		`SELECT count(*) FROM elitea_auth.admin_role_grants WHERE user_id = $1`, grantee).Scan(&history); err != nil || history != 0 {
		t.Fatalf("%d history rows (%v), want none", history, err)
	}
}

// A membership given under an elevation admits the person to the project
// during it and not a second after it expires, never writes
// auth_core__project_user_role, admits nobody to a suspended project, and a
// justified confirmation writes the standing row.
func TestAProvisionalMembershipLastsTheElevationUntilAJustifiedReviewConfirmsIt(t *testing.T) {
	provisionalgrants.EnableForTest(t)
	pool := newElevationDatabase(t)
	auditor := seedPerson(t, pool, adminaccess.RoleSecurityAuditor)
	now := time.Now().UTC().Truncate(time.Microsecond)
	elevation := startAt(t, pool, now.Add(-time.Minute), time.Hour)
	grantee := seedPerson(t, pool)
	project, role := seedProject(t, pool)
	suspended, suspendedRole := seedProject(t, pool)
	if _, err := pool.Exec(t.Context(), `UPDATE centry.project SET suspended = true WHERE id = $1`, suspended); err != nil {
		t.Fatal(err)
	}
	caller := holderOf(elevation)
	for _, membership := range [][2]int64{{project, int64(role)}, {suspended, int64(suspendedRole)}} {
		if err := adminaccess.GiveProvisionalMembership(t.Context(), pool, caller, membership[0], grantee,
			[]int{int(membership[1])}, now); err != nil {
			t.Fatalf("give: %v", err)
		}
	}
	if err := adminaccess.GiveProvisionalMembership(t.Context(), pool, caller, project, grantee, []int{suspendedRole}, now); err == nil {
		t.Fatal("a role of another project was given")
	}
	if !projectAccess(t, pool, project, grantee, now) {
		t.Fatal("the provisional member has no access during the elevation")
	}
	if projectAccess(t, pool, project, grantee, elevation.ExpiresAt.Add(time.Second)) {
		t.Fatal("the provisional member has access a second after the elevation expired")
	}
	if projectAccess(t, pool, suspended, grantee, now) {
		t.Fatal("a provisional membership admits to a suspended project")
	}
	if n := standingMemberships(t, pool, project, grantee); n != 0 {
		t.Fatalf("%d standing membership rows, want none", n)
	}
	members, err := adminaccess.ProvisionalProjectMembers(t.Context(), pool, project, now)
	if err != nil || len(members) != 1 || members[0].UserID != grantee || members[0].ProjectRoleName != "editor" {
		t.Fatalf("provisional members = %+v %v", members, err)
	}

	ended, _ := endElevation(t, pool, adminaccess.ElevationEnd{ID: elevation.ID, Reason: adminaccess.ElevationEndHolder, At: now})
	if projectAccess(t, pool, project, grantee, now) {
		t.Fatal("the provisional member has access after the elevation ended")
	}
	var confirm []uuid.UUID
	for _, grant := range grantsOf(t, pool, ended.ID) {
		if grant.ProjectID == project {
			confirm = append(confirm, grant.ID)
		}
	}
	if result, err := review(t, pool, adminaccess.ElevationReview{
		ID: ended.ID, ReviewerUserID: auditor, Outcome: adminaccess.ReviewOutcomeJustified, Confirm: confirm, At: now,
	}); err != nil || result.Confirmed != 1 {
		t.Fatalf("review: %+v %v", result, err)
	}
	if n := standingMemberships(t, pool, project, grantee); n != 1 {
		t.Fatalf("%d standing membership rows after the confirmation, want 1", n)
	}
	if n := standingMemberships(t, pool, suspended, grantee); n != 0 {
		t.Fatal("an unconfirmed membership was written")
	}
}

// After an unauthorized review a provisional member has no access and no
// membership row.
func TestAnUnauthorizedReviewLeavesNoMembership(t *testing.T) {
	provisionalgrants.EnableForTest(t)
	pool := newElevationDatabase(t)
	auditor := seedPerson(t, pool, adminaccess.RoleSecurityAuditor)
	now := time.Now().UTC().Truncate(time.Microsecond)
	elevation := startAt(t, pool, now.Add(-time.Minute), time.Hour)
	grantee := seedPerson(t, pool)
	project, role := seedProject(t, pool)
	if err := adminaccess.GiveProvisionalMembership(t.Context(), pool, holderOf(elevation), project, grantee, []int{role}, now); err != nil {
		t.Fatal(err)
	}
	ended, _ := endElevation(t, pool, adminaccess.ElevationEnd{ID: elevation.ID, Reason: adminaccess.ElevationEndHolder, At: now})
	if _, err := review(t, pool, adminaccess.ElevationReview{
		ID: ended.ID, ReviewerUserID: auditor, Outcome: adminaccess.ReviewOutcomeUnauthorized, At: now,
	}); err != nil {
		t.Fatalf("review: %v", err)
	}
	if projectAccess(t, pool, project, grantee, now) || standingMemberships(t, pool, project, grantee) != 0 {
		t.Fatal("an unauthorized review left the person a member")
	}
}

// The grants of an elevation whose review became unreviewed_no_reviewer stay
// unconfirmed (deferred from R2.9b, issue 2603): nothing confirms them, and
// a later review is refused.
func TestNoReviewerLeavesTheProvisionalGrantsUnconfirmed(t *testing.T) {
	provisionalgrants.EnableForTest(t)
	pool := newNoticeDatabase(t)
	base := time.Now().UTC().Truncate(time.Microsecond)
	holder := seedPerson(t, pool, adminaccess.RoleAdmin)
	approver := seedPerson(t, pool, adminaccess.RoleAdmin)
	start := validElevationStart()
	start.UserID, start.ApproverUserID = holder, approver
	at := base.Add(-30 * time.Minute)
	start.BrowserSessionRef = seedLiveSession(t, pool, holder, at)
	start.At, start.Duration = at, time.Hour
	elevation := startElevation(t, pool, start)
	grantee := seedPerson(t, pool)
	if _, err := adminaccess.GiveProvisionalRoles(t.Context(), pool, holderOf(elevation), grantee,
		adminaccess.ModeAdministration, []string{adminaccess.RoleSecurityAuditor}, base); err != nil {
		t.Fatal(err)
	}
	ended, _ := endElevation(t, pool, adminaccess.ElevationEnd{ID: elevation.ID, Reason: adminaccess.ElevationEndHolder, ActorUserID: holder, At: base})
	if ids := reviewersOf(t, pool, ended); len(ids) != 0 {
		t.Fatalf("premise: reviewers %v, want none (a provisional auditor is never a reviewer)", ids)
	}
	if result := deadlinePass(t, pool, base.Add(73*time.Hour)); result.Unreviewed != 1 {
		t.Fatalf("pass = %+v, want one move to unreviewed_no_reviewer", result)
	}
	auditor := seedPerson(t, pool, adminaccess.RoleSecurityAuditor)
	grants := grantsOf(t, pool, ended.ID)
	if _, err := review(t, pool, adminaccess.ElevationReview{
		ID: ended.ID, ReviewerUserID: auditor, Outcome: adminaccess.ReviewOutcomeJustified,
		Confirm: grantIDs(grants), At: base.Add(74 * time.Hour),
	}); !errors.Is(err, adminaccess.ErrElevationNotReviewable) {
		t.Fatalf("a confirmation of a terminal review: %v, want %v", err, adminaccess.ErrElevationNotReviewable)
	}
	if grantsOf(t, pool, ended.ID)[0].ConfirmedBy != 0 || standingRoles(t, pool, grantee) != 0 {
		t.Fatal("a grant of an unreviewed elevation was confirmed")
	}
}
