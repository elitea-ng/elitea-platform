package configurations_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	handler "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/configurations"
	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

type currentConfigurationTypesReaderStub struct {
	list  func(context.Context, configurationapp.CurrentConfigurationTypesQuery) (configurationapp.CurrentConfigurationTypesResult, error)
	calls int
}

func (stub *currentConfigurationTypesReaderStub) List(
	ctx context.Context,
	query configurationapp.CurrentConfigurationTypesQuery,
) (configurationapp.CurrentConfigurationTypesResult, error) {
	stub.calls++
	if stub.list == nil {
		return configurationapp.CurrentConfigurationTypesResult{}, nil
	}
	return stub.list(ctx, query)
}

func TestCurrentConfigurationTypesRouteBindsExactCurrentContract(t *testing.T) {
	if handler.CurrentConfigurationTypesPath != "/api/v2/configurations/types/{projectID}" ||
		handler.CurrentConfigurationTypesDefaultSection != "credentials" ||
		handler.CurrentConfigurationTypesMode != auth.PermissionModeDefault ||
		handler.CurrentConfigurationTypesPermission != "configurations.configurations.list" {
		t.Fatalf(
			"types route drifted: path=%q default=%q mode=%q permission=%q",
			handler.CurrentConfigurationTypesPath,
			handler.CurrentConfigurationTypesDefaultSection,
			handler.CurrentConfigurationTypesMode,
			handler.CurrentConfigurationTypesPermission,
		)
	}

	reader := &currentConfigurationTypesReaderStub{}
	principal := currentReadPrincipalValidatorFunc(func(_ context.Context, user auth.User) (auth.User, error) { return user, nil })
	peer := currentReadPeerVerifierFunc(func(*http.Request) error { return nil })
	permissions := permissionResolverFunc(func(context.Context, auth.User, string, string) (auth.PermissionResolution, error) {
		return auth.PermissionResolution{}, nil
	})
	authConfig := apimw.AuthConfig{PrincipalValidator: principal, ForwardedIdentityVerifier: peer}

	for name, test := range map[string]struct {
		reader      handler.CurrentConfigurationTypesReader
		authConfig  apimw.AuthConfig
		permissions auth.PermissionResolver
	}{
		"missing reader":       {authConfig: authConfig, permissions: permissions},
		"missing principal":    {reader: reader, authConfig: apimw.AuthConfig{ForwardedIdentityVerifier: peer}, permissions: permissions},
		"no credential reader": {reader: reader, authConfig: apimw.AuthConfig{PrincipalValidator: principal}, permissions: permissions},
		"missing permissions":  {reader: reader, authConfig: authConfig},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := handler.NewCurrentConfigurationTypesRoute(
				test.reader,
				test.authConfig,
				test.permissions,
			); !errors.Is(err, handler.ErrInvalidCurrentConfigurationTypesRoute) {
				t.Fatalf("error=%v, want %v", err, handler.ErrInvalidCurrentConfigurationTypesRoute)
			}
		})
	}
}

func TestCurrentConfigurationTypesRoutePreservesSectionSelectionAndExactDTO(t *testing.T) {
	tests := []struct {
		name        string
		path        string
		wantSection string
	}{
		{name: "absent defaults to credentials", path: "/api/v2/configurations/types/007", wantSection: "credentials"},
		{name: "explicit empty means all", path: "/api/v2/configurations/types/007?section=", wantSection: ""},
		{name: "explicit section is unchanged", path: "/api/v2/configurations/types/007?section=AI_credentials", wantSection: "AI_credentials"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reader := &currentConfigurationTypesReaderStub{
				list: func(_ context.Context, query configurationapp.CurrentConfigurationTypesQuery) (configurationapp.CurrentConfigurationTypesResult, error) {
					if query.ProjectID != 7 || query.Section != test.wantSection {
						t.Fatalf("query=%+v, want project 7 section %q", query, test.wantSection)
					}
					return configurationapp.CurrentConfigurationTypesResult{
						Rows: []string{"github", "gitlab"}, Total: 2,
					}, nil
				},
			}
			permissionCalls := 0
			permissions := permissionResolverFunc(func(_ context.Context, user auth.User, mode, projectID string) (auth.PermissionResolution, error) {
				permissionCalls++
				if user.UserID != "11" || mode != auth.PermissionModeDefault || projectID != "7" {
					t.Fatalf("permission input user=%+v mode=%q project=%q", user, mode, projectID)
				}
				return auth.PermissionResolution{UserID: 11, Permissions: []string{handler.CurrentConfigurationTypesPermission}}, nil
			})
			route := newCurrentConfigurationTypesRoute(t, reader, permissions)

			response := httptest.NewRecorder()
			route.ServeHTTP(response, currentReadRequest(http.MethodGet, test.path, "10.0.0.8:43120"))

			if response.Code != http.StatusOK || reader.calls != 1 || permissionCalls != 1 {
				t.Fatalf("status=%d reader_calls=%d permission_calls=%d body=%s", response.Code, reader.calls, permissionCalls, response.Body.String())
			}
			if response.Body.String() != "{\"rows\":[\"github\",\"gitlab\"],\"total\":2}\n" {
				t.Fatalf("exact response=%q", response.Body.String())
			}
		})
	}
}

func TestCurrentConfigurationTypesRouteAuthenticatesAndAuthorizesBeforeService(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		path       string
		remoteAddr string
		auth       bool
		allowed    bool
		wantStatus int
	}{
		{name: "missing authentication", method: http.MethodGet, path: "/api/v2/configurations/types/7", remoteAddr: "10.0.0.8:43120", wantStatus: http.StatusUnauthorized},
		{name: "untrusted forwarded identity", method: http.MethodGet, path: "/api/v2/configurations/types/7", remoteAddr: "192.0.2.9:443", auth: true, allowed: true, wantStatus: http.StatusUnauthorized},
		{name: "permission denied", method: http.MethodGet, path: "/api/v2/configurations/types/7", remoteAddr: "10.0.0.8:43120", auth: true, wantStatus: http.StatusForbidden},
		{name: "invalid project", method: http.MethodGet, path: "/api/v2/configurations/types/not-an-id", remoteAddr: "10.0.0.8:43120", auth: true, allowed: true, wantStatus: http.StatusForbidden},
		{name: "post is not exposed", method: http.MethodPost, path: "/api/v2/configurations/types/7", remoteAddr: "10.0.0.8:43120", auth: true, allowed: true, wantStatus: http.StatusMethodNotAllowed},
		{name: "extra path is not exposed", method: http.MethodGet, path: "/api/v2/configurations/types/7/extra", remoteAddr: "10.0.0.8:43120", auth: true, allowed: true, wantStatus: http.StatusNotFound},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reader := &currentConfigurationTypesReaderStub{}
			permissions := permissionResolverFunc(func(context.Context, auth.User, string, string) (auth.PermissionResolution, error) {
				resolved := auth.PermissionResolution{UserID: 11}
				if test.allowed {
					resolved.Permissions = []string{handler.CurrentConfigurationTypesPermission}
				}
				return resolved, nil
			})
			route := newCurrentConfigurationTypesRoute(t, reader, permissions)
			request := httptest.NewRequest(test.method, test.path, nil)
			request.RemoteAddr = test.remoteAddr
			if test.auth {
				request.Header.Set("X-Auth-Type", "user")
				request.Header.Set("X-Auth-ID", "11")
			}
			response := httptest.NewRecorder()
			route.ServeHTTP(response, request)
			if response.Code != test.wantStatus || reader.calls != 0 {
				t.Fatalf("status=%d want=%d service_calls=%d body=%s", response.Code, test.wantStatus, reader.calls, response.Body.String())
			}
		})
	}
}

func TestCurrentConfigurationTypesRouteUsesSafeServiceErrors(t *testing.T) {
	sensitiveFailure := errors.New("pq: password=do-not-return")
	for name, test := range map[string]struct {
		err        error
		wantStatus int
		wantBody   string
	}{
		"invalid input": {
			err:        configurationapp.ErrInvalidCurrentConfigurationTypesRequest,
			wantStatus: http.StatusBadRequest,
			wantBody:   "{\"error\":\"invalid configuration types request\"}\n",
		},
		"dependency failure": {
			err:        sensitiveFailure,
			wantStatus: http.StatusInternalServerError,
			wantBody:   "{\"error\":\"internal server error\"}\n",
		},
	} {
		t.Run(name, func(t *testing.T) {
			reader := &currentConfigurationTypesReaderStub{
				list: func(context.Context, configurationapp.CurrentConfigurationTypesQuery) (configurationapp.CurrentConfigurationTypesResult, error) {
					return configurationapp.CurrentConfigurationTypesResult{}, test.err
				},
			}
			permissions := permissionResolverFunc(func(context.Context, auth.User, string, string) (auth.PermissionResolution, error) {
				return auth.PermissionResolution{UserID: 11, Permissions: []string{handler.CurrentConfigurationTypesPermission}}, nil
			})
			route := newCurrentConfigurationTypesRoute(t, reader, permissions)
			response := httptest.NewRecorder()
			route.ServeHTTP(response, currentReadRequest(http.MethodGet, "/api/v2/configurations/types/7", "10.0.0.8:43120"))
			if response.Code != test.wantStatus || response.Body.String() != test.wantBody ||
				strings.Contains(response.Body.String(), "password") {
				t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
			}
		})
	}
}

func newCurrentConfigurationTypesRoute(
	t *testing.T,
	reader handler.CurrentConfigurationTypesReader,
	permissions auth.PermissionResolver,
) *handler.CurrentConfigurationTypesRoute {
	t.Helper()
	principal := currentReadPrincipalValidatorFunc(func(_ context.Context, user auth.User) (auth.User, error) {
		return user, nil
	})
	peer := currentReadPeerVerifierFunc(func(request *http.Request) error {
		if request.RemoteAddr != "10.0.0.8:43120" {
			return errors.New("untrusted peer")
		}
		return nil
	})
	route, err := handler.NewCurrentConfigurationTypesRoute(
		reader,
		apimw.AuthConfig{PrincipalValidator: principal, ForwardedIdentityVerifier: peer},
		permissions,
	)
	if err != nil {
		t.Fatal(err)
	}
	return route
}
