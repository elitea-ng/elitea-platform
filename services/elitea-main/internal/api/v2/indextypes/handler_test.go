package indextypes_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	handler "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/indextypes"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

type currentIndexTypesReaderStub struct {
	result    handler.CurrentIndexTypes
	err       error
	projectID int32
	calls     int
}

func (stub *currentIndexTypesReaderStub) GetCurrentIndexTypes(
	_ context.Context,
	projectID int32,
) (handler.CurrentIndexTypes, error) {
	stub.calls++
	stub.projectID = projectID
	return stub.result, stub.err
}

type currentIndexTypesPrincipalValidatorFunc func(
	context.Context,
	auth.User,
) (auth.User, error)

func (function currentIndexTypesPrincipalValidatorFunc) ValidatePrincipal(
	ctx context.Context,
	principal auth.User,
) (auth.User, error) {
	return function(ctx, principal)
}

type currentIndexTypesPeerVerifierFunc func(*http.Request) error

func (function currentIndexTypesPeerVerifierFunc) VerifyForwardedIdentityPeer(
	request *http.Request,
) error {
	return function(request)
}

type currentIndexTypesPermissionResolverFunc func(
	context.Context,
	auth.User,
	string,
	string,
) (auth.PermissionResolution, error)

func (function currentIndexTypesPermissionResolverFunc) ResolvePermissions(
	ctx context.Context,
	principal auth.User,
	mode string,
	projectID string,
) (auth.PermissionResolution, error) {
	return function(ctx, principal, mode, projectID)
}

func TestCurrentIndexTypesRoutePreservesUnchangedUIContract(t *testing.T) {
	if handler.CurrentIndexTypesPath !=
		"/api/v2/elitea_core/index_types/prompt_lib/{projectID}" ||
		handler.CurrentIndexTypesMode != auth.PermissionModeDefault ||
		handler.CurrentIndexTypesPermission !=
			"models.applications.index_types.details" {
		t.Fatalf(
			"contract path=%q mode=%q permission=%q",
			handler.CurrentIndexTypesPath,
			handler.CurrentIndexTypesMode,
			handler.CurrentIndexTypesPermission,
		)
	}

	fixture := currentIndexTypesUIFixture(t)
	reader := &currentIndexTypesReaderStub{result: fixture}
	permissionCalls := 0
	route := newCurrentIndexTypesRoute(
		t,
		reader,
		currentIndexTypesPrincipalValidatorFunc(
			func(_ context.Context, principal auth.User) (auth.User, error) {
				return principal, nil
			},
		),
		currentIndexTypesPermissionResolverFunc(
			func(
				_ context.Context,
				principal auth.User,
				mode string,
				projectID string,
			) (auth.PermissionResolution, error) {
				permissionCalls++
				if principal.UserID != "11" ||
					mode != auth.PermissionModeDefault || projectID != "7" {
					t.Fatalf(
						"permission principal=%+v mode=%q project=%q",
						principal,
						mode,
						projectID,
					)
				}
				return auth.PermissionResolution{
					UserID:      11,
					Permissions: []string{handler.CurrentIndexTypesPermission},
				}, nil
			},
		),
	)

	response := httptest.NewRecorder()
	route.ServeHTTP(
		response,
		currentIndexTypesRequest(
			http.MethodGet,
			"/api/v2/elitea_core/index_types/prompt_lib/007",
			true,
			"11",
		),
	)
	if response.Code != http.StatusOK ||
		response.Header().Get("Content-Type") != "application/json" ||
		reader.calls != 1 || reader.projectID != 7 || permissionCalls != 1 {
		t.Fatalf(
			"status=%d reader=%d project=%d permission=%d body=%q",
			response.Code,
			reader.calls,
			reader.projectID,
			permissionCalls,
			response.Body.String(),
		)
	}

	// The Pylon half stays byte for byte what the pinned SDK fixture holds.
	// The body also carries the published `items`/`total` envelope since #394,
	// so the whole body is no longer the fixture; decode the three maps back
	// out and compare those. TestCurrentIndexTypesRouteAnswersPublishedEnvelope
	// BesidePylonKeys covers the published half and holds the two halves to
	// the same rows.
	var pylon handler.CurrentIndexTypes
	if err := json.Unmarshal(response.Body.Bytes(), &pylon); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(pylon, fixture) {
		t.Fatalf("pylon half drifted from the fixture: %+v", pylon)
	}

	var topLevel map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &topLevel); err != nil {
		t.Fatal(err)
	}
	if len(topLevel) != 5 || topLevel["document_types"] == nil ||
		topLevel["image_types"] == nil || topLevel["code_types"] == nil ||
		topLevel["items"] == nil || topLevel["total"] == nil ||
		topLevel["index_types"] != nil {
		t.Fatalf("incompatible top-level response=%v", topLevel)
	}
}

func TestCurrentIndexTypesRouteAuthenticatesAndAuthorizesBeforeSnapshotRead(
	t *testing.T,
) {
	tests := []struct {
		name          string
		method        string
		path          string
		authenticated bool
		principalErr  error
		allowed       bool
		wantStatus    int
	}{
		{
			name:       "missing authentication",
			method:     http.MethodGet,
			path:       "/api/v2/elitea_core/index_types/prompt_lib/7",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:          "inactive principal",
			method:        http.MethodGet,
			path:          "/api/v2/elitea_core/index_types/prompt_lib/7",
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
			path:          "/api/v2/elitea_core/index_types/prompt_lib/7",
			authenticated: true,
			wantStatus:    http.StatusForbidden,
		},
		{
			name:          "zero project denied",
			method:        http.MethodGet,
			path:          "/api/v2/elitea_core/index_types/prompt_lib/0",
			authenticated: true,
			allowed:       true,
			wantStatus:    http.StatusForbidden,
		},
		{
			name:          "nonnumeric project is not a route",
			method:        http.MethodGet,
			path:          "/api/v2/elitea_core/index_types/prompt_lib/not-an-id",
			authenticated: true,
			allowed:       true,
			wantStatus:    http.StatusNotFound,
		},
		{
			name:          "wrong mode is not exposed",
			method:        http.MethodGet,
			path:          "/api/v2/elitea_core/index_types/default/7",
			authenticated: true,
			allowed:       true,
			wantStatus:    http.StatusNotFound,
		},
		{
			name:          "post is not exposed",
			method:        http.MethodPost,
			path:          "/api/v2/elitea_core/index_types/prompt_lib/7",
			authenticated: true,
			allowed:       true,
			wantStatus:    http.StatusMethodNotAllowed,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reader := &currentIndexTypesReaderStub{}
			route := newCurrentIndexTypesRoute(
				t,
				reader,
				currentIndexTypesPrincipalValidatorFunc(
					func(
						_ context.Context,
						principal auth.User,
					) (auth.User, error) {
						return principal, test.principalErr
					},
				),
				currentIndexTypesPermissionResolverFunc(
					func(
						context.Context,
						auth.User,
						string,
						string,
					) (auth.PermissionResolution, error) {
						resolution := auth.PermissionResolution{UserID: 11}
						if test.allowed {
							resolution.Permissions = []string{
								handler.CurrentIndexTypesPermission,
							}
						}
						return resolution, nil
					},
				),
			)
			response := httptest.NewRecorder()
			route.ServeHTTP(
				response,
				currentIndexTypesRequest(
					test.method,
					test.path,
					test.authenticated,
					"11",
				),
			)
			if response.Code != test.wantStatus || reader.calls != 0 {
				t.Fatalf(
					"status=%d want=%d reader_calls=%d body=%q",
					response.Code,
					test.wantStatus,
					reader.calls,
					response.Body.String(),
				)
			}
		})
	}
}

func TestCurrentIndexTypesRoutePreservesEmptyCategoriesAndSafeFailure(
	t *testing.T,
) {
	permissions := currentIndexTypesPermissionResolverFunc(
		func(
			context.Context,
			auth.User,
			string,
			string,
		) (auth.PermissionResolution, error) {
			return auth.PermissionResolution{
				UserID:      11,
				Permissions: []string{handler.CurrentIndexTypesPermission},
			}, nil
		},
	)
	principal := currentIndexTypesPrincipalValidatorFunc(
		func(_ context.Context, principal auth.User) (auth.User, error) {
			return principal, nil
		},
	)

	route := newCurrentIndexTypesRoute(
		t,
		&currentIndexTypesReaderStub{},
		principal,
		permissions,
	)
	response := httptest.NewRecorder()
	route.ServeHTTP(
		response,
		currentIndexTypesRequest(
			http.MethodGet,
			"/api/v2/elitea_core/index_types/prompt_lib/7",
			true,
			"11",
		),
	)
	// Empty categories still answer all three, and the published half still
	// lists all three with empty extension arrays. An absent category would
	// read to a client as "this deployment indexes no images".
	if response.Code != http.StatusOK ||
		response.Body.String() !=
			"{\"items\":["+
				"{\"type\":\"document_types\",\"name\":\"Document types\","+
				"\"description\":\"File extensions the indexer loads as documents.\","+
				"\"supported_extensions\":[]},"+
				"{\"type\":\"image_types\",\"name\":\"Image types\","+
				"\"description\":\"File extensions the indexer loads as images.\","+
				"\"supported_extensions\":[]},"+
				"{\"type\":\"code_types\",\"name\":\"Code types\","+
				"\"description\":\"File extensions the indexer loads as plain-text code.\","+
				"\"supported_extensions\":[]}"+
				"],\"total\":3,"+
				"\"document_types\":{},\"image_types\":{},\"code_types\":{}}\n" {
		t.Fatalf("empty status=%d body=%q", response.Code, response.Body.String())
	}

	route = newCurrentIndexTypesRoute(
		t,
		&currentIndexTypesReaderStub{
			err: errors.New("private dependency detail"),
		},
		principal,
		permissions,
	)
	response = httptest.NewRecorder()
	route.ServeHTTP(
		response,
		currentIndexTypesRequest(
			http.MethodGet,
			"/api/v2/elitea_core/index_types/prompt_lib/7",
			true,
			"11",
		),
	)
	if response.Code != http.StatusInternalServerError ||
		response.Body.String() !=
			"{\"error\":\"Failed to get index types\"}\n" ||
		strings.Contains(response.Body.String(), "private") {
		t.Fatalf("failure status=%d body=%q", response.Code, response.Body.String())
	}
}

func TestCurrentIndexTypesRouteRejectsIncompleteComposition(t *testing.T) {
	reader := &currentIndexTypesReaderStub{}
	principal := currentIndexTypesPrincipalValidatorFunc(
		func(_ context.Context, principal auth.User) (auth.User, error) {
			return principal, nil
		},
	)
	peer := currentIndexTypesPeerVerifierFunc(
		func(*http.Request) error { return nil },
	)
	permissions := currentIndexTypesPermissionResolverFunc(
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
		PrincipalValidator:        principal,
		ForwardedIdentityVerifier: peer,
	}

	for name, test := range map[string]struct {
		reader      handler.CurrentIndexTypesReader
		authConfig  apimw.AuthConfig
		permissions auth.PermissionResolver
	}{
		"missing reader":      {authConfig: authConfig, permissions: permissions},
		"missing principal":   {reader: reader, authConfig: apimw.AuthConfig{ForwardedIdentityVerifier: peer}, permissions: permissions},
		"missing permissions": {reader: reader, authConfig: authConfig},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := handler.NewCurrentIndexTypesRoute(
				test.reader,
				test.authConfig,
				test.permissions,
			); !errors.Is(err, handler.ErrInvalidCurrentIndexTypesRoute) {
				t.Fatalf("error=%v", err)
			}
		})
	}

	// NOTE(#394): "missing peer proof" stood in the table above. A missing
	// ForwardedIdentityVerifier is no longer an incomplete composition: it is
	// the shape an OIDC or SAML install has, and apimw.Auth authenticates that
	// caller through the session cookie or the bearer token. The route is
	// composed, and TestCurrentIndexTypesRouteComposesWithoutAPeerVerifier
	// below proves it. PrincipalValidator stays mandatory, one row above.
	t.Run("composes without a peer verifier", func(t *testing.T) {
		route, err := handler.NewCurrentIndexTypesRoute(
			reader,
			apimw.AuthConfig{PrincipalValidator: principal},
			permissions,
		)
		if err != nil {
			t.Fatalf("an OIDC-only composition was refused: %v", err)
		}
		if route == nil {
			t.Fatal("an OIDC-only composition returned no route")
		}
	})

	var nilRoute *handler.CurrentIndexTypesRoute
	response := httptest.NewRecorder()
	nilRoute.ServeHTTP(
		response,
		httptest.NewRequest(
			http.MethodGet,
			"/api/v2/elitea_core/index_types/prompt_lib/7",
			nil,
		),
	)
	if response.Code != http.StatusNotFound {
		t.Fatalf("nil route status=%d", response.Code)
	}
}

func currentIndexTypesUIFixture(t *testing.T) handler.CurrentIndexTypes {
	t.Helper()
	data, err := os.ReadFile("testdata/current_index_types_ui_response.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture handler.CurrentIndexTypes
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func newCurrentIndexTypesRoute(
	t *testing.T,
	reader handler.CurrentIndexTypesReader,
	principal apimw.PrincipalValidator,
	permissions auth.PermissionResolver,
) *handler.CurrentIndexTypesRoute {
	t.Helper()
	route, err := handler.NewCurrentIndexTypesRoute(
		reader,
		apimw.AuthConfig{
			PrincipalValidator: principal,
			ForwardedIdentityVerifier: currentIndexTypesPeerVerifierFunc(
				func(*http.Request) error { return nil },
			),
		},
		permissions,
	)
	if err != nil {
		t.Fatal(err)
	}
	return route
}

func currentIndexTypesRequest(
	method string,
	path string,
	authenticated bool,
	userID string,
) *http.Request {
	request := httptest.NewRequest(method, path, nil)
	request.RemoteAddr = "10.0.0.8:43120"
	if authenticated {
		request.Header.Set("X-Auth-Type", "user")
		request.Header.Set("X-Auth-ID", userID)
	}
	return request
}
