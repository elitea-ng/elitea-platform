package configurations_test

// The reviewed Configurations routes (read.go, mutation.go) serve the
// mode-less project configuration patterns wherever cmd/elitea-main composes
// them, shadowing the compatibility routes at the same patterns. They carry
// the same central branch of the guarded project settings (ADR-0059 R0.6b)
// and the same guarded-type rule (#3024).

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	apimw "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware/authtest"
	handler "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/configurations"
	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

// The rows the fake reader holds: a guarded setting and a toolkit credential.
const (
	guardedRowID    = 21
	credentialRowID = 22
)

var centralRows = currentMutationReader{types: map[int32]string{
	guardedRowID:    configurationapp.CloudExecutorConfigurationType,
	credentialRowID: "github",
}}

// grants is a caller's permissions: in the project (nil: not a member) and
// in the administration mode.
type grants struct{ project, central []string }

func (g grants) resolver() auth.PermissionResolver {
	return permissionResolverFunc(func(_ context.Context, _ auth.User, mode, projectID string) (auth.PermissionResolution, error) {
		switch {
		case mode == auth.PermissionModeAdministration && projectID == "":
			return auth.PermissionResolution{UserID: 11, Permissions: g.central}, nil
		case mode == auth.PermissionModeDefault && projectID == "7" && g.project != nil:
			return auth.PermissionResolution{UserID: 11, Permissions: g.project}, nil
		}
		return auth.PermissionResolution{}, auth.ErrPermissionDenied
	})
}

func centralAuth() apimw.AuthConfig {
	principal := currentReadPrincipalValidatorFunc(func(_ context.Context, user auth.User) (auth.User, error) {
		return user, nil
	})
	return apimw.AuthConfig{PrincipalValidator: principal, FormSessions: authtest.FormSessions{}}
}

func centralMutationRoute(t *testing.T, g grants, mutator handler.CurrentConfigurationMutator) http.Handler {
	t.Helper()
	route, err := handler.NewCurrentConfigurationMutationRoute(mutator, centralRows, centralAuth(), g.resolver())
	if err != nil {
		t.Fatal(err)
	}
	return route
}

func centralReadRoute(t *testing.T, g grants) http.Handler {
	t.Helper()
	route, err := handler.NewCurrentConfigurationReadRoute(centralRows, 1, centralAuth(), g.resolver())
	if err != nil {
		t.Fatal(err)
	}
	return route
}

func createBody(typeName string) string {
	return `{"elitea_title":"setting","type":"` + typeName + `","data":{}}`
}

// The acceptance of #3024: a central projects.settings.manage holder who is
// not a member writes a guarded setting through the mode-less create, and the
// route refuses it every type outside the allowlist.
func TestCurrentConfigurationWritesTakeTheCentralSettingsBranch(t *testing.T) {
	operator := grants{central: []string{adminaccess.ProjectsSettingsManage}}
	for _, test := range []struct {
		name, method, path, body string
		want                     int
	}{
		{"create a guarded setting", http.MethodPost, "/api/v2/configurations/configurations/7",
			createBody(configurationapp.CloudExecutorConfigurationType), http.StatusOK},
		{"create a toolkit credential", http.MethodPost, "/api/v2/configurations/configurations/7",
			createBody("github"), http.StatusForbidden},
		{"create a model credential", http.MethodPost, "/api/v2/configurations/configurations/7",
			createBody("open_ai"), http.StatusForbidden},
		{"update a guarded setting", http.MethodPut, "/api/v2/configurations/configuration/7/21",
			`{"data":{}}`, http.StatusOK},
		{"update a toolkit credential", http.MethodPut, "/api/v2/configurations/configuration/7/22",
			`{"data":{}}`, http.StatusForbidden},
		{"delete a guarded setting", http.MethodDelete, "/api/v2/configurations/configuration/7/21", "", http.StatusNoContent},
		{"delete a toolkit credential", http.MethodDelete, "/api/v2/configurations/configuration/7/22", "", http.StatusForbidden},
		{"delete an absent row", http.MethodDelete, "/api/v2/configurations/configuration/7/99", "", http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			mutator := &currentConfigurationMutatorStub{}
			response := httptest.NewRecorder()
			centralMutationRoute(t, operator, mutator).ServeHTTP(response, currentMutationRequest(test.method, test.path, test.body))
			if response.Code != test.want {
				t.Fatalf("status=%d, want %d; body=%s", response.Code, test.want, response.Body.String())
			}
			if reached := mutator.calls != 0; reached != (test.want < 400) {
				t.Fatalf("mutator calls=%d for a %d answer", mutator.calls, response.Code)
			}
		})
	}

	// projects.settings.view reads and does not write.
	viewer := grants{central: []string{adminaccess.ProjectsSettingsView}}
	mutator := &currentConfigurationMutatorStub{}
	response := httptest.NewRecorder()
	centralMutationRoute(t, viewer, mutator).ServeHTTP(response, currentMutationRequest(http.MethodPost,
		"/api/v2/configurations/configurations/7", createBody(configurationapp.CloudExecutorConfigurationType)))
	if response.Code != http.StatusForbidden || mutator.calls != 0 {
		t.Fatalf("a projects.settings.view holder created a setting: status=%d calls=%d", response.Code, mutator.calls)
	}
}

// The guarded-type rule of the compatibility routes: a member who may create
// configurations and lacks models.project_settings.edit writes no guarded
// setting, and writes every other type as before.
func TestCurrentConfigurationWritesHoldGuardedSettingsToTheSettingsPermission(t *testing.T) {
	writer := []string{handler.CurrentConfigurationCreatePermission, handler.CurrentConfigurationUpdatePermission,
		handler.CurrentConfigurationDeletePermission}
	editor := grants{project: writer}
	settingsEditor := grants{project: append([]string{handler.ProjectSettingsEditPermission}, writer...)}
	for _, test := range []struct {
		name         string
		caller       grants
		method, path string
		body         string
		want         int
	}{
		{"editor creates a guarded setting", editor, http.MethodPost, "/api/v2/configurations/configurations/7",
			createBody(configurationapp.WorkerEgressConfigurationType), http.StatusForbidden},
		{"editor updates a guarded setting", editor, http.MethodPut, "/api/v2/configurations/configuration/7/21",
			`{"data":{}}`, http.StatusForbidden},
		{"editor deletes a guarded setting", editor, http.MethodDelete, "/api/v2/configurations/configuration/7/21",
			"", http.StatusForbidden},
		{"editor creates a toolkit credential", editor, http.MethodPost, "/api/v2/configurations/configurations/7",
			createBody("github"), http.StatusOK},
		{"editor updates a toolkit credential", editor, http.MethodPut, "/api/v2/configurations/configuration/7/22",
			`{"data":{}}`, http.StatusOK},
		{"settings editor creates a guarded setting", settingsEditor, http.MethodPost,
			"/api/v2/configurations/configurations/7",
			createBody(configurationapp.WorkerEgressConfigurationType), http.StatusOK},
		{"settings editor deletes a guarded setting", settingsEditor, http.MethodDelete,
			"/api/v2/configurations/configuration/7/21", "", http.StatusNoContent},
	} {
		t.Run(test.name, func(t *testing.T) {
			mutator := &currentConfigurationMutatorStub{}
			response := httptest.NewRecorder()
			centralMutationRoute(t, test.caller, mutator).ServeHTTP(response,
				currentMutationRequest(test.method, test.path, test.body))
			if response.Code != test.want {
				t.Fatalf("status=%d, want %d; body=%s", response.Code, test.want, response.Body.String())
			}
			if test.want == http.StatusForbidden {
				assertCurrentMutationError(t, response, http.StatusForbidden,
					"this setting needs the project settings permission", "type")
			}
		})
	}
}

// The reads: projects.settings.view lists the guarded types it names and
// reads a guarded row; a list with no type, or of another type, and a
// credential row stay refused.
func TestCurrentConfigurationReadsTakeTheCentralSettingsBranch(t *testing.T) {
	viewer := grants{central: []string{adminaccess.ProjectsSettingsView}}
	for _, test := range []struct {
		name, path string
		want       int
	}{
		{"list a guarded type", "/api/v2/configurations/configurations/7?type=cloud_executor", http.StatusOK},
		{"list two guarded types", "/api/v2/configurations/configurations/7?type=browser&type=computer_use", http.StatusOK},
		{"list with no type", "/api/v2/configurations/configurations/7", http.StatusForbidden},
		{"list a guarded and a credential type", "/api/v2/configurations/configurations/7?type=browser&type=github",
			http.StatusForbidden},
		{"read a guarded row", "/api/v2/configurations/configuration/7/21", http.StatusOK},
		{"read a credential row", "/api/v2/configurations/configuration/7/22", http.StatusForbidden},
		{"read an absent row", "/api/v2/configurations/configuration/7/99", http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			centralReadRoute(t, viewer).ServeHTTP(response, currentReadRequest(http.MethodGet, test.path, "10.0.0.8:43120"))
			if response.Code != test.want {
				t.Fatalf("status=%d, want %d; body=%s", response.Code, test.want, response.Body.String())
			}
		})
	}

	// No central permission: the member gate alone, as before.
	response := httptest.NewRecorder()
	centralReadRoute(t, grants{}).ServeHTTP(response, currentReadRequest(http.MethodGet,
		"/api/v2/configurations/configurations/7?type=cloud_executor", "10.0.0.8:43120"))
	if response.Code != http.StatusForbidden {
		t.Fatalf("a caller with no grant read the settings: status=%d", response.Code)
	}
}
