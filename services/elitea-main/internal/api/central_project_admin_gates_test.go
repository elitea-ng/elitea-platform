package api

// Central project administration (ADR-0059 R0.6a #2495, R0.6b #2585): the
// administration member routes accept projects.members.manage beside the
// legacy configuration.users.users.* names, and the project configurations
// routes admit projects.settings.view / .manage for the five guarded settings
// of any project.
//
// Every request goes through the production router (NewRouter), so the gate
// under test is the one router.go composes.
//
// The legacy holders' answers are pinned in
// testdata/central_project_admin_legacy.golden, captured on main before the
// gates changed: a holder of the legacy member names, and a project member
// with the configuration permissions and models.project_settings.edit, see
// no change.

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	v2configs "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/configurations"
	v2skills "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/skills"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

const goldenCentralProjectAdminLegacy = "testdata/central_project_admin_legacy.golden"

// centralProjectAdminRoutes are the routes the two stories touch, in the
// order of the golden file. The bodies are the smallest a handler accepts as
// JSON.
var centralProjectAdminRoutes = []platformGateRoute{
	{method: http.MethodGet, path: "/api/v2/admin/users/administration/7"},
	{method: http.MethodPost, path: "/api/v2/admin/users/administration/7", body: `{}`},
	{method: http.MethodPut, path: "/api/v2/admin/users/administration/7", body: `{}`},
	{method: http.MethodDelete, path: "/api/v2/admin/users/administration/7?id=5"},
	{method: http.MethodGet, path: "/api/v2/configurations/configurations/7?type=cloud_executor"},
	{method: http.MethodGet, path: "/api/v2/configurations/configuration/7/3"},
	{method: http.MethodPost, path: "/api/v2/configurations/configurations/7", body: `{"type":"cloud_executor"}`},
	{method: http.MethodPut, path: "/api/v2/configurations/configuration/7/3", body: `{"type":"cloud_executor"}`},
	{method: http.MethodDelete, path: "/api/v2/configurations/configuration/7/3"},
}

// centralProjectAdminLegacyHolders are the two legacy holders the golden
// file pins, one line set each.
var centralProjectAdminLegacyHolders = []struct {
	name    string
	granted []string
}{
	{"members", []string{
		adminaccess.ConfigurationUsersUsersView, adminaccess.ConfigurationUsersUsersCreate,
		adminaccess.ConfigurationUsersUsersEdit, "configuration.users.users.delete",
	}},
	{"settings", []string{
		v2configs.CurrentConfigurationListPermission, v2configs.CurrentConfigurationGetPermission,
		v2configs.CurrentConfigurationCreatePermission, v2configs.CurrentConfigurationUpdatePermission,
		v2configs.CurrentConfigurationDeletePermission, v2configs.ProjectSettingsEditPermission,
	}},
}

// TestCentralProjectRoutesAnswerTheLegacyHoldersAsBefore pins every touched
// route's answer to each legacy holder. Regenerate the file with
// UPDATE_GOLDEN=1 only for a change that means to alter what a legacy holder
// sees.
func TestCentralProjectRoutesAnswerTheLegacyHoldersAsBefore(t *testing.T) {
	var b strings.Builder
	for _, holder := range centralProjectAdminLegacyHolders {
		router := newPlatformGateRouter(t, holder.granted...)
		for _, route := range centralProjectAdminRoutes {
			rec := servePlatformGate(t, router, route.method, route.path, route.body)
			fmt.Fprintf(&b, "%s\t%s\t%s\t%d\n", holder.name, route.method, route.path, rec.Code)
		}
	}
	got := b.String()

	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(goldenCentralProjectAdminLegacy), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenCentralProjectAdminLegacy, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(goldenCentralProjectAdminLegacy)
	if err != nil {
		t.Fatalf("read %s: %v (regenerate with UPDATE_GOLDEN=1)", goldenCentralProjectAdminLegacy, err)
	}
	if got != string(want) {
		t.Errorf("a legacy holder gets different answers than before:\n got:\n%s\nwant:\n%s", got, want)
	}
}

// modeResolver grants `central` in the administration mode and `member` in
// every project; a caller with no member set is refused in a project, as a
// non-member is.
type modeResolver struct{ central, member []string }

func (m modeResolver) ResolvePermissions(
	_ context.Context, _ auth.User, mode, _ string,
) (auth.PermissionResolution, error) {
	if mode == auth.PermissionModeAdministration {
		return auth.PermissionResolution{UserID: 1, Permissions: append([]string(nil), m.central...)}, nil
	}
	if len(m.member) == 0 {
		return auth.PermissionResolution{}, auth.ErrPermissionDenied
	}
	return auth.PermissionResolution{UserID: 1, Permissions: append([]string(nil), m.member...)}, nil
}

func (m modeResolver) ResolveMembershipPermissions(
	ctx context.Context, user auth.User, mode string,
) (auth.PermissionResolution, error) {
	return m.ResolvePermissions(ctx, user, mode, "")
}

// newModeRouter is the production router with no database and modeResolver.
func newModeRouter(t *testing.T, resolver modeResolver) http.Handler {
	t.Helper()
	return NewRouter(RouterConfig{
		SkillsRepo:         struct{ v2skills.Repository }{},
		AuthValidator:      testTokenValidator{user: authenticatedTestUser()},
		PrincipalValidator: testPrincipalValidator{},
		PermissionResolver: resolver,
	})
}

func projectAdministratorSet(t *testing.T) []string {
	t.Helper()
	role, ok := adminaccess.LookupBuiltInRole(adminaccess.RoleProjectAdministrator)
	if !ok {
		t.Fatal("project_administrator is not a built-in role")
	}
	return role.Set()
}

// The four member routes admit projects.members.manage alone (the handler
// behind answers without a database), and refuse a caller who holds nothing
// of them. The neighbours that write roles keep their own names.
func TestMemberRoutesAdmitMembersManage(t *testing.T) {
	narrow := newPlatformGateRouter(t, adminaccess.ProjectsMembersManage)
	nothing := newPlatformGateRouter(t, adminaccess.ProjectsSettingsManage)
	for _, route := range centralProjectAdminRoutes[:4] {
		if got := servePlatformGate(t, narrow, route.method, route.path, route.body).Code; got == http.StatusForbidden {
			t.Errorf("projects.members.manage: %s %s = 403", route.method, route.path)
		}
		if got := servePlatformGate(t, nothing, route.method, route.path, route.body).Code; got != http.StatusForbidden {
			t.Errorf("without it: %s %s = %d, want 403", route.method, route.path, got)
		}
	}
	for _, route := range []platformGateRoute{
		{method: http.MethodPut, path: "/api/v2/admin/user_project_permissions/administration", body: "{}"},
		{method: http.MethodPost, path: "/api/v2/admin/modes/administration", body: "{}"},
		{method: http.MethodPut, path: "/api/v2/admin/permissions/administration/default", body: "{}"},
	} {
		if got := servePlatformGate(t, narrow, route.method, route.path, route.body).Code; got != http.StatusForbidden {
			t.Errorf("projects.members.manage: %s %s = %d, want 403 (not this story's route)", route.method, route.path, got)
		}
	}
}

// The project administrator set reaches the project matrix through no gate:
// its self rule is proved behind the gate in
// v2/admin/project_administrator_self_postgres_integration_test.go.
func TestTheProjectAdministratorSetDoesNotReachTheProjectMatrix(t *testing.T) {
	router := newModeRouter(t, modeResolver{central: projectAdministratorSet(t)})
	got := servePlatformGate(t, router, http.MethodPut, "/api/v2/admin/user_project_permissions/administration", "{}").Code
	if got != http.StatusForbidden {
		t.Fatalf("PUT user_project_permissions = %d, want 403", got)
	}
}

// The settings override reaches the guarded settings of a project the caller
// is not a member of, and no other configuration route.
func TestSettingsOverrideOnTheComposedRouter(t *testing.T) {
	router := newModeRouter(t, modeResolver{central: projectAdministratorSet(t)})
	for _, route := range []platformGateRoute{
		{method: http.MethodGet, path: "/api/v2/configurations/configurations/7?type=cloud_executor"},
		{method: http.MethodPost, path: "/api/v2/configurations/configurations/7", body: `{"type":"worker_egress"}`},
	} {
		if got := servePlatformGate(t, router, route.method, route.path, route.body).Code; got == http.StatusForbidden {
			t.Errorf("%s %s = 403, want past the gate", route.method, route.path)
		}
	}
	for _, route := range []platformGateRoute{
		{method: http.MethodGet, path: "/api/v2/configurations/configurations/7"},
		{method: http.MethodGet, path: "/api/v2/configurations/configurations/7?type=github"},
		{method: http.MethodPost, path: "/api/v2/configurations/configurations/7", body: `{"type":"github"}`},
		{method: http.MethodPost, path: "/api/v2/configurations/configurations/7", body: `{"type":"llm_model"}`},
		{method: http.MethodPost, path: "/api/v2/configurations/check_connections/7", body: `{}`},
		{method: http.MethodGet, path: "/api/v2/configurations/models/7"},
	} {
		if got := servePlatformGate(t, router, route.method, route.path, route.body).Code; got != http.StatusForbidden {
			t.Errorf("%s %s = %d, want 403", route.method, route.path, got)
		}
	}
}
