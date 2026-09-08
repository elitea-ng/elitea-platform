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

type currentModelCatalogReaderStub struct {
	get   func(context.Context, configurationapp.CurrentModelCatalogQuery) (configurationapp.CurrentModelCatalogResponse, error)
	calls int
}

func (stub *currentModelCatalogReaderStub) Get(
	ctx context.Context,
	query configurationapp.CurrentModelCatalogQuery,
) (configurationapp.CurrentModelCatalogResponse, error) {
	stub.calls++
	if stub.get == nil {
		return configurationapp.CurrentModelCatalogResponse{}, nil
	}
	return stub.get(ctx, query)
}

func TestCurrentModelCatalogRouteBindsCurrentPathAndSafeReadPermission(t *testing.T) {
	if handler.CurrentModelCatalogPath != "/api/v2/configurations/models/{projectID}" ||
		handler.CurrentModelCatalogMode != auth.PermissionModeDefault ||
		handler.CurrentModelCatalogPermission != "configurations.configurations.list" {
		t.Fatalf(
			"current model route drifted: path=%q mode=%q permission=%q",
			handler.CurrentModelCatalogPath,
			handler.CurrentModelCatalogMode,
			handler.CurrentModelCatalogPermission,
		)
	}

	reader := &currentModelCatalogReaderStub{}
	principal := currentReadPrincipalValidatorFunc(func(_ context.Context, user auth.User) (auth.User, error) { return user, nil })
	peer := currentReadPeerVerifierFunc(func(*http.Request) error { return nil })
	permissions := permissionResolverFunc(func(context.Context, auth.User, string, string) (auth.PermissionResolution, error) {
		return auth.PermissionResolution{}, nil
	})
	authConfig := apimw.AuthConfig{PrincipalValidator: principal, ForwardedIdentityVerifier: peer}

	for name, test := range map[string]struct {
		reader          handler.CurrentModelCatalogReader
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
			if _, err := handler.NewCurrentModelCatalogRoute(
				test.reader,
				test.publicProjectID,
				test.authConfig,
				test.permissions,
			); !errors.Is(err, handler.ErrInvalidCurrentModelCatalogRoute) {
				t.Fatalf("error = %v, want %v", err, handler.ErrInvalidCurrentModelCatalogRoute)
			}
		})
	}
}

func TestCurrentModelCatalogRoutePreservesSelectionAndResponse(t *testing.T) {
	reader := &currentModelCatalogReaderStub{
		get: func(_ context.Context, query configurationapp.CurrentModelCatalogQuery) (configurationapp.CurrentModelCatalogResponse, error) {
			if query.ProjectID != 7 || query.PublicProjectID != 1 ||
				query.Section != configurationapp.CurrentModelSectionEmbedding || !query.IncludeShared {
				t.Fatalf("unexpected model query: %+v", query)
			}
			name := "embed-current"
			projectID := int32(7)
			return configurationapp.CurrentModelCatalogResponse{
				Total: 1,
				Items: []configurationapp.CurrentModelCatalogItem{{
					Name: name, ProjectID: projectID, Shared: false, Default: true,
				}},
				DefaultModelName:      &name,
				DefaultModelProjectID: &projectID,
			}, nil
		},
	}
	permissionCalls := 0
	permissions := permissionResolverFunc(func(_ context.Context, user auth.User, mode, projectID string) (auth.PermissionResolution, error) {
		permissionCalls++
		if user.UserID != "11" || mode != auth.PermissionModeDefault || projectID != "7" {
			t.Fatalf("permission input user=%+v mode=%q project=%q", user, mode, projectID)
		}
		return auth.PermissionResolution{UserID: 11, Permissions: []string{handler.CurrentModelCatalogPermission}}, nil
	})
	route := newCurrentModelCatalogRoute(t, reader, permissions)

	response := httptest.NewRecorder()
	route.ServeHTTP(response, currentReadRequest(
		http.MethodGet,
		"/api/v2/configurations/models/007?section=EmBeDdInG&include_shared=TrUe",
		"10.0.0.8:43120",
	))

	if response.Code != http.StatusOK || reader.calls != 1 || permissionCalls != 1 {
		t.Fatalf("status=%d reader_calls=%d permission_calls=%d body=%s", response.Code, reader.calls, permissionCalls, response.Body.String())
	}
	for _, fragment := range []string{
		`"total":1`, `"name":"embed-current"`, `"project_id":7`,
		`"shared":false`, `"default":true`, `"default_model_name":"embed-current"`,
	} {
		if !strings.Contains(response.Body.String(), fragment) {
			t.Fatalf("response %s does not contain %s", response.Body.String(), fragment)
		}
	}
}

func TestCurrentModelCatalogRouteDefaultsToLLMAndFailsClosed(t *testing.T) {
	reader := &currentModelCatalogReaderStub{
		get: func(_ context.Context, query configurationapp.CurrentModelCatalogQuery) (configurationapp.CurrentModelCatalogResponse, error) {
			if query.Section != configurationapp.CurrentModelSectionLLM || query.IncludeShared {
				t.Fatalf("unexpected default query: %+v", query)
			}
			return configurationapp.CurrentModelCatalogResponse{}, configurationapp.ErrInvalidCurrentModelCatalogRequest
		},
	}
	permissions := permissionResolverFunc(func(context.Context, auth.User, string, string) (auth.PermissionResolution, error) {
		return auth.PermissionResolution{UserID: 11, Permissions: []string{handler.CurrentModelCatalogPermission}}, nil
	})
	route := newCurrentModelCatalogRoute(t, reader, permissions)

	invalid := httptest.NewRecorder()
	route.ServeHTTP(invalid, currentReadRequest(
		http.MethodGet,
		"/api/v2/configurations/models/7",
		"10.0.0.8:43120",
	))
	if invalid.Code != http.StatusBadRequest || invalid.Body.String() != "{\"error\":\"invalid model catalog request\"}\n" {
		t.Fatalf("status=%d body=%q", invalid.Code, invalid.Body.String())
	}

	unknownReader := &currentModelCatalogReaderStub{}
	unknown := newCurrentModelCatalogRoute(t, unknownReader, permissions)
	unknownResponse := httptest.NewRecorder()
	unknown.ServeHTTP(unknownResponse, currentReadRequest(
		http.MethodGet,
		"/api/v2/configurations/models/7?section=unknown-current-section",
		"10.0.0.8:43120",
	))
	if unknownResponse.Code != http.StatusOK || unknownReader.calls != 0 ||
		unknownResponse.Body.String() != "{\"total\":0,\"items\":[],\"default_model_name\":null,\"default_model_project_id\":null}\n" {
		t.Fatalf("unknown status=%d calls=%d body=%q", unknownResponse.Code, unknownReader.calls, unknownResponse.Body.String())
	}

	deniedReader := &currentModelCatalogReaderStub{}
	denied := newCurrentModelCatalogRoute(t, deniedReader, permissionResolverFunc(
		func(context.Context, auth.User, string, string) (auth.PermissionResolution, error) {
			return auth.PermissionResolution{UserID: 11}, nil
		},
	))
	response := httptest.NewRecorder()
	denied.ServeHTTP(response, currentReadRequest(
		http.MethodGet,
		"/api/v2/configurations/models/7",
		"10.0.0.8:43120",
	))
	if response.Code != http.StatusForbidden || deniedReader.calls != 0 {
		t.Fatalf("status=%d service_calls=%d body=%s", response.Code, deniedReader.calls, response.Body.String())
	}
}

func newCurrentModelCatalogRoute(
	t *testing.T,
	reader handler.CurrentModelCatalogReader,
	permissions auth.PermissionResolver,
) *handler.CurrentModelCatalogRoute {
	t.Helper()
	principal := currentReadPrincipalValidatorFunc(func(_ context.Context, user auth.User) (auth.User, error) { return user, nil })
	peer := currentReadPeerVerifierFunc(func(request *http.Request) error {
		if request.RemoteAddr != "10.0.0.8:43120" {
			return errors.New("untrusted peer")
		}
		return nil
	})
	route, err := handler.NewCurrentModelCatalogRoute(
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
