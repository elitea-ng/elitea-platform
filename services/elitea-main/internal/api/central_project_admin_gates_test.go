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
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	v2configs "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/configurations"
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
