package projects_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	handler "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/projects"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
)

type principalValidatorFunc func(context.Context, auth.User) (auth.User, error)

func (f principalValidatorFunc) ValidatePrincipal(ctx context.Context, principal auth.User) (auth.User, error) {
	return f(ctx, principal)
}

type forwardedPeerVerifierFunc func(*http.Request) error

func (f forwardedPeerVerifierFunc) VerifyForwardedIdentityPeer(request *http.Request) error {
	return f(request)
}

type permissionResolverFunc func(context.Context, auth.User, string, string) (auth.PermissionResolution, error)

func (f permissionResolverFunc) ResolvePermissions(
	ctx context.Context,
	principal auth.User,
	mode string,
	projectID string,
) (auth.PermissionResolution, error) {
	return f(ctx, principal, mode, projectID)
}

// membershipResolverFunc is the gate the project-list route uses since #830:
// it is asked for the caller's permissions across their OWN memberships, and
// no project id travels with the question.
type membershipResolverFunc func(context.Context, auth.User, string) (auth.PermissionResolution, error)

func (f membershipResolverFunc) ResolveMembershipPermissions(
	ctx context.Context,
	principal auth.User,
	mode string,
) (auth.PermissionResolution, error) {
	return f(ctx, principal, mode)
}

func TestNewCurrentProjectListRouteRejectsIncompleteSecurityComposition(t *testing.T) {
	store := projectListerFunc(func(context.Context, sqlcgen.ListCurrentUserProjectsParams) ([]sqlcgen.ListCurrentUserProjectsRow, error) {
		return nil, nil
	})
	principal := principalValidatorFunc(func(_ context.Context, user auth.User) (auth.User, error) { return user, nil })
	peer := forwardedPeerVerifierFunc(func(*http.Request) error { return nil })
	permissions := membershipResolverFunc(func(context.Context, auth.User, string) (auth.PermissionResolution, error) {
		return auth.PermissionResolution{}, nil
	})

	for name, test := range map[string]struct {
		store       handler.CurrentProjectLister
		authConfig  apimw.AuthConfig
		permissions auth.MembershipPermissionResolver
	}{
		// Only store and permissions are required; PrincipalValidator and
		// ForwardedIdentityVerifier are optional (OIDC-only deployments use
		// session-cookie auth without either).
		"missing query":       {authConfig: apimw.AuthConfig{PrincipalValidator: principal, ForwardedIdentityVerifier: peer}, permissions: permissions},
		"missing permissions": {store: store, authConfig: apimw.AuthConfig{PrincipalValidator: principal, ForwardedIdentityVerifier: peer}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := handler.NewCurrentProjectListRoute(test.store, test.authConfig, test.permissions); !errors.Is(err, handler.ErrInvalidCurrentProjectListRoute) {
				t.Fatalf("error = %v, want %v", err, handler.ErrInvalidCurrentProjectListRoute)
			}
		})
	}
}

func TestCurrentProjectListRouteValidatesPeerPrincipalRBACBeforeQuery(t *testing.T) {
	queryCalls := 0
	store := projectListerFunc(func(_ context.Context, params sqlcgen.ListCurrentUserProjectsParams) ([]sqlcgen.ListCurrentUserProjectsRow, error) {
		queryCalls++
		if params.UserID != 7 {
			t.Fatalf("query user = %d, want 7", params.UserID)
		}
		return []sqlcgen.ListCurrentUserProjectsRow{}, nil
	})
	principal := principalValidatorFunc(func(_ context.Context, user auth.User) (auth.User, error) {
		if user.ID != "7" || user.UserID != "7" || user.AuthType != "user" {
			t.Fatalf("forwarded principal = %+v", user)
		}
		user.Email = "server-derived@example.test"
		return user, nil
	})
	peer := forwardedPeerVerifierFunc(func(request *http.Request) error {
		if request.RemoteAddr != "10.0.0.8:43120" {
			return errors.New("untrusted peer")
		}
		return nil
	})
	permissions := membershipResolverFunc(func(_ context.Context, user auth.User, mode string) (auth.PermissionResolution, error) {
		if user.Email != "server-derived@example.test" || mode != handler.CurrentProjectListMode {
			t.Fatalf("permission input = user=%+v mode=%q", user, mode)
		}
		return auth.PermissionResolution{
			UserID:      7,
			Permissions: []string{handler.CurrentProjectListPermission},
		}, nil
	})
	route, err := handler.NewCurrentProjectListRoute(store, apimw.AuthConfig{
		PrincipalValidator:        principal,
		ForwardedIdentityVerifier: peer,
	}, permissions)
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodGet, handler.CurrentProjectListPath, nil)
	request.RemoteAddr = "10.0.0.8:43120"
	request.Header.Set("X-Auth-Type", "user")
	request.Header.Set("X-Auth-ID", "7")
	recorder := httptest.NewRecorder()

	route.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK || queryCalls != 1 {
		t.Fatalf("status=%d query calls=%d body=%s", recorder.Code, queryCalls, recorder.Body.String())
	}
}

func TestCurrentProjectListRouteRejectsForgedPeerAndMissingPermission(t *testing.T) {
	queryCalls := 0
	store := projectListerFunc(func(context.Context, sqlcgen.ListCurrentUserProjectsParams) ([]sqlcgen.ListCurrentUserProjectsRow, error) {
		queryCalls++
		return nil, nil
	})
	principal := principalValidatorFunc(func(_ context.Context, user auth.User) (auth.User, error) { return user, nil })
	peer := forwardedPeerVerifierFunc(func(request *http.Request) error {
		if request.RemoteAddr != "10.0.0.8:43120" {
			return errors.New("untrusted peer")
		}
		return nil
	})
	permissions := membershipResolverFunc(func(context.Context, auth.User, string) (auth.PermissionResolution, error) {
		return auth.PermissionResolution{UserID: 7, Permissions: []string{}}, nil
	})
	route, err := handler.NewCurrentProjectListRoute(store, apimw.AuthConfig{
		PrincipalValidator:        principal,
		ForwardedIdentityVerifier: peer,
	}, permissions)
	if err != nil {
		t.Fatal(err)
	}

	for name, peerAddress := range map[string]string{
		"forged untrusted peer": "192.0.2.10:443",
		"trusted but denied":    "10.0.0.8:43120",
	} {
		t.Run(name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, handler.CurrentProjectListPath, nil)
			request.RemoteAddr = peerAddress
			request.Header.Set("X-Auth-Type", "user")
			request.Header.Set("X-Auth-ID", "7")
			recorder := httptest.NewRecorder()

			route.ServeHTTP(recorder, request)

			if recorder.Code != http.StatusUnauthorized && recorder.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want authentication/permission denial", recorder.Code)
			}
		})
	}
	if queryCalls != 0 {
		t.Fatalf("project query calls = %d, want 0 before authorization", queryCalls)
	}
}
