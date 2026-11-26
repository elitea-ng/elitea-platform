package legacyrbac_test

// The provisional grants in the resolver (ADR-0059 decision 5; stories
// R2.12a, issue 2526, and R2.12b, issue 2606), on the real schema and the
// resolver's test clock:
//
//   - a role given under an elevation is in the grantee's full
//     administration resolution while the elevation is active, as a
//     permission that came from an elevation, and not a second after it
//     expires;
//   - the standing-set call never includes it;
//   - a membership given under an elevation grants the project's role
//     permissions while the elevation is active, on the single-project and
//     the all-memberships paths, and nothing after it.

import (
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/provisionalgrants"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/legacyrbac"
)

const provisionalTestPermission = "provisional.fixture.read"

func TestAProvisionalRoleResolvesOnlyWhileItsElevationIsActive(t *testing.T) {
	provisionalgrants.EnableForTest(t)
	f := newElevationFixture(t)
	elevation := f.elevate(t)
	grantee := seedElevationPerson(t, f.pool)
	session := seedElevationSession(t, f.pool, grantee)
	caller := adminaccess.Caller{UserID: f.holder, Elevated: true,
		Elevations: []auth.ElevationGrant{{ID: elevation.ID.String(), Level: elevation.Level, Path: elevation.Path}}}
	if _, err := adminaccess.GiveProvisionalRoles(t.Context(), f.pool, caller, grantee, adminaccess.ModeAdministration,
		[]string{adminaccess.RoleAdmin}, elevationTestStart.Add(time.Minute)); err != nil {
		t.Fatalf("give: %v", err)
	}
	id := strconv.FormatInt(grantee, 10)
	principal := auth.User{ID: id, UserID: id, AuthType: "session", CredentialKind: auth.CredentialKindSession,
		BrowserSessionRef: session}

	got := f.administration(t, principal)
	if !slices.Equal(got.Permissions, f.adminSet) || !slices.Equal(got.ElevatedPermissions, f.adminSet) {
		t.Fatalf("the grantee resolves %v (elevated %v), want the admin set from the elevation", got.Permissions, got.ElevatedPermissions)
	}
	if !slices.Equal(got.ElevationIDs, []string{elevation.ID.String()}) || len(got.Roles) != 0 {
		t.Fatalf("the grantee names the elevations %v and the roles %v", got.ElevationIDs, got.Roles)
	}
	noElevation(t, "the grantee's standing set", f.standing(t, principal), []string{})

	legacyrbac.SetClockForTest(f.resolver, func() time.Time { return elevation.ExpiresAt.Add(time.Second) })
	noElevation(t, "the grantee a second after the expiry", f.administration(t, principal), []string{})
}

func TestAProvisionalMembershipResolvesOnlyWhileItsElevationIsActive(t *testing.T) {
	provisionalgrants.EnableForTest(t)
	f := newElevationFixture(t)
	elevation := f.elevate(t)
	grantee := seedElevationPerson(t, f.pool)
	var project int64
	if err := f.pool.QueryRow(t.Context(),
		`INSERT INTO centry.project (name, owner_id) VALUES ('provisional-fixture', 1) RETURNING id`).Scan(&project); err != nil {
		t.Fatal(err)
	}
	var role int
	if err := f.pool.QueryRow(t.Context(),
		`INSERT INTO public.auth_core__project_role (project_id, name) VALUES ($1, 'editor') RETURNING id`, project).Scan(&role); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(t.Context(), `
INSERT INTO public.auth_core__project_role_permission (project_id, role_id, permission) VALUES ($1, $2, $3)`,
		project, role, provisionalTestPermission); err != nil {
		t.Fatal(err)
	}
	caller := adminaccess.Caller{UserID: f.holder, Elevated: true,
		Elevations: []auth.ElevationGrant{{ID: elevation.ID.String(), Level: elevation.Level, Path: elevation.Path}}}
	if err := adminaccess.GiveProvisionalMembership(t.Context(), f.pool, caller, project, grantee, []int{role},
		elevationTestStart.Add(time.Minute)); err != nil {
		t.Fatalf("give: %v", err)
	}
	id := strconv.FormatInt(grantee, 10)
	// Any client kind: a membership is not bound to the session.
	principal := auth.User{ID: id, UserID: id, AuthType: "session", CredentialKind: auth.CredentialKindSession}
	want := []string{provisionalTestPermission}
	resolve := func() ([]string, []string) {
		t.Helper()
		one, err := f.resolver.ResolvePermissions(t.Context(), principal, auth.PermissionModeDefault, strconv.FormatInt(project, 10))
		if err != nil {
			t.Fatalf("resolve the project: %v", err)
		}
		all, err := f.resolver.ResolveMembershipPermissions(t.Context(), principal, auth.PermissionModeDefault)
		if err != nil {
			t.Fatalf("resolve the memberships: %v", err)
		}
		return one.Permissions, all.Permissions
	}
	if one, all := resolve(); !slices.Equal(one, want) || !slices.Equal(all, want) {
		t.Fatalf("during the elevation: project %v, memberships %v, want %v", one, all, want)
	}

	legacyrbac.SetClockForTest(f.resolver, func() time.Time { return elevation.ExpiresAt.Add(time.Second) })
	if one, all := resolve(); len(one) != 0 || len(all) != 0 {
		t.Fatalf("a second after the expiry: project %v, memberships %v, want nothing", one, all)
	}
}
