package eliteacore_test

// The project self-membership check reads the super_admin exemption from the
// standing set (ADR-0059 decision 3, "The exemption is read from standing
// roles only"; story R2.1b, issue 2595), against the REAL legacyrbac resolver.
// A person whose standing role is `admin`, under an ACTIVE super_admin-level
// elevation bound to the session of the request, resolves
// admin.auth.users.super_admin (the premise) and still cannot add itself to a
// project it is not a member of through the central member route. The
// standing super_admin can (the control).

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/legacyrbac"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/migrations"
)

const (
	elevatedAdminEmail     = "e2e-elevated-admin@autotest.local"
	standingSuperEmail     = "e2e-standing-super@autotest.local"
	elevatedMemberSession  = int64(930001)
	standingMemberSession  = int64(930002)
	elevationTestRoleAdmin = "admin"
)

// elevatedMemberEnvironment seeds the administration roles admin and
// super_admin (super_admin with the permission that makes it one), a person
// who holds admin and one who holds super_admin, the elevation ledger, and an
// active super_admin-level elevation of the admin bound to
// elevatedMemberSession.
func elevatedMemberEnvironment(t *testing.T) (*pgxpool.Pool, map[string]int) {
	t.Helper()
	pool := newUsersWritePostgresPool(t)
	prepareEscalationFixture(t, pool)
	ctx := context.Background()
	for _, statement := range []string{
		`INSERT INTO public.auth_core__role (name, mode)
			SELECT name, 'administration' FROM unnest(ARRAY['admin', 'super_admin']) AS name
			WHERE NOT EXISTS (SELECT 1 FROM public.auth_core__role role
			                  WHERE role.name = name AND role.mode = 'administration')`,
		fmt.Sprintf(`INSERT INTO public.auth_core__role_permission (role_id, permission)
			SELECT id, '%s' FROM public.auth_core__role WHERE name = 'super_admin' AND mode = 'administration'`,
			adminaccess.SuperAdminPermission),
		fmt.Sprintf(`INSERT INTO public.auth_core__role_permission (role_id, permission)
			SELECT role.id, permission FROM public.auth_core__role role,
			unnest(ARRAY['%s', '%s']) AS permission
			WHERE role.name IN ('admin', 'super_admin') AND role.mode = 'administration'`,
			adminaccess.AdminAuthUsers, adminaccess.ProjectsMembersManage),
		fmt.Sprintf(`INSERT INTO public.auth_core__user (email, name) VALUES ('%s', 'Elevated'), ('%s', 'Super')`,
			elevatedAdminEmail, standingSuperEmail),
		fmt.Sprintf(`INSERT INTO public.auth_core__user_role (user_id, role_id)
			SELECT account.id, role.id FROM public.auth_core__user account
			JOIN public.auth_core__role role ON role.mode = 'administration'
			WHERE (account.email = '%s' AND role.name = 'admin')
			   OR (account.email = '%s' AND role.name = 'super_admin')`,
			elevatedAdminEmail, standingSuperEmail),
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("seed %q: %v", statement, err)
		}
	}
	ids := map[string]int{}
	for _, email := range []string{elevatedAdminEmail, standingSuperEmail} {
		_, ids[email] = escalationRouter(t, pool, email)
	}
	ledger, err := migrations.Files.ReadFile("shared/0269_admin_elevations.sql")
	if err != nil {
		t.Fatalf("read the elevation ledger migration: %v", err)
	}
	if _, err := pool.Exec(ctx, string(ledger)); err != nil {
		t.Fatalf("apply the elevation ledger migration: %v", err)
	}
	// Written directly: the ledger function also checks the browser session
	// row, which this fixture's schema does not have.
	now := time.Now().UTC()
	if _, err := pool.Exec(ctx, `
INSERT INTO elitea_auth.admin_elevations
    (user_id, browser_session_ref, level, path, reason_code, reason, requester_user_id, approver_user_id,
     requested_at, decision, decided_at, started_at, expires_at)
VALUES ($1, $2, 'super_admin', 'approved', 'incident', 'R2.1b: project self-membership', $1, $3, $4,
        'approved', $4, $4, $5)`,
		ids[elevatedAdminEmail], elevatedMemberSession, ids[standingSuperEmail],
		now.Add(-time.Minute), now.Add(time.Hour)); err != nil {
		t.Fatalf("seed the elevation: %v", err)
	}
	return pool, ids
}

func sessionMemberRouter(pool *pgxpool.Pool, id int, sessionRef int64) (chi.Router, auth.User) {
	principal := auth.User{ID: fmt.Sprint(id), UserID: fmt.Sprint(id), CredentialKind: auth.CredentialKindSession,
		BrowserSessionRef: sessionRef}
	handler := eliteacore.NewHandler(pool, eliteacore.WithPermissionResolver(legacyrbac.NewPostgresResolver(pool)))
	return usersWriteRouterAs(handler, principal), principal
}

func TestASuperAdminLevelElevationCannotAddItsHolderToAProject(t *testing.T) {
	pool, ids := elevatedMemberEnvironment(t)
	id := ids[elevatedAdminEmail]
	router, principal := sessionMemberRouter(pool, id, elevatedMemberSession)
	resolution, err := legacyrbac.NewPostgresResolver(pool).ResolvePermissions(
		context.Background(), principal, adminaccess.ModeAdministration, "")
	if err != nil || !slices.Contains(resolution.ElevatedPermissions, adminaccess.SuperAdminPermission) {
		t.Fatalf("premise: the elevated session resolves %+v (%v), want %s from the elevation",
			resolution, err, adminaccess.SuperAdminPermission)
	}

	recorder := usersWriteDo(t, router, http.MethodPost, centralMembersPath,
		map[string]any{"emails": []string{elevatedAdminEmail}, "roles": []string{elevationTestRoleAdmin}})
	expectSelfEscalation(t, recorder.Code, recorder.Body.Bytes())
	recorder = usersWriteDo(t, router, http.MethodPut, centralMembersPath,
		map[string]any{"userId": fmt.Sprint(id), "roles": []string{elevationTestRoleAdmin}})
	expectSelfEscalation(t, recorder.Code, recorder.Body.Bytes())
	if roles := projectRolesOf(t, pool, id); len(roles) != 0 {
		t.Fatalf("the elevated admin holds %v in the project after the refusals", roles)
	}

	// The control: the standing global administrator adds itself.
	super, _ := sessionMemberRouter(pool, ids[standingSuperEmail], standingMemberSession)
	recorder = usersWriteDo(t, super, http.MethodPost, centralMembersPath,
		map[string]any{"emails": []string{standingSuperEmail}, "roles": []string{elevationTestRoleAdmin}})
	if recorder.Code != http.StatusOK {
		t.Fatalf("the standing super_admin adding itself: status = %d body %s, want 200",
			recorder.Code, recorder.Body.String())
	}
}
