package configurations_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	apimw "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware/authtest"
	handler "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/configurations"
	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

// tracingAccessStub answers CanManageTracing with allowed and records the
// (project, user) pairs it was asked about.
type tracingAccessStub struct {
	allowed bool
	err     error
	asked   [][2]int64
}

func (stub *tracingAccessStub) CanManageTracing(_ context.Context, projectID, userID int64) (bool, error) {
	stub.asked = append(stub.asked, [2]int64{projectID, userID})
	return stub.allowed, stub.err
}

func testTracingRule(t *testing.T, allowed bool) handler.TracingRule {
	t.Helper()
	return testTracingRuleWith(t, &tracingAccessStub{allowed: allowed})
}

func testTracingRuleWith(t *testing.T, access handler.TracingAccess) handler.TracingRule {
	t.Helper()
	catalog, err := configurationapp.LoadPinnedCurrentAvailableCatalog()
	if err != nil {
		t.Fatal(err)
	}
	rule, err := handler.NewTracingRule(catalog, access)
	if err != nil {
		t.Fatal(err)
	}
	return rule
}

func TestNewTracingRuleRequiresTheCatalogueAndTheAccessCheck(t *testing.T) {
	catalog, err := configurationapp.LoadPinnedCurrentAvailableCatalog()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handler.NewTracingRule(nil, &tracingAccessStub{}); !errors.Is(err, handler.ErrInvalidTracingRule) {
		t.Errorf("no catalogue: error = %v, want %v", err, handler.ErrInvalidTracingRule)
	}
	if _, err := handler.NewTracingRule(catalog, nil); !errors.Is(err, handler.ErrInvalidTracingRule) {
		t.Errorf("no access check: error = %v, want %v", err, handler.ErrInvalidTracingRule)
	}
}

// allowEveryCurrentMutationPermission grants the three write permissions and
// the read by id in project 7, so the route gate admits the caller and only
// the handler's rules decide.
func allowEveryCurrentMutationPermission() auth.PermissionResolver {
	return permissionResolverFunc(func(_ context.Context, _ auth.User, mode, projectID string) (auth.PermissionResolution, error) {
		if mode != auth.PermissionModeDefault || projectID != "7" {
			return auth.PermissionResolution{}, errors.New("unexpected permission scope")
		}
		return auth.PermissionResolution{UserID: 11, Permissions: []string{
			handler.CurrentConfigurationCreatePermission,
			handler.CurrentConfigurationUpdatePermission,
			handler.CurrentConfigurationDeletePermission,
			handler.CurrentConfigurationGetPermission,
		}}, nil
	})
}

// TestReviewedConfigurationWritesApplyTheTracingRule is the acceptance test
// of #3032: a caller who holds the write permissions and does not manage
// tracing is refused a create (on the body type), an update and a delete (on
// the stored type) of a tracing configuration, with the compatibility
// route's 403, and nothing reaches the mutator; a caller who manages tracing
// gets through, and so does a write of another type.
func TestReviewedConfigurationWritesApplyTheTracingRule(t *testing.T) {
	const tracingRow, otherRow = 9, 10
	reader := currentMutationReader{types: map[int32]string{tracingRow: "langfuse", otherRow: "github"}}
	writes := []struct {
		name, method, path, body string
		success                  int
	}{
		{"create a tracing type", http.MethodPost, "/api/v2/configurations/configurations/7",
			`{"elitea_title":"trace","type":"langfuse","data":{}}`, http.StatusOK},
		{"update a tracing row", http.MethodPut, "/api/v2/configurations/configuration/7/9", `{}`, http.StatusOK},
		{"delete a tracing row", http.MethodDelete, "/api/v2/configurations/configuration/7/9", ``, http.StatusNoContent},
	}
	for _, write := range writes {
		for _, allowed := range []bool{false, true} {
			name := write.name + map[bool]string{false: ", tracing not managed", true: ", tracing managed"}[allowed]
			t.Run(name, func(t *testing.T) {
				access := &tracingAccessStub{allowed: allowed}
				mutator := &currentConfigurationMutatorStub{}
				route := newTracingMutationRoute(t, mutator, reader, access)
				response := httptest.NewRecorder()
				route.ServeHTTP(response, currentMutationRequest(write.method, write.path, write.body))
				if !allowed {
					assertCurrentMutationError(t, response, http.StatusForbidden,
						"tracing configurations are managed by project admins", "type")
					if mutator.calls != 0 {
						t.Fatalf("a refused tracing write reached the mutator %d times", mutator.calls)
					}
				} else if response.Code != write.success || mutator.calls != 1 {
					t.Fatalf("status = %d (want %d), mutator calls = %d, body = %s",
						response.Code, write.success, mutator.calls, response.Body.String())
				}
				if len(access.asked) != 1 || access.asked[0] != [2]int64{7, 11} {
					t.Fatalf("tracing access asked %v, want once about project 7 and user 11", access.asked)
				}
			})
		}
	}

	t.Run("another type is not asked about tracing", func(t *testing.T) {
		access := &tracingAccessStub{allowed: false}
		mutator := &currentConfigurationMutatorStub{}
		route := newTracingMutationRoute(t, mutator, reader, access)
		response := httptest.NewRecorder()
		route.ServeHTTP(response, currentMutationRequest(http.MethodDelete, "/api/v2/configurations/configuration/7/10", ``))
		if response.Code != http.StatusNoContent || mutator.calls != 1 || len(access.asked) != 0 {
			t.Fatalf("status = %d, mutator calls = %d, tracing asked %v", response.Code, mutator.calls, access.asked)
		}
	})

	t.Run("a failed access check refuses", func(t *testing.T) {
		access := &tracingAccessStub{allowed: true, err: errors.New("database unavailable")}
		mutator := &currentConfigurationMutatorStub{}
		route := newTracingMutationRoute(t, mutator, reader, access)
		response := httptest.NewRecorder()
		route.ServeHTTP(response, currentMutationRequest(http.MethodPut, "/api/v2/configurations/configuration/7/9", `{}`))
		assertCurrentMutationError(t, response, http.StatusForbidden,
			"tracing configurations are managed by project admins", "type")
		if mutator.calls != 0 {
			t.Fatalf("a write the access check could not decide reached the mutator")
		}
	})
}

// TestReviewedConfigurationReadByIDHidesATracingRow: the compatibility read
// by id answers a tracing row the caller does not manage as absent, and so
// does the reviewed one (#3032).
func TestReviewedConfigurationReadByIDHidesATracingRow(t *testing.T) {
	for _, allowed := range []bool{false, true} {
		t.Run(map[bool]string{false: "tracing not managed", true: "tracing managed"}[allowed], func(t *testing.T) {
			reader := &currentConfigurationReaderStub{
				get: func(_ context.Context, projectID, id int32) (configurationapp.CurrentConfiguration, error) {
					return configurationapp.CurrentConfiguration{ID: id, ProjectID: projectID, Type: "langfuse", Shared: true}, nil
				},
			}
			route, err := handler.NewCurrentConfigurationReadRoute(reader, 1, currentReadAuth(t),
				allowEveryCurrentMutationPermission(), testTracingRuleWith(t, &tracingAccessStub{allowed: allowed}))
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodGet, "/api/v2/configurations/configuration/7/9", nil)
			signInCurrentRead(request)
			response := httptest.NewRecorder()
			route.ServeHTTP(response, request)
			want := map[bool]int{false: http.StatusNotFound, true: http.StatusOK}[allowed]
			if response.Code != want {
				t.Fatalf("status = %d, want %d, body = %s", response.Code, want, response.Body.String())
			}
		})
	}
}

func newTracingMutationRoute(
	t *testing.T,
	mutator handler.CurrentConfigurationMutator,
	reader handler.CurrentConfigurationReader,
	access handler.TracingAccess,
) *handler.CurrentConfigurationMutationRoute {
	t.Helper()
	route, err := handler.NewCurrentConfigurationMutationRoute(
		mutator, reader, currentReadAuth(t), allowEveryCurrentMutationPermission(), testTracingRuleWith(t, access))
	if err != nil {
		t.Fatal(err)
	}
	return route
}

func currentReadAuth(t *testing.T) apimw.AuthConfig {
	t.Helper()
	return apimw.AuthConfig{
		PrincipalValidator: currentReadPrincipalValidatorFunc(func(_ context.Context, user auth.User) (auth.User, error) {
			return user, nil
		}),
		FormSessions: authtest.FormSessions{},
	}
}

func signInCurrentRead(request *http.Request) {
	authtest.SignIn(request, "11")
	request.RemoteAddr = "10.0.0.8:43120"
}
