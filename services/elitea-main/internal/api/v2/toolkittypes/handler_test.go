package toolkittypes_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apimw "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware/authtest"
	handler "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/toolkittypes"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

type currentToolkitTypesReaderStub struct {
	rows              []string
	err               error
	projectID         int32
	filterMCP         bool
	filterApplication bool
	calls             int
}

func (stub *currentToolkitTypesReaderStub) ListCurrentToolkitTypes(
	_ context.Context,
	projectID int32,
	filterMCP bool,
	filterApplication bool,
) ([]string, error) {
	stub.calls++
	stub.projectID = projectID
	stub.filterMCP = filterMCP
	stub.filterApplication = filterApplication
	return stub.rows, stub.err
}

func TestCurrentToolkitTypesRouteBindsExactCurrentContract(t *testing.T) {
	if handler.CurrentToolkitTypesPath != "/api/v2/elitea_core/toolkit_types/prompt_lib/{projectID}" ||
		handler.CurrentToolkitTypesMode != auth.PermissionModeDefault ||
		handler.CurrentToolkitTypesPermission != "models.applications.tools.list" {
		t.Fatalf(
			"current toolkit types contract drifted: path=%q mode=%q permission=%q",
			handler.CurrentToolkitTypesPath,
			handler.CurrentToolkitTypesMode,
			handler.CurrentToolkitTypesPermission,
		)
	}

	reader := &currentToolkitTypesReaderStub{}
	principal := currentToolkitTypesPrincipalValidatorFunc(
		func(_ context.Context, user auth.User) (auth.User, error) { return user, nil },
	)
	permissions := currentToolkitTypesPermissionResolverFunc(
		func(context.Context, auth.User, string, string) (auth.PermissionResolution, error) {
			return auth.PermissionResolution{}, nil
		},
	)
	authConfig := apimw.AuthConfig{
		PrincipalValidator: principal,
		FormSessions:       authtest.FormSessions{},
	}

	for name, test := range map[string]struct {
		reader      handler.CurrentToolkitTypesReader
		authConfig  apimw.AuthConfig
		permissions auth.PermissionResolver
	}{
		"missing reader":      {authConfig: authConfig, permissions: permissions},
		"missing principal":   {reader: reader, authConfig: apimw.AuthConfig{FormSessions: authtest.FormSessions{}}, permissions: permissions},
		"missing peer proof":  {reader: reader, authConfig: apimw.AuthConfig{PrincipalValidator: principal}, permissions: permissions},
		"missing permissions": {reader: reader, authConfig: authConfig},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := handler.NewCurrentToolkitTypesRoute(
				test.reader,
				test.authConfig,
				test.permissions,
			); !errors.Is(err, handler.ErrInvalidCurrentToolkitTypesRoute) {
				t.Fatalf("error=%v, want %v", err, handler.ErrInvalidCurrentToolkitTypesRoute)
			}
		})
	}
}

func TestCurrentToolkitTypesRoutePreservesIndependentFiltersAndExactDTO(t *testing.T) {
	tests := []struct {
		name            string
		query           string
		wantMCP         bool
		wantApplication bool
	}{
		{name: "both absent", query: "", wantMCP: false, wantApplication: false},
		{name: "MCP only", query: "?mcp=TRUE&application=false", wantMCP: true, wantApplication: false},
		{name: "application only", query: "?mcp=false&application=TrUe", wantMCP: false, wantApplication: true},
		{name: "both selected", query: "?mcp=true&application=true", wantMCP: true, wantApplication: true},
		{name: "only literal true selects", query: "?mcp=%20true%20&application=1", wantMCP: false, wantApplication: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reader := &currentToolkitTypesReaderStub{rows: []string{"ado_repos", "github"}}
			permissionCalls := 0
			permissions := currentToolkitTypesPermissionResolverFunc(
				func(_ context.Context, user auth.User, mode, projectID string) (auth.PermissionResolution, error) {
					permissionCalls++
					if user.UserID != "11" || mode != auth.PermissionModeDefault || projectID != "7" {
						t.Fatalf("permission input user=%+v mode=%q project=%q", user, mode, projectID)
					}
					return auth.PermissionResolution{
						UserID:      11,
						Permissions: []string{handler.CurrentToolkitTypesPermission},
					}, nil
				},
			)
			route := newCurrentToolkitTypesRoute(t, reader, permissions)

			response := httptest.NewRecorder()
			route.ServeHTTP(response, currentToolkitTypesRequest(
				http.MethodGet,
				"/api/v2/elitea_core/toolkit_types/prompt_lib/007"+test.query,
				true,
				"10.0.0.8:43120",
			))

			if response.Code != http.StatusOK || reader.calls != 1 || permissionCalls != 1 {
				t.Fatalf(
					"status=%d reader_calls=%d permission_calls=%d body=%s",
					response.Code,
					reader.calls,
					permissionCalls,
					response.Body.String(),
				)
			}
			if reader.projectID != 7 || reader.filterMCP != test.wantMCP ||
				reader.filterApplication != test.wantApplication {
				t.Fatalf(
					"reader project=%d mcp=%t application=%t",
					reader.projectID,
					reader.filterMCP,
					reader.filterApplication,
				)
			}
			if response.Body.String() != "{\"rows\":[\"ado_repos\",\"github\"],\"total\":2}\n" {
				t.Fatalf("exact response=%q", response.Body.String())
			}
		})
	}
}

func TestCurrentToolkitTypesRouteAuthenticatesAndAuthorizesBeforeTenantRead(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		path       string
		remoteAddr string
		auth       bool
		allowed    bool
		wantStatus int
	}{
		{
			name:       "missing authentication",
			method:     http.MethodGet,
			path:       "/api/v2/elitea_core/toolkit_types/prompt_lib/7",
			remoteAddr: "10.0.0.8:43120",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "forged X-Auth-* identity without a session",
			method:     http.MethodGet,
			path:       "/api/v2/elitea_core/toolkit_types/prompt_lib/7",
			remoteAddr: "192.0.2.9:443",
			allowed:    true,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "permission denied",
			method:     http.MethodGet,
			path:       "/api/v2/elitea_core/toolkit_types/prompt_lib/7",
			remoteAddr: "10.0.0.8:43120",
			auth:       true,
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "invalid project",
			method:     http.MethodGet,
			path:       "/api/v2/elitea_core/toolkit_types/prompt_lib/not-an-id",
			remoteAddr: "10.0.0.8:43120",
			auth:       true,
			allowed:    true,
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "post is not exposed",
			method:     http.MethodPost,
			path:       "/api/v2/elitea_core/toolkit_types/prompt_lib/7",
			remoteAddr: "10.0.0.8:43120",
			auth:       true,
			allowed:    true,
			wantStatus: http.StatusMethodNotAllowed,
		},
		{
			name:       "extra path is not exposed",
			method:     http.MethodGet,
			path:       "/api/v2/elitea_core/toolkit_types/prompt_lib/7/extra",
			remoteAddr: "10.0.0.8:43120",
			auth:       true,
			allowed:    true,
			wantStatus: http.StatusNotFound,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reader := &currentToolkitTypesReaderStub{}
			permissions := currentToolkitTypesPermissionResolverFunc(
				func(context.Context, auth.User, string, string) (auth.PermissionResolution, error) {
					resolution := auth.PermissionResolution{UserID: 11}
					if test.allowed {
						resolution.Permissions = []string{handler.CurrentToolkitTypesPermission}
					}
					return resolution, nil
				},
			)
			route := newCurrentToolkitTypesRoute(t, reader, permissions)
			response := httptest.NewRecorder()
			route.ServeHTTP(response, currentToolkitTypesRequest(
				test.method,
				test.path,
				test.auth,
				test.remoteAddr,
			))
			if response.Code != test.wantStatus || reader.calls != 0 {
				t.Fatalf(
					"status=%d want=%d reader_calls=%d body=%s",
					response.Code,
					test.wantStatus,
					reader.calls,
					response.Body.String(),
				)
			}
		})
	}
}

func TestCurrentToolkitTypesRoutePreservesEmptyRowsAndSafeGenericFailure(t *testing.T) {
	permissions := currentToolkitTypesPermissionResolverFunc(
		func(context.Context, auth.User, string, string) (auth.PermissionResolution, error) {
			return auth.PermissionResolution{
				UserID:      11,
				Permissions: []string{handler.CurrentToolkitTypesPermission},
			}, nil
		},
	)

	empty := &currentToolkitTypesReaderStub{}
	route := newCurrentToolkitTypesRoute(t, empty, permissions)
	response := httptest.NewRecorder()
	route.ServeHTTP(response, currentToolkitTypesRequest(
		http.MethodGet,
		"/api/v2/elitea_core/toolkit_types/prompt_lib/7",
		true,
		"10.0.0.8:43120",
	))
	if response.Code != http.StatusOK || response.Body.String() != "{\"rows\":[],\"total\":0}\n" {
		t.Fatalf("empty response status=%d body=%q", response.Code, response.Body.String())
	}

	failure := &currentToolkitTypesReaderStub{err: errors.New("pq: password=do-not-return")}
	route = newCurrentToolkitTypesRoute(t, failure, permissions)
	response = httptest.NewRecorder()
	route.ServeHTTP(response, currentToolkitTypesRequest(
		http.MethodGet,
		"/api/v2/elitea_core/toolkit_types/prompt_lib/7",
		true,
		"10.0.0.8:43120",
	))
	if response.Code != http.StatusBadRequest ||
		response.Body.String() != "{\"ok\":false,\"error\":\"Failed to list toolkit types\"}\n" ||
		strings.Contains(response.Body.String(), "password") {
		t.Fatalf("failure response status=%d body=%q", response.Code, response.Body.String())
	}
}

func newCurrentToolkitTypesRoute(
	t *testing.T,
	reader handler.CurrentToolkitTypesReader,
	permissions auth.PermissionResolver,
) *handler.CurrentToolkitTypesRoute {
	t.Helper()
	route, err := handler.NewCurrentToolkitTypesRoute(
		reader,
		apimw.AuthConfig{
			PrincipalValidator: currentToolkitTypesPrincipalValidatorFunc(
				func(_ context.Context, user auth.User) (auth.User, error) {
					return user, nil
				},
			),
			FormSessions: authtest.FormSessions{},
		},
		permissions,
	)
	if err != nil {
		t.Fatal(err)
	}
	return route
}

func currentToolkitTypesRequest(
	method string,
	path string,
	authenticated bool,
	remoteAddr string,
) *http.Request {
	request := httptest.NewRequest(method, path, nil)
	request.RemoteAddr = remoteAddr
	if authenticated {
		authtest.SignIn(request, "11")
	} else {
		// No credential, but the forged X-Auth-* identity the retired edge
		// projection used: it must authenticate nobody (#1359).
		authtest.ForgeXAuth(request, "11")
	}
	return request
}

type currentToolkitTypesPermissionResolverFunc func(
	context.Context,
	auth.User,
	string,
	string,
) (auth.PermissionResolution, error)

func (function currentToolkitTypesPermissionResolverFunc) ResolvePermissions(
	ctx context.Context,
	user auth.User,
	mode string,
	projectID string,
) (auth.PermissionResolution, error) {
	return function(ctx, user, mode, projectID)
}

type currentToolkitTypesPrincipalValidatorFunc func(context.Context, auth.User) (auth.User, error)

func (function currentToolkitTypesPrincipalValidatorFunc) ValidatePrincipal(
	ctx context.Context,
	user auth.User,
) (auth.User, error) {
	return function(ctx, user)
}
