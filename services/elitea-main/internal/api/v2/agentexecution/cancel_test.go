package agentexecution

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

type currentAgentCancellerStub struct {
	request agentexecutionapp.CurrentAgentCancelRequest
	outcome agentexecutionapp.CurrentAgentCancelOutcome
	err     error
	calls   int
}

func (stub *currentAgentCancellerStub) Cancel(
	_ context.Context,
	request agentexecutionapp.CurrentAgentCancelRequest,
) (agentexecutionapp.CurrentAgentCancelOutcome, error) {
	stub.calls++
	stub.request = request
	return stub.outcome, stub.err
}

func TestCurrentAgentCancelRoutePreservesContractAndSecurityBeforeUseCase(t *testing.T) {
	if CurrentAgentCancelPath != "/api/v2/elitea_core/task/prompt_lib/{projectID}/{responseMessageID}" ||
		CurrentAgentCancelPermission != "models.chat.task.delete" ||
		CurrentAgentCancelMode != auth.PermissionModeDefault {
		t.Fatalf("path=%q permission=%q mode=%q", CurrentAgentCancelPath, CurrentAgentCancelPermission, CurrentAgentCancelMode)
	}
	canceller := &currentAgentCancellerStub{outcome: agentexecutionapp.CurrentAgentCancelOutcome{Salvaged: true}}
	permissionCalls := 0
	permissions := currentStartPermissionResolverFunc(func(
		_ context.Context,
		user auth.User,
		mode,
		projectID string,
	) (auth.PermissionResolution, error) {
		permissionCalls++
		if user.UserID != "11" || mode != auth.PermissionModeDefault || projectID != "7" {
			t.Fatalf("user=%+v mode=%q project=%q", user, mode, projectID)
		}
		return auth.PermissionResolution{UserID: 11, Permissions: []string{CurrentAgentCancelPermission}}, nil
	})
	route := newCurrentAgentCancelRoute(t, canceller, permissions)
	response := httptest.NewRecorder()
	route.ServeHTTP(response, currentAgentCancelRequest(http.MethodDelete, "7", "10000000-0000-4000-8000-000000000041"))
	if response.Code != http.StatusNoContent || response.Body.Len() != 0 || permissionCalls != 1 || canceller.calls != 1 {
		t.Fatalf("status=%d body=%q permissions=%d calls=%d", response.Code, response.Body.String(), permissionCalls, canceller.calls)
	}
	want := agentexecutionapp.CurrentAgentCancelRequest{
		ProjectID: 7, ActorUserID: 11,
		ResponseMessageID: "10000000-0000-4000-8000-000000000041",
	}
	if canceller.request != want {
		t.Fatalf("request=%+v want=%+v", canceller.request, want)
	}
}

func TestCurrentAgentCancelRouteRejectsInvalidForbiddenAndNonOwner(t *testing.T) {
	for _, test := range []struct {
		name        string
		projectID   string
		responseID  string
		permissions []string
		cancelErr   error
		wantStatus  int
		wantBody    string
	}{
		{name: "invalid project", projectID: "no", responseID: "10000000-0000-4000-8000-000000000042", permissions: []string{CurrentAgentCancelPermission}, wantStatus: http.StatusForbidden},
		{name: "invalid response", projectID: "7", responseID: "not-a-uuid", permissions: []string{CurrentAgentCancelPermission}, wantStatus: http.StatusBadRequest},
		{name: "permission denied", projectID: "7", responseID: "10000000-0000-4000-8000-000000000042", wantStatus: http.StatusForbidden},
		{name: "inactive stale or not author", projectID: "7", responseID: "10000000-0000-4000-8000-000000000042", permissions: []string{CurrentAgentCancelPermission}, cancelErr: agentexecutionapp.ErrCurrentAgentCancelNotAllowed, wantStatus: http.StatusConflict, wantBody: "Message is not active or cannot be stopped by this user"},
	} {
		t.Run(test.name, func(t *testing.T) {
			canceller := &currentAgentCancellerStub{err: test.cancelErr}
			permissions := currentStartPermissionResolverFunc(func(
				context.Context,
				auth.User,
				string,
				string,
			) (auth.PermissionResolution, error) {
				return auth.PermissionResolution{UserID: 11, Permissions: test.permissions}, nil
			})
			route := newCurrentAgentCancelRoute(t, canceller, permissions)
			response := httptest.NewRecorder()
			route.ServeHTTP(response, currentAgentCancelRequest(http.MethodDelete, test.projectID, test.responseID))
			if response.Code != test.wantStatus {
				t.Fatalf("status=%d want=%d body=%q", response.Code, test.wantStatus, response.Body.String())
			}
			if test.wantBody != "" && !strings.Contains(response.Body.String(), test.wantBody) {
				t.Fatalf("body=%q does not contain %q", response.Body.String(), test.wantBody)
			}
			if test.cancelErr == nil && canceller.calls != 0 {
				t.Fatalf("rejected request reached canceller %d times", canceller.calls)
			}
		})
	}
}

func TestCurrentAgentCancelRouteReturns204ForReplayAndRejectsOtherMethods(t *testing.T) {
	canceller := &currentAgentCancellerStub{outcome: agentexecutionapp.CurrentAgentCancelOutcome{Replay: true}}
	route := newCurrentAgentCancelRoute(t, canceller, currentStartPermissionResolverFunc(func(
		context.Context,
		auth.User,
		string,
		string,
	) (auth.PermissionResolution, error) {
		return auth.PermissionResolution{UserID: 11, Permissions: []string{CurrentAgentCancelPermission}}, nil
	}))
	response := httptest.NewRecorder()
	route.ServeHTTP(response, currentAgentCancelRequest(http.MethodDelete, "7", "10000000-0000-4000-8000-000000000043"))
	if response.Code != http.StatusNoContent || response.Body.Len() != 0 {
		t.Fatalf("replay status=%d body=%q", response.Code, response.Body.String())
	}
	response = httptest.NewRecorder()
	route.ServeHTTP(response, currentAgentCancelRequest(http.MethodGet, "7", "10000000-0000-4000-8000-000000000043"))
	if response.Code != http.StatusMethodNotAllowed || canceller.calls != 1 {
		t.Fatalf("GET status=%d calls=%d", response.Code, canceller.calls)
	}
}

func TestCurrentAgentCancelRouteRejectsIncompleteComposition(t *testing.T) {
	principal := currentStartPrincipalValidatorFunc(func(_ context.Context, user auth.User) (auth.User, error) { return user, nil })
	peer := currentStartPeerVerifierFunc(func(*http.Request) error { return nil })
	permissions := currentStartPermissionResolverFunc(func(context.Context, auth.User, string, string) (auth.PermissionResolution, error) {
		return auth.PermissionResolution{}, nil
	})
	for name, test := range map[string]struct {
		canceller   CurrentAgentCanceller
		authConfig  apimw.AuthConfig
		permissions auth.PermissionResolver
	}{
		"missing canceller":  {authConfig: apimw.AuthConfig{PrincipalValidator: principal, ForwardedIdentityVerifier: peer}, permissions: permissions},
		"missing principal":  {canceller: &currentAgentCancellerStub{}, authConfig: apimw.AuthConfig{ForwardedIdentityVerifier: peer}, permissions: permissions},
		"missing peer":       {canceller: &currentAgentCancellerStub{}, authConfig: apimw.AuthConfig{PrincipalValidator: principal}, permissions: permissions},
		"missing permission": {canceller: &currentAgentCancellerStub{}, authConfig: apimw.AuthConfig{PrincipalValidator: principal, ForwardedIdentityVerifier: peer}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewCurrentAgentCancelRoute(test.canceller, test.authConfig, test.permissions); !errors.Is(err, ErrInvalidCurrentAgentCancelRoute) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func newCurrentAgentCancelRoute(
	t *testing.T,
	canceller CurrentAgentCanceller,
	permissions auth.PermissionResolver,
) *CurrentAgentCancelRoute {
	t.Helper()
	route, err := NewCurrentAgentCancelRoute(
		canceller,
		apimw.AuthConfig{
			PrincipalValidator: currentStartPrincipalValidatorFunc(func(_ context.Context, user auth.User) (auth.User, error) { return user, nil }),
			ForwardedIdentityVerifier: currentStartPeerVerifierFunc(func(request *http.Request) error {
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

func currentAgentCancelRequest(method, projectID, responseID string) *http.Request {
	request := httptest.NewRequest(method, "/api/v2/elitea_core/task/prompt_lib/"+projectID+"/"+responseID, nil)
	request.Header.Set("X-Auth-Type", "user")
	request.Header.Set("X-Auth-ID", "11")
	request.RemoteAddr = "10.0.0.8:43120"
	return request
}

// The path parameter is declared `format: uuid`, which admits either case. A
// client that re-serialises the id it was given (Swift's uuidString is
// upper-case) must still stop its run, and the use case must see the same
// canonical spelling the server stored, so a repeated stop replays.
func TestCurrentAgentCancelRouteCanonicalisesAnUpperCaseResponseID(t *testing.T) {
	canceller := &currentAgentCancellerStub{}
	route := newCurrentAgentCancelRoute(t, canceller, currentStartPermissionResolverFunc(func(
		context.Context,
		auth.User,
		string,
		string,
	) (auth.PermissionResolution, error) {
		return auth.PermissionResolution{UserID: 11, Permissions: []string{CurrentAgentCancelPermission}}, nil
	}))
	response := httptest.NewRecorder()
	route.ServeHTTP(response, currentAgentCancelRequest(http.MethodDelete, "7", "1000000A-0000-4000-8000-00000000004B"))
	if response.Code != http.StatusNoContent || canceller.calls != 1 {
		t.Fatalf("status=%d body=%q calls=%d", response.Code, response.Body.String(), canceller.calls)
	}
	if canceller.request.ResponseMessageID != "1000000a-0000-4000-8000-00000000004b" {
		t.Fatalf("response message id=%q, want the lowercase canonical spelling", canceller.request.ResponseMessageID)
	}
	for _, malformed := range []string{"{1000000a-0000-4000-8000-00000000004b}", "1000000a00004000800000000000004b"} {
		response = httptest.NewRecorder()
		route.ServeHTTP(response, currentAgentCancelRequest(http.MethodDelete, "7", malformed))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("%q status=%d, want 400", malformed, response.Code)
		}
	}
}
