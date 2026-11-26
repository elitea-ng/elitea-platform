package adminaccess_test

// The role rules read the caller from the standing-set call (ADR-0059
// decision 3, "The exemption is read from standing roles only"; story R2.1b,
// issue 2595). A person whose standing role is `admin`, under a
// super_admin-level elevation, is resolved without the elevation: not
// exempt, and refused when it assigns a role to itself or edits a role it
// holds. The same rules against the real resolver and the real routes are in
// internal/api/v2/admin/role_elevation_standing_postgres_integration_test.go
// and internal/api/v2/eliteacore/users_central_elevation_postgres_integration_test.go.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

const elevatedCaller = 7

// elevatedResolver answers an admin's standing set plus a super_admin-level
// elevation, the shape legacyrbac answers for the bound session. It has a
// standing-set call when standing is set, and none otherwise.
type elevatedResolver struct{ calls *[]string }

func (e elevatedResolver) ResolvePermissions(context.Context, auth.User, string, string) (auth.PermissionResolution, error) {
	*e.calls = append(*e.calls, "full")
	return auth.PermissionResolution{
		UserID:              elevatedCaller,
		Permissions:         []string{adminaccess.AdminAuthUsers, adminaccess.SuperAdminPermission},
		ElevatedPermissions: []string{adminaccess.SuperAdminPermission},
		ElevationIDs:        []string{"0b7c2d4e-0000-4000-8000-000000000001"},
		Roles:               []string{adminaccess.RoleAdmin},
	}, nil
}

type elevatedStandingResolver struct{ elevatedResolver }

func (e elevatedStandingResolver) ResolveStandingAdministration(context.Context, auth.User) (auth.PermissionResolution, error) {
	*e.calls = append(*e.calls, "standing")
	return auth.PermissionResolution{
		UserID: elevatedCaller, Permissions: []string{adminaccess.AdminAuthUsers}, Roles: []string{adminaccess.RoleAdmin},
	}, nil
}

func elevatedRequest() *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/admin", nil)
	return request.WithContext(auth.ContextWithUser(request.Context(),
		auth.User{ID: "7", UserID: "7", CredentialKind: auth.CredentialKindSession, BrowserSessionRef: 41}))
}

func TestAnElevatedCallerIsResolvedFromItsStandingSet(t *testing.T) {
	for name, resolver := range map[string]func(*[]string) auth.PermissionResolver{
		"with the standing-set call": func(calls *[]string) auth.PermissionResolver {
			return elevatedStandingResolver{elevatedResolver{calls: calls}}
		},
		"without it": func(calls *[]string) auth.PermissionResolver { return elevatedResolver{calls: calls} },
	} {
		t.Run(name, func(t *testing.T) {
			var calls []string
			caller, refusal := adminaccess.ResolveCaller(elevatedRequest(), resolver(&calls))
			if refusal != nil {
				t.Fatalf("the caller was not resolved: %+v", refusal)
			}
			if caller.SuperAdmin || caller.Administration[adminaccess.SuperAdminPermission] {
				t.Fatalf("a super_admin-level elevation made the caller exempt: %+v", caller)
			}
			if !caller.Administration[adminaccess.AdminAuthUsers] {
				t.Fatalf("the standing set was lost: %+v", caller.Administration)
			}
			// The set is the standing-set call's; the full resolution is read
			// after it only to learn that the session is elevated (R2.8b).
			if name == "with the standing-set call" && !slices.Equal(calls, []string{"standing", "full"}) {
				t.Fatalf("the caller was resolved through %v, want the standing-set call, then the elevation", calls)
			}
			if !caller.Elevated || caller.Effective[adminaccess.SuperAdminPermission] != true {
				t.Fatalf("the elevation was not read beside the standing set: %+v", caller)
			}

			// Assigning a role to itself.
			if refusal := adminaccess.CheckSelfAssignment(caller.UserID, caller.SuperAdmin, elevatedCaller,
				adminaccess.RoleAdmin); refusal == nil || refusal.Code != adminaccess.CodeSelfAssignment {
				t.Fatalf("self-assignment under the elevation: %+v, want %s", refusal, adminaccess.CodeSelfAssignment)
			}
			// Editing a role it holds.
			refusal = adminaccess.CheckCentralEdit(adminaccess.CentralEdit{
				Caller:     caller.Administration,
				SuperAdmin: caller.SuperAdmin,
				HeldRoles:  map[string]bool{adminaccess.RoleAdmin: true},
				Mode:       adminaccess.ModeAdministration,
				Revoked:    []adminaccess.Cell{{Role: adminaccess.RoleAdmin, Permission: adminaccess.AdminAuthUsers}},
			})
			if refusal == nil || refusal.Code != adminaccess.CodeOwnRoleLocked {
				t.Fatalf("editing the held role under the elevation: %+v, want %s", refusal, adminaccess.CodeOwnRoleLocked)
			}

			// Reload reads the standing set too.
			reloaded, err := caller.Reload(context.Background())
			if err != nil || reloaded.SuperAdmin {
				t.Fatalf("the reloaded caller: %+v (%v), want not exempt", reloaded, err)
			}
		})
	}
}
