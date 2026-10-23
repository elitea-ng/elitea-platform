package indexing_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	apimw "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware/authtest"
	handler "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/indexing"
	indexingapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/indexing"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

const currentGoIndexExecutionID = "0123456789abcdef0123456789abcdef"

func TestCurrentIndexCancelRouteBindsCurrentContractAndSecurityBeforeUseCase(t *testing.T) {
	t.Parallel()

	if handler.CurrentIndexCancelPath != "/api/v2/elitea_core/index_cancel/prompt_lib/{projectID}/{toolkitID}/{indexName}/{taskID}" ||
		handler.CurrentIndexCancelPermission != "models.applications.task.delete" ||
		handler.CurrentIndexCancelMode != auth.PermissionModeDefault {
		t.Fatalf(
			"current cancel constants drifted: path=%q permission=%q mode=%q",
			handler.CurrentIndexCancelPath,
			handler.CurrentIndexCancelPermission,
			handler.CurrentIndexCancelMode,
		)
	}

	canceller := &currentIndexCancellerStub{transitioned: true}
	permissionCalls := 0
	route := currentIndexCancelRouteForTest(
		t,
		canceller,
		func(_ context.Context, user auth.User, mode, projectID string) (auth.PermissionResolution, error) {
			permissionCalls++
			if user.UserID != "11" || mode != auth.PermissionModeDefault || projectID != "7" {
				t.Fatalf("permission input user=%+v mode=%q project=%q", user, mode, projectID)
			}
			return auth.PermissionResolution{
				UserID:      11,
				Permissions: []string{"models.applications.task.delete"},
			}, nil
		},
	)

	response := serveCurrentIndexCancel(
		route,
		http.MethodDelete,
		"/api/v2/elitea_core/index_cancel/prompt_lib/007/9/documents/"+currentGoIndexExecutionID,
		"10.0.0.8:43120",
	)
	if response.Code != http.StatusNoContent || response.Body.Len() != 0 ||
		permissionCalls != 1 || canceller.calls != 1 {
		t.Fatalf(
			"status=%d body=%q permissions=%d cancels=%d",
			response.Code,
			response.Body.String(),
			permissionCalls,
			canceller.calls,
		)
	}
	want := indexingapp.CurrentIndexCancelRequest{
		ProjectID:   7,
		ToolkitID:   9,
		IndexName:   "documents",
		ExecutionID: currentGoIndexExecutionID,
	}
	if canceller.request != want {
		t.Fatalf("cancel request=%+v want=%+v", canceller.request, want)
	}
}

func TestCurrentIndexCancelRouteRejectsUntrustedUnauthorizedAndInvalidRequests(t *testing.T) {
	t.Parallel()

	for name, test := range map[string]struct {
		remoteAddress string
		permissions   []string
		path          string
		wantStatus    int
	}{
		"forged X-Auth-* identity without a session": {
			remoteAddress: forgedIdentityAddress,
			permissions:   []string{"models.applications.task.delete"},
			path:          "/api/v2/elitea_core/index_cancel/prompt_lib/7/9/documents/" + currentGoIndexExecutionID,
			wantStatus:    http.StatusUnauthorized,
		},
		"permission denied": {
			remoteAddress: "10.0.0.8:43120",
			path:          "/api/v2/elitea_core/index_cancel/prompt_lib/7/9/documents/" + currentGoIndexExecutionID,
			wantStatus:    http.StatusForbidden,
		},
		"arbiter UUID": {
			remoteAddress: "10.0.0.8:43120",
			permissions:   []string{"models.applications.task.delete"},
			path:          "/api/v2/elitea_core/index_cancel/prompt_lib/7/9/documents/01234567-89ab-cdef-0123-456789abcdef",
			wantStatus:    http.StatusBadRequest,
		},
		"wrong execution case": {
			remoteAddress: "10.0.0.8:43120",
			permissions:   []string{"models.applications.task.delete"},
			path:          "/api/v2/elitea_core/index_cancel/prompt_lib/7/9/documents/0123456789ABCDEF0123456789abcdef",
			wantStatus:    http.StatusBadRequest,
		},
		"invalid toolkit": {
			remoteAddress: "10.0.0.8:43120",
			permissions:   []string{"models.applications.task.delete"},
			path:          "/api/v2/elitea_core/index_cancel/prompt_lib/7/no/documents/" + currentGoIndexExecutionID,
			wantStatus:    http.StatusBadRequest,
		},
	} {
		name, test := name, test
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			canceller := &currentIndexCancellerStub{}
			route := currentIndexCancelRouteForTest(
				t,
				canceller,
				func(context.Context, auth.User, string, string) (auth.PermissionResolution, error) {
					return auth.PermissionResolution{UserID: 11, Permissions: test.permissions}, nil
				},
			)
			response := serveCurrentIndexCancel(route, http.MethodDelete, test.path, test.remoteAddress)
			if response.Code != test.wantStatus {
				t.Fatalf("status=%d want=%d body=%s", response.Code, test.wantStatus, response.Body.String())
			}
			if canceller.calls != 0 {
				t.Fatalf("rejected request reached use case %d times", canceller.calls)
			}
		})
	}
}

func TestCurrentIndexCancelRouteReturns204ForNoTransition(t *testing.T) {
	t.Parallel()

	canceller := &currentIndexCancellerStub{}
	route := currentIndexCancelRouteForTest(
		t,
		canceller,
		func(context.Context, auth.User, string, string) (auth.PermissionResolution, error) {
			return auth.PermissionResolution{
				UserID:      11,
				Permissions: []string{"models.applications.task.delete"},
			}, nil
		},
	)
	response := serveCurrentIndexCancel(
		route,
		http.MethodDelete,
		"/api/v2/elitea_core/index_cancel/prompt_lib/7/9/documents/"+currentGoIndexExecutionID,
		"10.0.0.8:43120",
	)
	if response.Code != http.StatusNoContent || response.Body.Len() != 0 || canceller.calls != 1 {
		t.Fatalf("status=%d body=%q calls=%d", response.Code, response.Body.String(), canceller.calls)
	}
}

func TestCurrentIndexCancelRouteRejectsIncompleteCompositionAndOtherMethods(t *testing.T) {
	t.Parallel()

	canceller := &currentIndexCancellerStub{}
	principal := principalValidatorFunc(func(_ context.Context, user auth.User) (auth.User, error) {
		return user, nil
	})
	permissions := permissionResolverFunc(func(context.Context, auth.User, string, string) (auth.PermissionResolution, error) {
		return auth.PermissionResolution{}, nil
	})
	for name, test := range map[string]struct {
		canceller   handler.CurrentIndexCanceller
		authConfig  apimw.AuthConfig
		permissions auth.PermissionResolver
	}{
		"missing use case":    {authConfig: apimw.AuthConfig{PrincipalValidator: principal, FormSessions: authtest.FormSessions{}}, permissions: permissions},
		"missing principal":   {canceller: canceller, authConfig: apimw.AuthConfig{FormSessions: authtest.FormSessions{}}, permissions: permissions},
		"missing peer proof":  {canceller: canceller, authConfig: apimw.AuthConfig{PrincipalValidator: principal}, permissions: permissions},
		"missing permissions": {canceller: canceller, authConfig: apimw.AuthConfig{PrincipalValidator: principal, FormSessions: authtest.FormSessions{}}},
	} {
		name, test := name, test
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := handler.NewCurrentIndexCancelRoute(test.canceller, test.authConfig, test.permissions); !errors.Is(err, handler.ErrInvalidCurrentIndexCancelRoute) {
				t.Fatalf("error=%v want=%v", err, handler.ErrInvalidCurrentIndexCancelRoute)
			}
		})
	}

	route := currentIndexCancelRouteForTest(
		t,
		canceller,
		func(context.Context, auth.User, string, string) (auth.PermissionResolution, error) {
			return auth.PermissionResolution{
				UserID:      11,
				Permissions: []string{"models.applications.task.delete"},
			}, nil
		},
	)
	response := serveCurrentIndexCancel(
		route,
		http.MethodGet,
		"/api/v2/elitea_core/index_cancel/prompt_lib/7/9/documents/"+currentGoIndexExecutionID,
		"10.0.0.8:43120",
	)
	if response.Code != http.StatusMethodNotAllowed || canceller.calls != 0 {
		t.Fatalf("GET status=%d calls=%d", response.Code, canceller.calls)
	}
}

func currentIndexCancelRouteForTest(
	t *testing.T,
	canceller handler.CurrentIndexCanceller,
	resolvePermissions func(context.Context, auth.User, string, string) (auth.PermissionResolution, error),
) *handler.CurrentIndexCancelRoute {
	t.Helper()
	route, err := handler.NewCurrentIndexCancelRoute(
		canceller,
		apimw.AuthConfig{
			PrincipalValidator: principalValidatorFunc(func(_ context.Context, user auth.User) (auth.User, error) {
				return user, nil
			}),
			FormSessions: authtest.FormSessions{},
		},
		permissionResolverFunc(resolvePermissions),
	)
	if err != nil {
		t.Fatal(err)
	}
	return route
}

// forgedIdentityAddress makes serveCurrentIndexCancel send the forged X-Auth-*
// identity instead of a Form session.
const forgedIdentityAddress = "192.0.2.9:443"

func serveCurrentIndexCancel(
	route http.Handler,
	method string,
	path string,
	remoteAddress string,
) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, nil)
	if remoteAddress == forgedIdentityAddress {
		// The retired edge projection, and no session (#1359).
		authtest.ForgeXAuth(request, "11")
	} else {
		authtest.SignIn(request, "11")
	}
	request.RemoteAddr = remoteAddress
	response := httptest.NewRecorder()
	route.ServeHTTP(response, request)
	return response
}

type currentIndexCancellerStub struct {
	request      indexingapp.CurrentIndexCancelRequest
	transitioned bool
	err          error
	calls        int
}

func (s *currentIndexCancellerStub) Cancel(
	_ context.Context,
	request indexingapp.CurrentIndexCancelRequest,
) (bool, error) {
	s.calls++
	s.request = request
	return s.transitioned, s.err
}
