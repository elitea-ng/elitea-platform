package indexing_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	handler "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/indexing"
	indexmetaapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexmeta"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

type currentIndexConfigurationStub struct {
	request indexmetaapp.ConfigurationRequest
	err     error
	calls   int
}

func (s *currentIndexConfigurationStub) SaveConfiguration(
	_ context.Context,
	request indexmetaapp.ConfigurationRequest,
) error {
	s.calls++
	s.request = request
	return s.err
}

func newCurrentIndexConfigurationRoute(
	t *testing.T,
	saver handler.CurrentIndexConfigurationSaver,
	permissions auth.PermissionResolver,
) *handler.CurrentIndexConfigurationRoute {
	t.Helper()
	route, err := handler.NewCurrentIndexConfigurationRoute(
		saver,
		apimw.AuthConfig{
			PrincipalValidator: principalValidatorFunc(func(_ context.Context, user auth.User) (auth.User, error) {
				if user.ID != "11" {
					return auth.User{}, errors.New("unexpected user")
				}
				return user, nil
			}),
			ForwardedIdentityVerifier: forwardedPeerVerifierFunc(func(request *http.Request) error {
				if request.RemoteAddr != "10.0.0.8:43120" {
					return errors.New("untrusted peer")
				}
				return nil
			}),
		},
		permissions,
	)
	if err != nil {
		t.Fatal(err)
	}
	return route
}

func currentIndexConfigurationRequest(path, body string) *http.Request {
	request := httptest.NewRequest(http.MethodPut, path, strings.NewReader(body))
	request.Header.Set("X-Auth-Type", "user")
	request.Header.Set("X-Auth-ID", "11")
	request.Header.Set("Content-Type", "application/json")
	request.RemoteAddr = "10.0.0.8:43120"
	return request
}

func grantingResolver(permission string) auth.PermissionResolver {
	return permissionResolverFunc(func(_ context.Context, _ auth.User, _, _ string) (auth.PermissionResolution, error) {
		return auth.PermissionResolution{UserID: 11, Permissions: []string{permission}}, nil
	})
}

// The route is the SAVE half of the index editor's Save / Save & Reindex
// split. Its contract — path, method, mode and permission — is asserted
// literally, because the client interpolates the same string and the RBAC
// name is what decides whether a member who may configure an index can use it.
func TestCurrentIndexConfigurationRouteContract(t *testing.T) {
	t.Parallel()

	if handler.CurrentIndexConfigurationPath !=
		"/api/v2/elitea_core/index_meta/prompt_lib/{projectID}/{toolkitID}/{indexMetaID}/configuration" ||
		handler.CurrentIndexConfigurationMethod != http.MethodPut ||
		handler.CurrentIndexConfigurationMode != auth.PermissionModeDefault ||
		handler.CurrentIndexConfigurationPermission != "models.applications.index_meta.edit" {
		t.Fatalf(
			"path=%q method=%q mode=%q permission=%q",
			handler.CurrentIndexConfigurationPath,
			handler.CurrentIndexConfigurationMethod,
			handler.CurrentIndexConfigurationMode,
			handler.CurrentIndexConfigurationPermission,
		)
	}
}

func TestCurrentIndexConfigurationRouteSavesTheConfiguration(t *testing.T) {
	t.Parallel()

	saver := &currentIndexConfigurationStub{}
	route := newCurrentIndexConfigurationRoute(t, saver, permissionResolverFunc(
		func(_ context.Context, user auth.User, mode, projectID string) (auth.PermissionResolution, error) {
			if user.UserID != "11" || mode != auth.PermissionModeDefault || projectID != "7" {
				t.Fatalf("user=%+v mode=%q project=%q", user, mode, projectID)
			}
			return auth.PermissionResolution{
				UserID:      11,
				Permissions: []string{handler.CurrentIndexConfigurationPermission},
			}, nil
		}))

	response := httptest.NewRecorder()
	route.ServeHTTP(response, currentIndexConfigurationRequest(
		"/api/v2/elitea_core/index_meta/prompt_lib/007/009/docs/configuration",
		`{"index_configuration":{"index_name":"docs","progress_step":75}}`,
	))

	if response.Code != http.StatusOK || response.Body.String() != "{\"ok\":true}\n" {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}
	if saver.calls != 1 || saver.request.ProjectID != 7 || saver.request.ActorUserID != 11 ||
		saver.request.ToolkitID != 9 || saver.request.IndexName != "docs" {
		t.Fatalf("calls=%d request=%+v", saver.calls, saver.request)
	}
	// The configuration reaches the application byte for byte: re-encoding it
	// here would be a second JSON round trip the index run never performs.
	var decoded map[string]any
	if err := json.Unmarshal(saver.request.Configuration, &decoded); err != nil {
		t.Fatalf("configuration=%s: %v", saver.request.Configuration, err)
	}
	if decoded["index_name"] != "docs" {
		t.Fatalf("configuration=%s", saver.request.Configuration)
	}
}

func TestCurrentIndexConfigurationRouteAuthorizesBeforeSaving(t *testing.T) {
	t.Parallel()

	for name, test := range map[string]struct {
		remote      string
		permissions []string
		wantStatus  int
		wantCalls   int
	}{
		"untrusted peer": {
			remote:      "192.0.2.8:443",
			permissions: []string{handler.CurrentIndexConfigurationPermission},
			wantStatus:  http.StatusUnauthorized,
		},
		"viewer denied": {remote: "10.0.0.8:43120", wantStatus: http.StatusForbidden},
		"the delete permission does not grant the save": {
			remote:      "10.0.0.8:43120",
			permissions: []string{handler.CurrentIndexMetaDeletePermission},
			wantStatus:  http.StatusForbidden,
		},
		"editor accepted": {
			remote:      "10.0.0.8:43120",
			permissions: []string{handler.CurrentIndexConfigurationPermission},
			wantStatus:  http.StatusOK,
			wantCalls:   1,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			saver := &currentIndexConfigurationStub{}
			route := newCurrentIndexConfigurationRoute(t, saver, permissionResolverFunc(
				func(_ context.Context, _ auth.User, _, _ string) (auth.PermissionResolution, error) {
					return auth.PermissionResolution{UserID: 11, Permissions: test.permissions}, nil
				}))
			request := currentIndexConfigurationRequest(
				"/api/v2/elitea_core/index_meta/prompt_lib/7/9/docs/configuration",
				`{"index_configuration":{}}`,
			)
			request.RemoteAddr = test.remote
			response := httptest.NewRecorder()
			route.ServeHTTP(response, request)
			if response.Code != test.wantStatus || saver.calls != test.wantCalls {
				t.Fatalf("status=%d calls=%d, want status=%d calls=%d",
					response.Code, saver.calls, test.wantStatus, test.wantCalls)
			}
		})
	}
}

func TestCurrentIndexConfigurationRouteRefusesAMalformedBody(t *testing.T) {
	t.Parallel()

	// `reachesTheApplication` is the half that matters: a body the READER
	// cannot decode is refused before the application is asked, while a body
	// that decodes but carries the wrong JSON kind is refused BY the
	// application — the same rule the run path applies to its own tool
	// parameters, rather than a second, drifting copy of it in the handler.
	for name, test := range map[string]struct {
		body                  string
		reachesTheApplication bool
	}{
		"not json":                   {body: `{`},
		"empty":                      {body: ``},
		"no index_configuration key": {body: `{"progress_step":75}`, reachesTheApplication: true},
		"array":                      {body: `{"index_configuration":[1,2]}`, reachesTheApplication: true},
		"bare string":                {body: `{"index_configuration":"progress_step"}`, reachesTheApplication: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			saver := &currentIndexConfigurationStub{
				err: indexmetaapp.ErrCurrentIndexConfigurationInvalid,
			}
			route := newCurrentIndexConfigurationRoute(t, saver,
				grantingResolver(handler.CurrentIndexConfigurationPermission))
			response := httptest.NewRecorder()
			route.ServeHTTP(response, currentIndexConfigurationRequest(
				"/api/v2/elitea_core/index_meta/prompt_lib/7/9/docs/configuration", test.body))
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
			}
			if !strings.Contains(response.Body.String(), "index_configuration must be a JSON object") {
				t.Fatalf("body=%q", response.Body.String())
			}
			wantCalls := 0
			if test.reachesTheApplication {
				wantCalls = 1
			}
			if saver.calls != wantCalls {
				t.Fatalf("saver calls=%d, want %d", saver.calls, wantCalls)
			}
		})
	}
}

// Every application refusal has to arrive as its own status. A save that lost
// the PgVector target and a save against an index that does not exist are
// different problems for the person looking at the screen, and the delete
// route already answers them apart.
func TestCurrentIndexConfigurationRouteMapsApplicationRefusals(t *testing.T) {
	t.Parallel()

	for name, test := range map[string]struct {
		err        error
		wantStatus int
		wantBody   string
	}{
		"toolkit missing": {
			err: indexmetaapp.ErrCurrentIndexMetaToolkitMissing, wantStatus: http.StatusBadRequest,
			wantBody: "Toolkit id is missing for toolkit 9",
		},
		"no pgvector": {
			err: indexmetaapp.ErrCurrentIndexMetaTargetMissing, wantStatus: http.StatusBadRequest,
			wantBody: "PGVector configuration is missing for toolkit 9",
		},
		"no connection string": {
			err: indexmetaapp.ErrCurrentIndexMetaConnectionMissing, wantStatus: http.StatusBadRequest,
			wantBody: "Connection string is missing in PGVector configuration for toolkit 9",
		},
		"index missing": {
			err: indexmetaapp.ErrCurrentIndexMetaNotFound, wantStatus: http.StatusNotFound,
			wantBody: "index_meta not found",
		},
		"storage unavailable": {
			err: indexmetaapp.ErrCurrentIndexConfigurationUnavailable, wantStatus: http.StatusBadGateway,
			wantBody: "Error occurred while saving the index configuration",
		},
		"timed out": {
			err: context.DeadlineExceeded, wantStatus: http.StatusGatewayTimeout,
			wantBody: "Index configuration request timed out",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			saver := &currentIndexConfigurationStub{err: test.err}
			route := newCurrentIndexConfigurationRoute(t, saver,
				grantingResolver(handler.CurrentIndexConfigurationPermission))
			response := httptest.NewRecorder()
			route.ServeHTTP(response, currentIndexConfigurationRequest(
				"/api/v2/elitea_core/index_meta/prompt_lib/7/9/docs/configuration",
				`{"index_configuration":{"progress_step":75}}`,
			))
			if response.Code != test.wantStatus || !strings.Contains(response.Body.String(), test.wantBody) {
				t.Fatalf("status=%d body=%q, want %d containing %q",
					response.Code, response.Body.String(), test.wantStatus, test.wantBody)
			}
			// No refusal may carry a storage detail.
			if strings.Contains(response.Body.String(), "postgres") {
				t.Fatalf("the refusal leaks storage detail: %q", response.Body.String())
			}
		})
	}
}

func TestNewCurrentIndexConfigurationRouteRefusesMissingDependencies(t *testing.T) {
	t.Parallel()

	resolver := grantingResolver(handler.CurrentIndexConfigurationPermission)
	authConfig := apimw.AuthConfig{
		PrincipalValidator: principalValidatorFunc(func(_ context.Context, user auth.User) (auth.User, error) {
			return user, nil
		}),
		ForwardedIdentityVerifier: forwardedPeerVerifierFunc(func(*http.Request) error { return nil }),
	}
	if _, err := handler.NewCurrentIndexConfigurationRoute(nil, authConfig, resolver); !errors.Is(err, handler.ErrInvalidCurrentIndexConfigurationRoute) {
		t.Fatalf("err=%v", err)
	}
	if _, err := handler.NewCurrentIndexConfigurationRoute(&currentIndexConfigurationStub{}, apimw.AuthConfig{}, resolver); !errors.Is(err, handler.ErrInvalidCurrentIndexConfigurationRoute) {
		t.Fatalf("err=%v", err)
	}
	if _, err := handler.NewCurrentIndexConfigurationRoute(&currentIndexConfigurationStub{}, authConfig, nil); !errors.Is(err, handler.ErrInvalidCurrentIndexConfigurationRoute) {
		t.Fatalf("err=%v", err)
	}
	// A nil route answers 404 rather than panicking on a nil receiver.
	var route *handler.CurrentIndexConfigurationRoute
	response := httptest.NewRecorder()
	route.ServeHTTP(response, currentIndexConfigurationRequest("/x", `{}`))
	if response.Code != http.StatusNotFound {
		t.Fatalf("status=%d", response.Code)
	}
}
