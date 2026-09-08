package configurations_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	handler "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/configurations"
	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

type currentConfigurationReaderStub struct {
	list  func(context.Context, configurationapp.CurrentConfigurationListRequest) (configurationapp.CurrentConfigurationListResult, error)
	get   func(context.Context, int32, int32) (configurationapp.CurrentConfiguration, error)
	calls int
}

func (stub *currentConfigurationReaderStub) List(
	ctx context.Context,
	request configurationapp.CurrentConfigurationListRequest,
) (configurationapp.CurrentConfigurationListResult, error) {
	stub.calls++
	if stub.list == nil {
		return configurationapp.CurrentConfigurationListResult{}, nil
	}
	return stub.list(ctx, request)
}

func (stub *currentConfigurationReaderStub) Get(
	ctx context.Context,
	projectID int32,
	configurationID int32,
) (configurationapp.CurrentConfiguration, error) {
	stub.calls++
	if stub.get == nil {
		return configurationapp.CurrentConfiguration{}, nil
	}
	return stub.get(ctx, projectID, configurationID)
}

type currentReadPrincipalValidatorFunc func(context.Context, auth.User) (auth.User, error)

func (function currentReadPrincipalValidatorFunc) ValidatePrincipal(ctx context.Context, user auth.User) (auth.User, error) {
	return function(ctx, user)
}

type currentReadPeerVerifierFunc func(*http.Request) error

func (function currentReadPeerVerifierFunc) VerifyForwardedIdentityPeer(request *http.Request) error {
	return function(request)
}

func TestCurrentConfigurationReadRouteBindsExactCurrentPathsAndPermissions(t *testing.T) {
	if handler.CurrentConfigurationListPath != "/api/v2/configurations/configurations/{projectID}" ||
		handler.CurrentConfigurationDetailsPath != "/api/v2/configurations/configuration/{projectID}/{configID}" ||
		handler.CurrentConfigurationReadMode != auth.PermissionModeDefault ||
		handler.CurrentConfigurationListPermission != "configurations.configurations.list" ||
		handler.CurrentConfigurationGetPermission != "configurations.configuration.details" {
		t.Fatalf(
			"current contract drifted: list=%q details=%q mode=%q permissions=%q,%q",
			handler.CurrentConfigurationListPath,
			handler.CurrentConfigurationDetailsPath,
			handler.CurrentConfigurationReadMode,
			handler.CurrentConfigurationListPermission,
			handler.CurrentConfigurationGetPermission,
		)
	}

	reader := &currentConfigurationReaderStub{}
	principal := currentReadPrincipalValidatorFunc(func(_ context.Context, user auth.User) (auth.User, error) {
		return user, nil
	})
	peer := currentReadPeerVerifierFunc(func(*http.Request) error { return nil })
	permissions := permissionResolverFunc(func(context.Context, auth.User, string, string) (auth.PermissionResolution, error) {
		return auth.PermissionResolution{}, nil
	})
	authConfig := apimw.AuthConfig{PrincipalValidator: principal, ForwardedIdentityVerifier: peer}

	for name, test := range map[string]struct {
		reader          handler.CurrentConfigurationReader
		publicProjectID int32
		authConfig      apimw.AuthConfig
		permissions     auth.PermissionResolver
	}{
		"missing reader":         {publicProjectID: 1, authConfig: authConfig, permissions: permissions},
		"missing public project": {reader: reader, authConfig: authConfig, permissions: permissions},
		"missing principal": {
			reader: reader, publicProjectID: 1,
			authConfig: apimw.AuthConfig{ForwardedIdentityVerifier: peer}, permissions: permissions,
		},
		"no credential reader": {
			reader: reader, publicProjectID: 1,
			authConfig: apimw.AuthConfig{PrincipalValidator: principal}, permissions: permissions,
		},
		"missing permissions": {reader: reader, publicProjectID: 1, authConfig: authConfig},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := handler.NewCurrentConfigurationReadRoute(
				test.reader,
				test.publicProjectID,
				test.authConfig,
				test.permissions,
			); !errors.Is(err, handler.ErrInvalidCurrentConfigurationReadRoute) {
				t.Fatalf("error = %v, want %v", err, handler.ErrInvalidCurrentConfigurationReadRoute)
			}
		})
	}
}

func TestCurrentConfigurationListRoutePreservesQueryAndResponseContract(t *testing.T) {
	createdAt := time.Date(2026, time.July, 22, 12, 0, 0, 0, time.UTC)
	options := map[string]any{}
	reader := &currentConfigurationReaderStub{
		list: func(_ context.Context, request configurationapp.CurrentConfigurationListRequest) (configurationapp.CurrentConfigurationListResult, error) {
			if request.ProjectID != 7 || request.PublicProjectID != 1 ||
				request.Offset != 0 || request.Limit != 25 ||
				!request.IncludeShared || request.SharedOffset != -3 || request.SharedLimit != 0 ||
				request.Query != "docs" || request.SortBy != "label" || request.SortOrder != "asc" {
				t.Fatalf("unexpected list request: %+v", request)
			}
			if strings.Join(request.Types, ",") != "github,confluence" ||
				strings.Join(request.Sections, ",") != "integration,toolkit" {
				t.Fatalf("filters were not preserved: types=%v sections=%v", request.Types, request.Sections)
			}
			return configurationapp.CurrentConfigurationListResult{
				CurrentConfigurationPage: configurationapp.CurrentConfigurationPage{
					Total: 1, Offset: 0, Limit: 25,
					Items: []configurationapp.CurrentConfiguration{{
						ID: 9, UUID: "00000000-0000-4000-8000-000000000009", ProjectID: 7,
						EliteaTitle: "team_docs", Type: "confluence", Section: "integration",
						Data: map[string]any{"url": "https://example.invalid"}, Meta: map[string]any{},
						Source: "user", CreatedAt: createdAt, Options: &options,
					}},
				},
				Shared: &configurationapp.CurrentConfigurationPage{Items: []configurationapp.CurrentConfiguration{}, Limit: 20},
			}, nil
		},
	}
	permissionCalls := 0
	permissions := permissionResolverFunc(func(_ context.Context, user auth.User, mode, projectID string) (auth.PermissionResolution, error) {
		permissionCalls++
		if user.UserID != "11" || mode != auth.PermissionModeDefault || projectID != "7" {
			t.Fatalf("permission input user=%+v mode=%q project=%q", user, mode, projectID)
		}
		return auth.PermissionResolution{UserID: 11, Permissions: []string{handler.CurrentConfigurationListPermission}}, nil
	})
	route := newCurrentConfigurationReadRoute(t, reader, permissions)

	request := currentReadRequest(
		http.MethodGet,
		"/api/v2/configurations/configurations/007?type=github&type=confluence&section=integration&section=toolkit&offset=not-an-int&limit=25&include_shared=TrUe&shared_offset=-3&shared_limit=bad&query=docs&sort_by=label&sort_order=asc",
		"10.0.0.8:43120",
	)
	response := httptest.NewRecorder()
	route.ServeHTTP(response, request)

	if response.Code != http.StatusOK || reader.calls != 1 || permissionCalls != 1 {
		t.Fatalf("status=%d reader_calls=%d permission_calls=%d body=%s", response.Code, reader.calls, permissionCalls, response.Body.String())
	}
	for _, fragment := range []string{
		`"total":1`, `"offset":0`, `"limit":25`, `"items":[{`,
		`"elitea_title":"team_docs"`, `"data":{"url":"https://example.invalid"}`,
		`"is_pinned":false`, `"options":{}`, `"shared":{"total":0,"items":[]`,
	} {
		if !strings.Contains(response.Body.String(), fragment) {
			t.Fatalf("response %s does not contain %s", response.Body.String(), fragment)
		}
	}
	if strings.Contains(response.Body.String(), `"name"`) {
		t.Fatalf("prototype field leaked into current response: %s", response.Body.String())
	}
}

func TestCurrentConfigurationDetailsRouteMapsNotFoundAndExactDTO(t *testing.T) {
	createdAt := time.Date(2026, time.July, 22, 12, 0, 0, 0, time.UTC)
	options := map[string]any{"embedding_model": []any{}}
	reader := &currentConfigurationReaderStub{
		get: func(_ context.Context, projectID, configurationID int32) (configurationapp.CurrentConfiguration, error) {
			if projectID != 7 {
				t.Fatalf("project id = %d, want 7", projectID)
			}
			if configurationID == 404 {
				return configurationapp.CurrentConfiguration{}, configurationapp.ErrCurrentConfigurationNotFound
			}
			if configurationID != 9 {
				t.Fatalf("configuration id = %d, want 9", configurationID)
			}
			return configurationapp.CurrentConfiguration{
				ID: 9, UUID: "00000000-0000-4000-8000-000000000009", ProjectID: 7,
				EliteaTitle: "team_docs", Type: "confluence", Section: "integration",
				Data: map[string]any{}, Meta: map[string]any{}, Source: "user",
				CreatedAt: createdAt, Options: &options,
			}, nil
		},
	}
	permissions := permissionResolverFunc(func(_ context.Context, _ auth.User, mode, projectID string) (auth.PermissionResolution, error) {
		if mode != auth.PermissionModeDefault || projectID != "7" {
			t.Fatalf("permission mode=%q project=%q", mode, projectID)
		}
		return auth.PermissionResolution{UserID: 11, Permissions: []string{handler.CurrentConfigurationGetPermission}}, nil
	})
	route := newCurrentConfigurationReadRoute(t, reader, permissions)

	response := httptest.NewRecorder()
	route.ServeHTTP(response, currentReadRequest(
		http.MethodGet,
		"/api/v2/configurations/configuration/7/9",
		"10.0.0.8:43120",
	))
	if response.Code != http.StatusOK ||
		!strings.Contains(response.Body.String(), `"elitea_title":"team_docs"`) ||
		!strings.Contains(response.Body.String(), `"options":{"embedding_model":[]}`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}

	notFound := httptest.NewRecorder()
	route.ServeHTTP(notFound, currentReadRequest(
		http.MethodGet,
		"/api/v2/configurations/configuration/7/404",
		"10.0.0.8:43120",
	))
	if notFound.Code != http.StatusNotFound || notFound.Body.String() != "{\"error\":\"Configuration not found\"}\n" {
		t.Fatalf("status=%d body=%q", notFound.Code, notFound.Body.String())
	}
}

func TestCurrentConfigurationReadRouteRejectsBeforeServiceAndOtherMethods(t *testing.T) {
	reader := &currentConfigurationReaderStub{}
	permissions := permissionResolverFunc(func(context.Context, auth.User, string, string) (auth.PermissionResolution, error) {
		return auth.PermissionResolution{UserID: 11}, nil
	})
	route := newCurrentConfigurationReadRoute(t, reader, permissions)

	for name, test := range map[string]struct {
		method     string
		path       string
		remoteAddr string
		wantStatus int
	}{
		"untrusted peer": {
			method: http.MethodGet, path: "/api/v2/configurations/configurations/7",
			remoteAddr: "192.0.2.9:443", wantStatus: http.StatusUnauthorized,
		},
		"permission denied": {
			method: http.MethodGet, path: "/api/v2/configurations/configurations/7",
			remoteAddr: "10.0.0.8:43120", wantStatus: http.StatusForbidden,
		},
		"invalid project before resolver": {
			method: http.MethodGet, path: "/api/v2/configurations/configurations/not-an-id",
			remoteAddr: "10.0.0.8:43120", wantStatus: http.StatusForbidden,
		},
		"post is not exposed": {
			method: http.MethodPost, path: "/api/v2/configurations/configurations/7",
			remoteAddr: "10.0.0.8:43120", wantStatus: http.StatusMethodNotAllowed,
		},
	} {
		t.Run(name, func(t *testing.T) {
			response := httptest.NewRecorder()
			route.ServeHTTP(response, currentReadRequest(test.method, test.path, test.remoteAddr))
			if response.Code != test.wantStatus {
				t.Fatalf("status=%d want=%d body=%s", response.Code, test.wantStatus, response.Body.String())
			}
		})
	}
	if reader.calls != 0 {
		t.Fatalf("denied requests reached service %d times", reader.calls)
	}
}

func newCurrentConfigurationReadRoute(
	t *testing.T,
	reader handler.CurrentConfigurationReader,
	permissions auth.PermissionResolver,
) *handler.CurrentConfigurationReadRoute {
	t.Helper()
	principal := currentReadPrincipalValidatorFunc(func(_ context.Context, user auth.User) (auth.User, error) {
		if user.ID != "11" || user.UserID != "11" || user.AuthType != "user" {
			t.Fatalf("unexpected forwarded principal: %+v", user)
		}
		return user, nil
	})
	peer := currentReadPeerVerifierFunc(func(request *http.Request) error {
		if request.RemoteAddr != "10.0.0.8:43120" {
			return errors.New("untrusted peer")
		}
		return nil
	})
	route, err := handler.NewCurrentConfigurationReadRoute(
		reader,
		1,
		apimw.AuthConfig{PrincipalValidator: principal, ForwardedIdentityVerifier: peer},
		permissions,
	)
	if err != nil {
		t.Fatal(err)
	}
	return route
}

func currentReadRequest(method, path, remoteAddress string) *http.Request {
	request := httptest.NewRequest(method, path, nil)
	request.Header.Set("X-Auth-Type", "user")
	request.Header.Set("X-Auth-ID", "11")
	request.RemoteAddr = remoteAddress
	return request
}
