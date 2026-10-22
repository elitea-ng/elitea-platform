package social_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware/authtest"
	handler "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/social"
	socialapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/social"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

type currentAuthorsReaderStub struct {
	list  func(context.Context, int32) ([]socialapp.CurrentAuthor, error)
	calls int
}

func (stub *currentAuthorsReaderStub) ListCurrentProjectAuthors(
	ctx context.Context,
	projectID int32,
) ([]socialapp.CurrentAuthor, error) {
	stub.calls++
	if stub.list == nil {
		return nil, nil
	}
	return stub.list(ctx, projectID)
}

type currentAuthorsPrincipalValidatorFunc func(context.Context, auth.User) (auth.User, error)

func (function currentAuthorsPrincipalValidatorFunc) ValidatePrincipal(
	ctx context.Context,
	user auth.User,
) (auth.User, error) {
	return function(ctx, user)
}

type currentAuthorsPermissionResolverFunc func(context.Context, auth.User, string, string) (auth.PermissionResolution, error)

func (function currentAuthorsPermissionResolverFunc) ResolvePermissions(
	ctx context.Context,
	user auth.User,
	mode string,
	projectID string,
) (auth.PermissionResolution, error) {
	return function(ctx, user, mode, projectID)
}

func TestCurrentAuthorsRoutePreservesPathsRBACAndPlainArrayDTO(t *testing.T) {
	if handler.CurrentAuthorsPath != "/api/v2/social/authors/{projectID}" ||
		handler.CurrentAuthorsDefaultPath != "/api/v2/social/authors/default/{projectID}" ||
		handler.CurrentAuthorsMode != auth.PermissionModeDefault ||
		handler.CurrentAuthorsPermission != "models.social.authors.get" {
		t.Fatalf(
			"contract drift paths=(%q,%q) mode=%q permission=%q",
			handler.CurrentAuthorsPath,
			handler.CurrentAuthorsDefaultPath,
			handler.CurrentAuthorsMode,
			handler.CurrentAuthorsPermission,
		)
	}

	email := "member@example.test"
	name := "Member"
	avatar := "avatar-data"
	lastLogin := time.Date(2026, time.July, 27, 11, 12, 13, 987654321, time.FixedZone("source", 2*60*60))
	for _, target := range []string{
		"/api/v2/social/authors/7?limit=5&sort_by=name",
		"/api/v2/social/authors/default/7",
	} {
		t.Run(target, func(t *testing.T) {
			reader := &currentAuthorsReaderStub{
				list: func(ctx context.Context, projectID int32) ([]socialapp.CurrentAuthor, error) {
					if ctx == nil || projectID != 7 {
						t.Fatalf("reader input context=%v project=%d", ctx, projectID)
					}
					return []socialapp.CurrentAuthor{{
						ID:        41,
						Email:     &email,
						Name:      &name,
						LastLogin: &lastLogin,
						Suspended: true,
						Avatar:    &avatar,
					}, {
						ID: 42,
					}}, nil
				},
			}
			permissionCalls := 0
			route := newCurrentAuthorsRoute(
				t,
				reader,
				currentAuthorsPermissionResolverFunc(
					func(_ context.Context, user auth.User, mode, projectID string) (auth.PermissionResolution, error) {
						permissionCalls++
						if user.TokenID != "91" || user.UserID != "41" ||
							mode != auth.PermissionModeDefault || projectID != "7" {
							t.Fatalf("permission input user=%+v mode=%q project=%q", user, mode, projectID)
						}
						return auth.PermissionResolution{
							UserID:      41,
							Permissions: []string{handler.CurrentAuthorsPermission},
						}, nil
					},
				),
				nil,
			)
			response := httptest.NewRecorder()
			route.ServeHTTP(response, currentAuthorsRequest(target))

			const want = `[{"id":41,"email":"member@example.test","name":"Member","last_login":"Mon, 27 Jul 2026 09:12:13 GMT","suspended":true,"avatar":"avatar-data"},{"id":42,"email":null,"name":null,"last_login":null,"suspended":false,"avatar":null}]` + "\n"
			if response.Code != http.StatusOK || response.Body.String() != want ||
				reader.calls != 1 || permissionCalls != 1 ||
				response.Header().Get("Content-Type") != "application/json" {
				t.Fatalf(
					"status=%d reader_calls=%d permission_calls=%d content_type=%q body=%q",
					response.Code,
					reader.calls,
					permissionCalls,
					response.Header().Get("Content-Type"),
					response.Body.String(),
				)
			}
		})
	}
}

func TestCurrentAuthorsRouteReturnsEmptyArrayAndHidesFailures(t *testing.T) {
	reader := &currentAuthorsReaderStub{}
	route := newAuthorizedCurrentAuthorsRoute(t, reader)
	response := httptest.NewRecorder()
	route.ServeHTTP(response, currentAuthorsRequest("/api/v2/social/authors/7"))
	if response.Code != http.StatusOK || response.Body.String() != "[]\n" || reader.calls != 1 {
		t.Fatalf("empty status=%d calls=%d body=%q", response.Code, reader.calls, response.Body.String())
	}

	privateFailure := errors.New("postgres password and private SQL")
	reader = &currentAuthorsReaderStub{
		list: func(context.Context, int32) ([]socialapp.CurrentAuthor, error) {
			return nil, privateFailure
		},
	}
	route = newAuthorizedCurrentAuthorsRoute(t, reader)
	response = httptest.NewRecorder()
	route.ServeHTTP(response, currentAuthorsRequest("/api/v2/social/authors/7"))
	if response.Code != http.StatusInternalServerError ||
		strings.Contains(response.Body.String(), privateFailure.Error()) ||
		response.Body.String() != "{\"error\":\"request failed\"}\n" {
		t.Fatalf("failure status=%d body=%q", response.Code, response.Body.String())
	}
}

func TestCurrentAuthorsRouteDeniesSuspendedPrincipalProjectAndInvalidScope(t *testing.T) {
	// A REFUSAL must carry auth.ErrPermissionDenied. The real resolver
	// (legacyrbac.PostgresResolver) returns that sentinel for a suspended
	// project. Any other error means an infrastructure failure and answers
	// 500, so a plain error here would test the wrong branch.
	privateFailure := fmt.Errorf("suspended project has private permission detail: %w", auth.ErrPermissionDenied)
	for name, test := range map[string]struct {
		target      string
		principal   currentAuthorsPrincipalValidatorFunc
		permissions auth.PermissionResolver
		status      int
	}{
		"suspended principal": {
			target: "/api/v2/social/authors/7",
			principal: func(context.Context, auth.User) (auth.User, error) {
				// The sentinel, not a bare error: only auth.ErrPrincipalInactive
				// is a 401, so a database fault stays a 5xx (#537).
				return auth.User{}, auth.ErrPrincipalInactive
			},
			permissions: authorizedCurrentAuthorsPermissions(),
			status:      http.StatusUnauthorized,
		},
		"suspended project": {
			target:    "/api/v2/social/authors/7",
			principal: activeCurrentAuthorsPrincipal(),
			permissions: currentAuthorsPermissionResolverFunc(
				func(context.Context, auth.User, string, string) (auth.PermissionResolution, error) {
					return auth.PermissionResolution{}, privateFailure
				},
			),
			status: http.StatusForbidden,
		},
		"invalid project": {
			target:      "/api/v2/social/authors/007",
			principal:   activeCurrentAuthorsPrincipal(),
			permissions: authorizedCurrentAuthorsPermissions(),
			status:      http.StatusForbidden,
		},
	} {
		t.Run(name, func(t *testing.T) {
			reader := &currentAuthorsReaderStub{}
			route := newCurrentAuthorsRoute(t, reader, test.permissions, test.principal)
			response := httptest.NewRecorder()
			route.ServeHTTP(response, currentAuthorsRequest(test.target))

			if response.Code != test.status || reader.calls != 0 {
				t.Fatalf("status=%d reader_calls=%d body=%q", response.Code, reader.calls, response.Body.String())
			}
			if strings.Contains(response.Body.String(), handler.CurrentAuthorsPermission) ||
				strings.Contains(response.Body.String(), privateFailure.Error()) {
				t.Fatalf("denial leaked authorization detail: %q", response.Body.String())
			}
		})
	}
}

func TestNewCurrentAuthorsRouteRejectsIncompleteSecurityAndUnsupportedOperations(t *testing.T) {
	reader := &currentAuthorsReaderStub{}
	authConfig := currentAuthorsAuthConfig(activeCurrentAuthorsPrincipal())
	permissions := authorizedCurrentAuthorsPermissions()
	for name, test := range map[string]struct {
		reader      handler.CurrentAuthorsReader
		authConfig  apimw.AuthConfig
		permissions auth.PermissionResolver
	}{
		"missing reader":      {authConfig: authConfig, permissions: permissions},
		"missing validator":   {reader: reader, authConfig: apimw.AuthConfig{FormSessions: authtest.FormSessions{}}, permissions: permissions},
		"missing verifier":    {reader: reader, authConfig: apimw.AuthConfig{PrincipalValidator: authConfig.PrincipalValidator}, permissions: permissions},
		"missing permissions": {reader: reader, authConfig: authConfig},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := handler.NewCurrentAuthorsRoute(test.reader, test.authConfig, test.permissions); !errors.Is(err, handler.ErrInvalidCurrentAuthorsRoute) {
				t.Fatalf("error=%v want=%v", err, handler.ErrInvalidCurrentAuthorsRoute)
			}
		})
	}

	route := newAuthorizedCurrentAuthorsRoute(t, reader)
	for name, request := range map[string]*http.Request{
		"post":           currentAuthorsRequest("/api/v2/social/authors/7"),
		"administration": currentAuthorsRequest("/api/v2/social/authors/administration/7"),
	} {
		t.Run(name, func(t *testing.T) {
			if name == "post" {
				request.Method = http.MethodPost
			}
			response := httptest.NewRecorder()
			route.ServeHTTP(response, request)
			if response.Code != http.StatusMethodNotAllowed && response.Code != http.StatusNotFound {
				t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
			}
		})
	}
}

func newAuthorizedCurrentAuthorsRoute(
	t *testing.T,
	reader handler.CurrentAuthorsReader,
) *handler.CurrentAuthorsRoute {
	t.Helper()
	return newCurrentAuthorsRoute(
		t,
		reader,
		authorizedCurrentAuthorsPermissions(),
		activeCurrentAuthorsPrincipal(),
	)
}

func newCurrentAuthorsRoute(
	t *testing.T,
	reader handler.CurrentAuthorsReader,
	permissions auth.PermissionResolver,
	principal currentAuthorsPrincipalValidatorFunc,
) *handler.CurrentAuthorsRoute {
	t.Helper()
	if principal == nil {
		principal = activeCurrentAuthorsPrincipal()
	}
	route, err := handler.NewCurrentAuthorsRoute(
		reader,
		currentAuthorsAuthConfig(principal),
		permissions,
	)
	if err != nil {
		t.Fatal(err)
	}
	return route
}

func currentAuthorsAuthConfig(
	principal currentAuthorsPrincipalValidatorFunc,
) apimw.AuthConfig {
	return apimw.AuthConfig{
		PrincipalValidator: principal,
		FormSessions:       authtest.FormSessions{},
		Validator:          authtest.Tokens{},
	}
}

func activeCurrentAuthorsPrincipal() currentAuthorsPrincipalValidatorFunc {
	return func(_ context.Context, user auth.User) (auth.User, error) {
		return user, nil
	}
}

func authorizedCurrentAuthorsPermissions() auth.PermissionResolver {
	return currentAuthorsPermissionResolverFunc(
		func(context.Context, auth.User, string, string) (auth.PermissionResolution, error) {
			return auth.PermissionResolution{
				UserID:      41,
				Permissions: []string{handler.CurrentAuthorsPermission},
			}, nil
		},
	)
}

func currentAuthorsRequest(target string) *http.Request {
	request := httptest.NewRequest(http.MethodGet, target, nil)
	request.RemoteAddr = "10.0.0.8:43120"
	authtest.BearerToken(request, "91", "41")
	return request
}
