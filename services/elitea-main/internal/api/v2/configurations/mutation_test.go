package configurations_test

import (
	"context"
	"errors"
	"io"
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

type currentConfigurationMutatorStub struct {
	create func(context.Context, configurationapp.CurrentConfigurationCreateRequest) (configurationapp.CurrentConfiguration, error)
	update func(context.Context, configurationapp.CurrentConfigurationUpdateRequest) (configurationapp.CurrentConfiguration, error)
	delete func(context.Context, configurationapp.CurrentConfigurationDeleteRequest) error
	calls  int
}

func (stub *currentConfigurationMutatorStub) Create(
	ctx context.Context,
	request configurationapp.CurrentConfigurationCreateRequest,
) (configurationapp.CurrentConfiguration, error) {
	stub.calls++
	if stub.create == nil {
		return configurationapp.CurrentConfiguration{}, nil
	}
	return stub.create(ctx, request)
}

func (stub *currentConfigurationMutatorStub) Update(
	ctx context.Context,
	request configurationapp.CurrentConfigurationUpdateRequest,
) (configurationapp.CurrentConfiguration, error) {
	stub.calls++
	if stub.update == nil {
		return configurationapp.CurrentConfiguration{}, nil
	}
	return stub.update(ctx, request)
}

func (stub *currentConfigurationMutatorStub) Delete(
	ctx context.Context,
	request configurationapp.CurrentConfigurationDeleteRequest,
) error {
	stub.calls++
	if stub.delete == nil {
		return nil
	}
	return stub.delete(ctx, request)
}

func TestCurrentConfigurationMutationRouteBindsExactPathsPermissionsAndDependencies(t *testing.T) {
	if handler.CurrentConfigurationListPath != "/api/v2/configurations/configurations/{projectID}" ||
		handler.CurrentConfigurationDetailsPath != "/api/v2/configurations/configuration/{projectID}/{configID}" ||
		handler.CurrentConfigurationMutationMode != auth.PermissionModeDefault ||
		handler.CurrentConfigurationCreatePermission != "configurations.configuration.create" ||
		handler.CurrentConfigurationUpdatePermission != "configurations.configuration.update" ||
		handler.CurrentConfigurationDeletePermission != "configurations.configuration.delete" {
		t.Fatalf(
			"mutation contract drifted: list=%q details=%q mode=%q permissions=%q,%q,%q",
			handler.CurrentConfigurationListPath,
			handler.CurrentConfigurationDetailsPath,
			handler.CurrentConfigurationMutationMode,
			handler.CurrentConfigurationCreatePermission,
			handler.CurrentConfigurationUpdatePermission,
			handler.CurrentConfigurationDeletePermission,
		)
	}

	mutator := &currentConfigurationMutatorStub{}
	principal := currentReadPrincipalValidatorFunc(func(_ context.Context, user auth.User) (auth.User, error) {
		return user, nil
	})
	peer := currentReadPeerVerifierFunc(func(*http.Request) error { return nil })
	permissions := permissionResolverFunc(func(context.Context, auth.User, string, string) (auth.PermissionResolution, error) {
		return auth.PermissionResolution{}, nil
	})
	authConfig := apimw.AuthConfig{PrincipalValidator: principal, ForwardedIdentityVerifier: peer}

	for name, test := range map[string]struct {
		mutator     handler.CurrentConfigurationMutator
		authConfig  apimw.AuthConfig
		permissions auth.PermissionResolver
	}{
		"missing mutator":      {authConfig: authConfig, permissions: permissions},
		"missing principal":    {mutator: mutator, authConfig: apimw.AuthConfig{ForwardedIdentityVerifier: peer}, permissions: permissions},
		"no credential reader": {mutator: mutator, authConfig: apimw.AuthConfig{PrincipalValidator: principal}, permissions: permissions},
		"missing permissions":  {mutator: mutator, authConfig: authConfig},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := handler.NewCurrentConfigurationMutationRoute(
				test.mutator,
				test.authConfig,
				test.permissions,
			); !errors.Is(err, handler.ErrInvalidCurrentConfigurationMutationRoute) {
				t.Fatalf("error=%v, want %v", err, handler.ErrInvalidCurrentConfigurationMutationRoute)
			}
		})
	}
}

func TestCurrentConfigurationCreateUsesAuthenticatedAuthorAndReturnsSealedDTO(t *testing.T) {
	createdAt := time.Date(2026, time.July, 22, 12, 0, 0, 0, time.UTC)
	label := "Team GitHub"
	authorID := int32(42)
	mutator := &currentConfigurationMutatorStub{
		create: func(_ context.Context, request configurationapp.CurrentConfigurationCreateRequest) (configurationapp.CurrentConfiguration, error) {
			if request.ProjectID != 7 || request.AuthorID != authorID || request.EliteaTitle != "team_github" ||
				request.Label == nil || *request.Label != label || request.Type != "github" || !request.Shared {
				t.Fatalf("unexpected create request: %+v", request)
			}
			if request.Data["username"] != "octocat" || request.Data["password"] != "RAW_TEST_SECRET" {
				t.Fatalf("create data was not preserved for the mutation service: %#v", request.Data)
			}
			return configurationapp.CurrentConfiguration{
				ID: 9, UUID: "00000000-0000-4000-8000-000000000009", ProjectID: 7,
				EliteaTitle: "team_github", Label: &label, Type: "github", Section: "credentials",
				Data: map[string]any{"username": "octocat", "password": "{{secret.configuration.9.password}}"},
				Meta: map[string]any{}, Shared: true, StatusOK: true, Source: "user",
				AuthorID: &authorID, CreatedAt: createdAt,
			}, nil
		},
	}
	permissionCalls := 0
	permissions := permissionResolverFunc(func(_ context.Context, user auth.User, mode, projectID string) (auth.PermissionResolution, error) {
		permissionCalls++
		if user.UserID != "11" || mode != auth.PermissionModeDefault || projectID != "7" {
			t.Fatalf("permission input user=%+v mode=%q project=%q", user, mode, projectID)
		}
		return auth.PermissionResolution{UserID: int64(authorID), Permissions: []string{handler.CurrentConfigurationCreatePermission}}, nil
	})
	route := newCurrentConfigurationMutationRoute(t, mutator, permissions)
	request := currentMutationRequest(
		http.MethodPost,
		"/api/v2/configurations/configurations/007",
		`{"elitea_title":"team_github","label":"Team GitHub","type":"github","shared":true,"data":{"username":"octocat","password":"RAW_TEST_SECRET"}}`,
	)
	response := httptest.NewRecorder()
	route.ServeHTTP(response, request)

	if response.Code != http.StatusOK || mutator.calls != 1 || permissionCalls != 1 {
		t.Fatalf("status=%d mutator_calls=%d permission_calls=%d body=%s", response.Code, mutator.calls, permissionCalls, response.Body.String())
	}
	for _, fragment := range []string{
		`"id":9`, `"project_id":7`, `"elitea_title":"team_github"`, `"label":"Team GitHub"`,
		`"type":"github"`, `"section":"credentials"`, `"password":"{{secret.configuration.9.password}}"`,
		`"status_ok":true`, `"source":"user"`, `"author_id":42`, `"is_pinned":false`,
	} {
		if !strings.Contains(response.Body.String(), fragment) {
			t.Fatalf("response %s does not contain %s", response.Body.String(), fragment)
		}
	}
	if strings.Contains(response.Body.String(), "RAW_TEST_SECRET") {
		t.Fatalf("request secret was reflected in response: %s", response.Body.String())
	}
	assertCurrentMutationNoStore(t, response)
}

func TestCurrentConfigurationCreatePreservesCurrentNullableLabel(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
	}{
		{name: "omitted", body: `{"elitea_title":"team_github","type":"github","data":{}}`},
		{name: "explicit null", body: `{"elitea_title":"team_github","label":null,"type":"github","data":{}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			mutator := &currentConfigurationMutatorStub{
				create: func(_ context.Context, request configurationapp.CurrentConfigurationCreateRequest) (configurationapp.CurrentConfiguration, error) {
					if request.Label != nil {
						t.Fatalf("label = %q, want nil", *request.Label)
					}
					return configurationapp.CurrentConfiguration{
						ID: 9, UUID: "00000000-0000-4000-8000-000000000009", ProjectID: 7,
						EliteaTitle: "team_github", Type: "github", Section: "credentials",
						Data: map[string]any{}, Meta: map[string]any{}, Source: "user",
						CreatedAt: time.Date(2026, time.July, 22, 12, 0, 0, 0, time.UTC),
					}, nil
				},
			}
			route := newCurrentConfigurationMutationRoute(t, mutator, allowCurrentMutationPermission(
				handler.CurrentConfigurationCreatePermission,
				42,
			))
			response := httptest.NewRecorder()
			route.ServeHTTP(response, currentMutationRequest(
				http.MethodPost,
				"/api/v2/configurations/configurations/7",
				test.body,
			))
			if response.Code != http.StatusOK || mutator.calls != 1 {
				t.Fatalf("status=%d calls=%d body=%s", response.Code, mutator.calls, response.Body.String())
			}
		})
	}
}

func TestCurrentConfigurationUpdatePreservesPublicFieldPresence(t *testing.T) {
	mutator := &currentConfigurationMutatorStub{
		update: func(_ context.Context, request configurationapp.CurrentConfigurationUpdateRequest) (configurationapp.CurrentConfiguration, error) {
			if request.ProjectID != 7 || request.ConfigurationID != 9 || request.AuthorID != 42 {
				t.Fatalf("unexpected trusted identities: %+v", request)
			}
			if request.EliteaTitle != nil || !request.LabelSet || request.Label != nil ||
				!request.DataSet || request.Data != nil || !request.MetaSet || request.Meta == nil || len(request.Meta) != 0 ||
				request.Shared == nil || *request.Shared {
				t.Fatalf("PUT presence was not preserved: %+v", request)
			}
			return configurationapp.CurrentConfiguration{
				ID: 9, UUID: "00000000-0000-4000-8000-000000000009", ProjectID: 7,
				EliteaTitle: "team_github", Type: "github", Section: "credentials",
				Data: map[string]any{"password": "{{secret.configuration.9.password}}"},
				Meta: map[string]any{}, Source: "user", CreatedAt: time.Date(2026, time.July, 22, 12, 0, 0, 0, time.UTC),
			}, nil
		},
	}
	route := newCurrentConfigurationMutationRoute(t, mutator, allowCurrentMutationPermission(
		handler.CurrentConfigurationUpdatePermission,
		42,
	))
	response := httptest.NewRecorder()
	route.ServeHTTP(response, currentMutationRequest(
		http.MethodPut,
		"/api/v2/configurations/configuration/7/9",
		`{"label":null,"data":null,"meta":{},"shared":false}`,
	))

	if response.Code != http.StatusOK || mutator.calls != 1 {
		t.Fatalf("status=%d mutator_calls=%d body=%s", response.Code, mutator.calls, response.Body.String())
	}
	if strings.Contains(response.Body.String(), `"options"`) {
		t.Fatalf("unset optional enrichment leaked into response: %s", response.Body.String())
	}
	assertCurrentMutationNoStore(t, response)
}

func TestCurrentConfigurationDeleteReturnsExactNoContentContract(t *testing.T) {
	mutator := &currentConfigurationMutatorStub{
		delete: func(_ context.Context, request configurationapp.CurrentConfigurationDeleteRequest) error {
			if request.ProjectID != 7 || request.ConfigurationID != 9 || request.AuthorID != 42 {
				t.Fatalf("unexpected delete request: %+v", request)
			}
			return nil
		},
	}
	route := newCurrentConfigurationMutationRoute(t, mutator, allowCurrentMutationPermission(
		handler.CurrentConfigurationDeletePermission,
		42,
	))
	response := httptest.NewRecorder()
	route.ServeHTTP(response, currentMutationRequest(
		http.MethodDelete,
		"/api/v2/configurations/configuration/7/9",
		"",
	))

	if response.Code != http.StatusNoContent || response.Body.Len() != 0 || mutator.calls != 1 {
		t.Fatalf("status=%d body=%q mutator_calls=%d", response.Code, response.Body.String(), mutator.calls)
	}
	assertCurrentMutationNoStore(t, response)
}

func TestCurrentConfigurationMutationRejectsUntrustedTopLevelFields(t *testing.T) {
	mutator := &currentConfigurationMutatorStub{}
	route := newCurrentConfigurationMutationRoute(t, mutator, permissionResolverFunc(
		func(context.Context, auth.User, string, string) (auth.PermissionResolution, error) {
			return auth.PermissionResolution{
				UserID: 42,
				Permissions: []string{
					handler.CurrentConfigurationCreatePermission,
					handler.CurrentConfigurationUpdatePermission,
				},
			}, nil
		},
	))

	createBase := `{"elitea_title":"team_github","label":"Team GitHub","type":"github","data":{}}`
	for _, field := range []string{
		"project_id", "author_id", "uuid", "meta", "section", "source", "status_ok", "status_logs",
		"id", "created_at", "updated_at", "is_pinned", "options",
	} {
		t.Run("create "+field, func(t *testing.T) {
			body := strings.TrimSuffix(createBase, "}") + `,"` + field + `":"caller-owned"}`
			response := httptest.NewRecorder()
			route.ServeHTTP(response, currentMutationRequest(
				http.MethodPost,
				"/api/v2/configurations/configurations/7",
				body,
			))
			assertCurrentMutationError(t, response, http.StatusBadRequest, "field is not allowed", field)
		})
	}

	for _, field := range []string{
		"project_id", "author_id", "uuid", "type", "section", "source", "status_ok", "status_logs",
		"id", "created_at", "updated_at", "is_pinned", "options",
	} {
		t.Run("update "+field, func(t *testing.T) {
			response := httptest.NewRecorder()
			route.ServeHTTP(response, currentMutationRequest(
				http.MethodPut,
				"/api/v2/configurations/configuration/7/9",
				`{"`+field+`":"caller-owned"}`,
			))
			assertCurrentMutationError(t, response, http.StatusBadRequest, "field is not allowed", field)
		})
	}
	if mutator.calls != 0 {
		t.Fatalf("server-owned fields reached mutator %d times", mutator.calls)
	}
}

func TestCurrentConfigurationCreateStrictlyRejectsAmbiguousOrInvalidJSON(t *testing.T) {
	mutator := &currentConfigurationMutatorStub{}
	route := newCurrentConfigurationMutationRoute(t, mutator, allowCurrentMutationPermission(
		handler.CurrentConfigurationCreatePermission,
		42,
	))
	valid := `{"elitea_title":"team_github","label":"Team GitHub","type":"github","data":{}}`
	oversized := `{"elitea_title":"team_github","label":"Team GitHub","type":"github","data":{"padding":"` +
		strings.Repeat("x", int(handler.MaxCurrentConfigurationMutationBodyBytes)) + `"}}`

	for _, test := range []struct {
		name        string
		body        string
		contentType string
		wantStatus  int
		wantError   string
		wantField   string
	}{
		{name: "missing content type", body: valid, wantStatus: http.StatusBadRequest, wantError: "invalid request body", wantField: "body"},
		{name: "non object", body: `[]`, contentType: "application/json", wantStatus: http.StatusBadRequest, wantError: "invalid request body", wantField: "body"},
		{name: "trailing document", body: valid + `{}`, contentType: "application/json", wantStatus: http.StatusBadRequest, wantError: "invalid request body", wantField: "body"},
		{name: "duplicate public field", body: `{"elitea_title":"a","elitea_title":"b","label":"Label","type":"github","data":{}}`, contentType: "application/json", wantStatus: http.StatusBadRequest, wantError: "invalid request body", wantField: "body"},
		{name: "duplicate nested field", body: `{"elitea_title":"a","label":"Label","type":"github","data":{"token":"a","token":"b"}}`, contentType: "application/json", wantStatus: http.StatusBadRequest, wantError: "invalid request body", wantField: "body"},
		{name: "unknown field", body: strings.TrimSuffix(valid, "}") + `,"not documented":true}`, contentType: "application/json", wantStatus: http.StatusBadRequest, wantError: "field is not allowed", wantField: "body"},
		{name: "missing required data", body: `{"elitea_title":"a","label":"Label","type":"github"}`, contentType: "application/json", wantStatus: http.StatusBadRequest, wantError: "field is required", wantField: "data"},
		{name: "null required data", body: `{"elitea_title":"a","label":"Label","type":"github","data":null}`, contentType: "application/json", wantStatus: http.StatusBadRequest, wantError: "invalid field value", wantField: "data"},
		{name: "null shared", body: `{"elitea_title":"a","label":"Label","type":"github","shared":null,"data":{}}`, contentType: "application/json", wantStatus: http.StatusBadRequest, wantError: "invalid field value", wantField: "shared"},
		{name: "oversized", body: oversized, contentType: "application/json", wantStatus: http.StatusRequestEntityTooLarge, wantError: "request body too large", wantField: "body"},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := currentMutationRequest(http.MethodPost, "/api/v2/configurations/configurations/7", test.body)
			if test.contentType == "" {
				request.Header.Del("Content-Type")
			} else {
				request.Header.Set("Content-Type", test.contentType)
			}
			response := httptest.NewRecorder()
			route.ServeHTTP(response, request)
			assertCurrentMutationError(t, response, test.wantStatus, test.wantError, test.wantField)
		})
	}
	if mutator.calls != 0 {
		t.Fatalf("invalid bodies reached mutator %d times", mutator.calls)
	}
}

func TestCurrentConfigurationMutationAuthenticatesAndAuthorizesBeforeReadingBody(t *testing.T) {
	mutator := &currentConfigurationMutatorStub{}
	deniedRoute := newCurrentConfigurationMutationRoute(t, mutator, permissionResolverFunc(
		func(context.Context, auth.User, string, string) (auth.PermissionResolution, error) {
			return auth.PermissionResolution{UserID: 42}, nil
		},
	))

	for _, test := range []struct {
		name       string
		path       string
		authorized bool
		wantStatus int
	}{
		{name: "unauthenticated", path: "/api/v2/configurations/configurations/7", wantStatus: http.StatusUnauthorized},
		{name: "permission denied", path: "/api/v2/configurations/configurations/7", authorized: true, wantStatus: http.StatusForbidden},
		{name: "invalid project denied", path: "/api/v2/configurations/configurations/not-an-id", authorized: true, wantStatus: http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := &observedReadCloser{}
			request := httptest.NewRequest(http.MethodPost, test.path, body)
			request.Header.Set("Content-Type", "application/json")
			request.RemoteAddr = "10.0.0.8:43120"
			if test.authorized {
				request.Header.Set("X-Auth-Type", "user")
				request.Header.Set("X-Auth-ID", "11")
			}
			response := httptest.NewRecorder()
			deniedRoute.ServeHTTP(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf("status=%d want=%d body=%s", response.Code, test.wantStatus, response.Body.String())
			}
			if body.read {
				t.Fatal("denied request body was read")
			}
		})
	}
	if mutator.calls != 0 {
		t.Fatalf("denied requests reached mutator %d times", mutator.calls)
	}
}

func TestCurrentConfigurationMutationMapsSafeServiceErrors(t *testing.T) {
	for _, test := range []struct {
		name       string
		err        error
		wantStatus int
		wantError  string
		wantField  string
	}{
		{name: "not found", err: configurationapp.ErrCurrentConfigurationNotFound, wantStatus: http.StatusNotFound, wantError: "Configuration not found", wantField: "configuration_id"},
		{name: "conflict", err: configurationapp.ErrCurrentConfigurationConflict, wantStatus: http.StatusBadRequest, wantError: "Configuration already exists", wantField: "elitea_title"},
		{name: "invalid", err: &configurationapp.CurrentConfigurationMutationError{Code: configurationapp.CurrentConfigurationMutationInvalid, Field: "data.token"}, wantStatus: http.StatusBadRequest, wantError: "Invalid configuration", wantField: "data.token"},
		{name: "unknown type", err: &configurationapp.CurrentConfigurationMutationError{Code: configurationapp.CurrentConfigurationMutationUnknownType, Field: "type"}, wantStatus: http.StatusBadRequest, wantError: "Unknown configuration type", wantField: "type"},
		{name: "immutable", err: &configurationapp.CurrentConfigurationMutationError{Code: configurationapp.CurrentConfigurationMutationImmutable, Field: "data.key"}, wantStatus: http.StatusBadRequest, wantError: "Configuration field is immutable", wantField: "data.key"},
		{name: "normalizer gap", err: &configurationapp.CurrentConfigurationMutationError{Code: configurationapp.CurrentConfigurationMutationNormalizationRequired, Field: "data"}, wantStatus: http.StatusNotImplemented, wantError: "Configuration normalization is not implemented for this type", wantField: "data"},
		{name: "unknown error code", err: &configurationapp.CurrentConfigurationMutationError{Code: "unexpected", Field: "data"}, wantStatus: http.StatusInternalServerError, wantError: "Unexpected error", wantField: "unknown"},
		{name: "internal details hidden", err: errors.New("RAW_TEST_SECRET database failure"), wantStatus: http.StatusInternalServerError, wantError: "Unexpected error", wantField: "unknown"},
	} {
		t.Run(test.name, func(t *testing.T) {
			mutator := &currentConfigurationMutatorStub{
				update: func(context.Context, configurationapp.CurrentConfigurationUpdateRequest) (configurationapp.CurrentConfiguration, error) {
					return configurationapp.CurrentConfiguration{}, test.err
				},
			}
			route := newCurrentConfigurationMutationRoute(t, mutator, allowCurrentMutationPermission(
				handler.CurrentConfigurationUpdatePermission,
				42,
			))
			response := httptest.NewRecorder()
			route.ServeHTTP(response, currentMutationRequest(
				http.MethodPut,
				"/api/v2/configurations/configuration/7/9",
				`{}`,
			))
			assertCurrentMutationError(t, response, test.wantStatus, test.wantError, test.wantField)
			if strings.Contains(response.Body.String(), "RAW_TEST_SECRET") {
				t.Fatalf("service details leaked: %s", response.Body.String())
			}
		})
	}
}

func newCurrentConfigurationMutationRoute(
	t *testing.T,
	mutator handler.CurrentConfigurationMutator,
	permissions auth.PermissionResolver,
) *handler.CurrentConfigurationMutationRoute {
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
	route, err := handler.NewCurrentConfigurationMutationRoute(
		mutator,
		apimw.AuthConfig{PrincipalValidator: principal, ForwardedIdentityVerifier: peer},
		permissions,
	)
	if err != nil {
		t.Fatal(err)
	}
	return route
}

func allowCurrentMutationPermission(permission string, userID int64) auth.PermissionResolver {
	return permissionResolverFunc(func(_ context.Context, _ auth.User, mode, projectID string) (auth.PermissionResolution, error) {
		if mode != auth.PermissionModeDefault || projectID != "7" {
			return auth.PermissionResolution{}, errors.New("unexpected permission scope")
		}
		return auth.PermissionResolution{UserID: userID, Permissions: []string{permission}}, nil
	})
}

func currentMutationRequest(method, path, body string) *http.Request {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("X-Auth-Type", "user")
	request.Header.Set("X-Auth-ID", "11")
	request.Header.Set("Content-Type", "application/json")
	request.RemoteAddr = "10.0.0.8:43120"
	return request
}

func assertCurrentMutationError(t *testing.T, response *httptest.ResponseRecorder, status int, message, field string) {
	t.Helper()
	want := `{"error":"` + message + `","field":"` + field + `"}` + "\n"
	if response.Code != status || response.Body.String() != want {
		t.Fatalf("status=%d want=%d body=%q want_body=%q", response.Code, status, response.Body.String(), want)
	}
	assertCurrentMutationNoStore(t, response)
}

func assertCurrentMutationNoStore(t *testing.T, response *httptest.ResponseRecorder) {
	t.Helper()
	if response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Pragma") != "no-cache" {
		t.Fatalf("sensitive response is cacheable: headers=%v", response.Header())
	}
}

type observedReadCloser struct {
	read bool
}

func (body *observedReadCloser) Read([]byte) (int, error) {
	body.read = true
	return 0, io.EOF
}

func (*observedReadCloser) Close() error { return nil }
