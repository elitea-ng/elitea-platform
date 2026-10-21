package projectinfo_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware/authtest"
	handler "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/projectinfo"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

type currentProjectInfoReaderStub struct {
	result    handler.CurrentProjectInfo
	err       error
	projectID int32
	calls     int
}

func (stub *currentProjectInfoReaderStub) GetCurrentProjectInfo(
	_ context.Context,
	projectID int32,
) (handler.CurrentProjectInfo, error) {
	stub.calls++
	stub.projectID = projectID
	return stub.result, stub.err
}

func TestCurrentProjectInfoRoutePreservesExactCurrentContract(t *testing.T) {
	if handler.CurrentProjectInfoPath !=
		"/api/v2/elitea_core/project_info/prompt_lib/{projectID}/project-info" ||
		handler.CurrentProjectInfoMode != auth.PermissionModeDefault ||
		handler.CurrentProjectInfoPermission != "models.project_context.view" ||
		handler.CurrentProjectInfoTimeout.String() != "5s" {
		t.Fatalf(
			"current project-info contract drifted: path=%q mode=%q permission=%q timeout=%s",
			handler.CurrentProjectInfoPath,
			handler.CurrentProjectInfoMode,
			handler.CurrentProjectInfoPermission,
			handler.CurrentProjectInfoTimeout,
		)
	}

	reader := &currentProjectInfoReaderStub{
		result: handler.CurrentProjectInfo{
			TeammatesCount: 6,
			IconMeta: json.RawMessage(
				`{"url":"/api/v2/elitea_core/project_icon/prompt_lib/7/icon.svg","type":"image/svg+xml"}`,
			),
		},
	}
	permissionCalls := 0
	route := newCurrentProjectInfoRoute(
		t,
		reader,
		currentProjectInfoPrincipalValidatorFunc(
			func(_ context.Context, user auth.User) (auth.User, error) {
				return user, nil
			},
		),
		currentProjectInfoPermissionResolverFunc(
			func(
				_ context.Context,
				user auth.User,
				mode string,
				projectID string,
			) (auth.PermissionResolution, error) {
				permissionCalls++
				if user.UserID != "11" || mode != auth.PermissionModeDefault ||
					projectID != "7" {
					t.Fatalf(
						"permission input user=%+v mode=%q project=%q",
						user,
						mode,
						projectID,
					)
				}
				return auth.PermissionResolution{
					UserID:      11,
					Permissions: []string{handler.CurrentProjectInfoPermission},
				}, nil
			},
		),
	)

	response := httptest.NewRecorder()
	route.ServeHTTP(response, currentProjectInfoRequest(
		http.MethodGet,
		"/api/v2/elitea_core/project_info/prompt_lib/007/project-info",
		true,
		"10.0.0.8:43120",
		"11",
	))

	want := "{\"teammates_count\":6,\"icon_meta\":" +
		"{\"url\":\"/api/v2/elitea_core/project_icon/prompt_lib/7/icon.svg\"," +
		"\"type\":\"image/svg+xml\"}}\n"
	if response.Code != http.StatusOK || response.Body.String() != want ||
		reader.calls != 1 || reader.projectID != 7 || permissionCalls != 1 {
		t.Fatalf(
			"status=%d reader=%d project=%d permissions=%d body=%q",
			response.Code,
			reader.calls,
			reader.projectID,
			permissionCalls,
			response.Body.String(),
		)
	}
}

func TestCurrentProjectInfoRouteAuthenticatesAndAuthorizesBeforeRead(t *testing.T) {
	tests := []struct {
		name          string
		method        string
		path          string
		remoteAddr    string
		authenticated bool
		principalErr  error
		allowed       bool
		wantStatus    int
	}{
		{
			name:       "missing authentication",
			method:     http.MethodGet,
			path:       "/api/v2/elitea_core/project_info/prompt_lib/7/project-info",
			remoteAddr: "10.0.0.8:43120",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "forged X-Auth-* identity without a session",
			method:     http.MethodGet,
			path:       "/api/v2/elitea_core/project_info/prompt_lib/7/project-info",
			remoteAddr: "192.0.2.9:443",
			allowed:    true,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:          "suspended principal",
			method:        http.MethodGet,
			path:          "/api/v2/elitea_core/project_info/prompt_lib/7/project-info",
			remoteAddr:    "10.0.0.8:43120",
			authenticated: true,
			// A real suspension carries auth.ErrPrincipalInactive; only that
			// answer is a 401, because a database fault must not read as one
			// (#537).
			principalErr: auth.ErrPrincipalInactive,
			allowed:      true,
			wantStatus:   http.StatusUnauthorized,
		},
		{
			name:          "permission denied",
			method:        http.MethodGet,
			path:          "/api/v2/elitea_core/project_info/prompt_lib/7/project-info",
			remoteAddr:    "10.0.0.8:43120",
			authenticated: true,
			wantStatus:    http.StatusForbidden,
		},
		{
			name:          "invalid project",
			method:        http.MethodGet,
			path:          "/api/v2/elitea_core/project_info/prompt_lib/not-an-id/project-info",
			remoteAddr:    "10.0.0.8:43120",
			authenticated: true,
			allowed:       true,
			wantStatus:    http.StatusForbidden,
		},
		{
			name:          "wrong mode is not exposed",
			method:        http.MethodGet,
			path:          "/api/v2/elitea_core/project_info/default/7/project-info",
			remoteAddr:    "10.0.0.8:43120",
			authenticated: true,
			allowed:       true,
			wantStatus:    http.StatusNotFound,
		},
		{
			name:          "put is not exposed",
			method:        http.MethodPut,
			path:          "/api/v2/elitea_core/project_info/prompt_lib/7/project-info",
			remoteAddr:    "10.0.0.8:43120",
			authenticated: true,
			allowed:       true,
			wantStatus:    http.StatusMethodNotAllowed,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reader := &currentProjectInfoReaderStub{}
			principal := currentProjectInfoPrincipalValidatorFunc(
				func(_ context.Context, user auth.User) (auth.User, error) {
					return user, test.principalErr
				},
			)
			permissions := currentProjectInfoPermissionResolverFunc(
				func(
					context.Context,
					auth.User,
					string,
					string,
				) (auth.PermissionResolution, error) {
					resolution := auth.PermissionResolution{UserID: 11}
					if test.allowed {
						resolution.Permissions = []string{
							handler.CurrentProjectInfoPermission,
						}
					}
					return resolution, nil
				},
			)
			route := newCurrentProjectInfoRoute(t, reader, principal, permissions)
			response := httptest.NewRecorder()
			route.ServeHTTP(response, currentProjectInfoRequest(
				test.method,
				test.path,
				test.authenticated,
				test.remoteAddr,
				"11",
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

func TestCurrentProjectInfoRoutePreservesNullIconAndSafeGenericFailure(t *testing.T) {
	permissions := currentProjectInfoPermissionResolverFunc(
		func(
			context.Context,
			auth.User,
			string,
			string,
		) (auth.PermissionResolution, error) {
			return auth.PermissionResolution{
				UserID:      11,
				Permissions: []string{handler.CurrentProjectInfoPermission},
			}, nil
		},
	)
	principal := currentProjectInfoPrincipalValidatorFunc(
		func(_ context.Context, user auth.User) (auth.User, error) {
			return user, nil
		},
	)

	nullIcon := &currentProjectInfoReaderStub{
		result: handler.CurrentProjectInfo{TeammatesCount: 0},
	}
	route := newCurrentProjectInfoRoute(t, nullIcon, principal, permissions)
	response := httptest.NewRecorder()
	route.ServeHTTP(response, currentProjectInfoRequest(
		http.MethodGet,
		"/api/v2/elitea_core/project_info/prompt_lib/7/project-info",
		true,
		"10.0.0.8:43120",
		"11",
	))
	if response.Code != http.StatusOK ||
		response.Body.String() != "{\"teammates_count\":0,\"icon_meta\":null}\n" {
		t.Fatalf("null icon status=%d body=%q", response.Code, response.Body.String())
	}

	failure := &currentProjectInfoReaderStub{
		err: errors.New("pq: password=do-not-return"),
	}
	route = newCurrentProjectInfoRoute(t, failure, principal, permissions)
	response = httptest.NewRecorder()
	route.ServeHTTP(response, currentProjectInfoRequest(
		http.MethodGet,
		"/api/v2/elitea_core/project_info/prompt_lib/7/project-info",
		true,
		"10.0.0.8:43120",
		"11",
	))
	if response.Code != http.StatusInternalServerError ||
		response.Body.String() != "{\"error\":\"Failed to get project info\"}\n" ||
		strings.Contains(response.Body.String(), "password") {
		t.Fatalf("failure status=%d body=%q", response.Code, response.Body.String())
	}
}

func TestCurrentProjectInfoRouteRejectsIncompleteComposition(t *testing.T) {
	reader := &currentProjectInfoReaderStub{}
	principal := currentProjectInfoPrincipalValidatorFunc(
		func(_ context.Context, user auth.User) (auth.User, error) { return user, nil },
	)
	permissions := currentProjectInfoPermissionResolverFunc(
		func(
			context.Context,
			auth.User,
			string,
			string,
		) (auth.PermissionResolution, error) {
			return auth.PermissionResolution{}, nil
		},
	)
	authConfig := apimw.AuthConfig{
		PrincipalValidator: principal,
		FormSessions:       authtest.FormSessions{},
	}

	for name, test := range map[string]struct {
		reader      handler.CurrentProjectInfoReader
		authConfig  apimw.AuthConfig
		permissions auth.PermissionResolver
	}{
		"missing reader":      {authConfig: authConfig, permissions: permissions},
		"missing principal":   {reader: reader, authConfig: apimw.AuthConfig{FormSessions: authtest.FormSessions{}}, permissions: permissions},
		"missing peer proof":  {reader: reader, authConfig: apimw.AuthConfig{PrincipalValidator: principal}, permissions: permissions},
		"missing permissions": {reader: reader, authConfig: authConfig},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := handler.NewCurrentProjectInfoRoute(
				test.reader,
				test.authConfig,
				test.permissions,
			); !errors.Is(err, handler.ErrInvalidCurrentProjectInfoRoute) {
				t.Fatalf(
					"error=%v want=%v",
					err,
					handler.ErrInvalidCurrentProjectInfoRoute,
				)
			}
		})
	}

	var nilRoute *handler.CurrentProjectInfoRoute
	response := httptest.NewRecorder()
	nilRoute.ServeHTTP(
		response,
		httptest.NewRequest(
			http.MethodGet,
			"/api/v2/elitea_core/project_info/prompt_lib/7/project-info",
			nil,
		),
	)
	if response.Code != http.StatusNotFound {
		t.Fatalf("nil route status=%d", response.Code)
	}
}

func TestCurrentProjectInfoRepositoryRejectsInvalidDependenciesAndRequest(t *testing.T) {
	if _, err := handler.NewCurrentProjectInfoRepository(nil); err == nil {
		t.Fatal("nil database was accepted")
	}

	var repository *handler.CurrentProjectInfoRepository
	if _, err := repository.GetCurrentProjectInfo(
		context.Background(),
		1,
	); !errors.Is(err, handler.ErrInvalidCurrentProjectInfoRequest) {
		t.Fatalf(
			"nil repository error=%v want=%v",
			err,
			handler.ErrInvalidCurrentProjectInfoRequest,
		)
	}
}

func newCurrentProjectInfoRoute(
	t *testing.T,
	reader handler.CurrentProjectInfoReader,
	principal apimw.PrincipalValidator,
	permissions auth.PermissionResolver,
) *handler.CurrentProjectInfoRoute {
	t.Helper()
	route, err := handler.NewCurrentProjectInfoRoute(
		reader,
		apimw.AuthConfig{
			PrincipalValidator: principal,
			FormSessions:       authtest.FormSessions{},
		},
		permissions,
	)
	if err != nil {
		t.Fatal(err)
	}
	return route
}

func currentProjectInfoRequest(
	method string,
	path string,
	authenticated bool,
	remoteAddr string,
	userID string,
) *http.Request {
	request := httptest.NewRequest(method, path, nil)
	request.RemoteAddr = remoteAddr
	if authenticated {
		authtest.SignIn(request, userID)
	} else {
		// No credential, but the forged X-Auth-* identity the retired edge
		// projection used: it must authenticate nobody (#1359).
		authtest.ForgeXAuth(request, userID)
	}
	return request
}

type currentProjectInfoPermissionResolverFunc func(
	context.Context,
	auth.User,
	string,
	string,
) (auth.PermissionResolution, error)

func (function currentProjectInfoPermissionResolverFunc) ResolvePermissions(
	ctx context.Context,
	user auth.User,
	mode string,
	projectID string,
) (auth.PermissionResolution, error) {
	return function(ctx, user, mode, projectID)
}

type currentProjectInfoPrincipalValidatorFunc func(
	context.Context,
	auth.User,
) (auth.User, error)

func (function currentProjectInfoPrincipalValidatorFunc) ValidatePrincipal(
	ctx context.Context,
	user auth.User,
) (auth.User, error) {
	return function(ctx, user)
}
