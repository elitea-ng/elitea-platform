package agentexecution

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

type currentAgentTaskStatusStub struct {
	request agentexecutionapp.CurrentAgentTaskStatusRequest
	outcome agentexecutionapp.CurrentAgentTaskStatusOutcome
	err     error
	calls   int
}

func (stub *currentAgentTaskStatusStub) Status(
	_ context.Context,
	request agentexecutionapp.CurrentAgentTaskStatusRequest,
) (agentexecutionapp.CurrentAgentTaskStatusOutcome, error) {
	stub.calls++
	stub.request = request
	return stub.outcome, stub.err
}

// TestCurrentApplicationTaskRoutePinsTheLegacyContract fixes the path and the
// TWO permissions. The verb split is legacy's own (application_task.py), and a
// single permission for both verbs would let a caller who may only WATCH a run
// stop it.
func TestCurrentApplicationTaskRoutePinsTheLegacyContract(t *testing.T) {
	if CurrentApplicationTaskPath !=
		"/api/v2/elitea_core/application_task/prompt_lib/{projectID}/{responseMessageID}" ||
		CurrentApplicationTaskStatusPermission != "models.applications.task.get" ||
		CurrentApplicationTaskCancelPermission != "models.applications.task.delete" ||
		CurrentApplicationTaskMode != auth.PermissionModeDefault {
		t.Fatalf("path=%q status=%q cancel=%q mode=%q",
			CurrentApplicationTaskPath,
			CurrentApplicationTaskStatusPermission,
			CurrentApplicationTaskCancelPermission,
			CurrentApplicationTaskMode)
	}
}

func TestCurrentApplicationTaskRouteServesStatusAndForwardsTheResolvedActor(t *testing.T) {
	settledAt := time.Date(2026, 9, 7, 10, 30, 0, 0, time.UTC)
	reader := &currentAgentTaskStatusStub{
		outcome: agentexecutionapp.CurrentAgentTaskStatusOutcome{
			Status:    agentexecutionapp.CurrentAgentTaskStatusStopped,
			Stopping:  false,
			SettledAt: &settledAt,
			ErrorCode: "DEADLINE_EXCEEDED",
		},
	}
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
		return auth.PermissionResolution{
			UserID:      11,
			Permissions: []string{CurrentApplicationTaskStatusPermission},
		}, nil
	})
	route := newCurrentApplicationTaskRoute(t, reader, &currentAgentCancellerStub{}, permissions)

	response := httptest.NewRecorder()
	route.ServeHTTP(response, currentApplicationTaskRequest(
		http.MethodGet, "7", "10000000-0000-4000-8000-000000000051", ""))

	if response.Code != http.StatusOK || permissionCalls != 1 || reader.calls != 1 {
		t.Fatalf("status=%d permissions=%d calls=%d body=%q",
			response.Code, permissionCalls, reader.calls, response.Body.String())
	}
	want := agentexecutionapp.CurrentAgentTaskStatusRequest{
		ProjectID: 7, ActorUserID: 11,
		ResponseMessageID: "10000000-0000-4000-8000-000000000051",
	}
	if reader.request != want {
		t.Fatalf("request=%+v want=%+v", reader.request, want)
	}
	var body struct {
		Status    string     `json:"status"`
		Stopping  bool       `json:"stopping"`
		SettledAt *time.Time `json:"settled_at"`
		ErrorCode string     `json:"error_code"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Status != "stopped" || body.Stopping ||
		body.SettledAt == nil || !body.SettledAt.Equal(settledAt) ||
		body.ErrorCode != "DEADLINE_EXCEEDED" {
		t.Fatalf("body=%+v", body)
	}
}

// TestCurrentApplicationTaskRouteSeparatesTheTwoPermissions is the reason the
// two gates are built separately. A caller holding only the read permission
// must not be able to STOP the run, and a caller holding only the delete
// permission must not be able to poll it.
func TestCurrentApplicationTaskRouteSeparatesTheTwoPermissions(t *testing.T) {
	for _, test := range []struct {
		name       string
		method     string
		granted    []string
		wantStatus int
	}{
		{
			name:   "reader may not stop",
			method: http.MethodDelete,
			granted: []string{
				CurrentApplicationTaskStatusPermission,
			},
			wantStatus: http.StatusForbidden,
		},
		{
			name:   "canceller may not poll",
			method: http.MethodGet,
			granted: []string{
				CurrentApplicationTaskCancelPermission,
			},
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "no permission at all",
			method:     http.MethodGet,
			granted:    nil,
			wantStatus: http.StatusForbidden,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader := &currentAgentTaskStatusStub{}
			canceller := &currentAgentCancellerStub{}
			route := newCurrentApplicationTaskRoute(t, reader, canceller,
				currentStartPermissionResolverFunc(func(
					context.Context, auth.User, string, string,
				) (auth.PermissionResolution, error) {
					return auth.PermissionResolution{UserID: 11, Permissions: test.granted}, nil
				}))
			response := httptest.NewRecorder()
			route.ServeHTTP(response, currentApplicationTaskRequest(
				test.method, "7", "10000000-0000-4000-8000-000000000052", ""))
			if response.Code != test.wantStatus {
				t.Fatalf("status=%d want=%d body=%q",
					response.Code, test.wantStatus, response.Body.String())
			}
			if reader.calls != 0 || canceller.calls != 0 {
				t.Fatalf("refused request reached the use case: reader=%d canceller=%d",
					reader.calls, canceller.calls)
			}
		})
	}
}

// TestCurrentApplicationTaskRouteRefusesAnotherProject proves the {projectID}
// segment is not an authorization claim. The caller is fully entitled in
// project 7 and holds nothing in project 8; only the segment changes.
func TestCurrentApplicationTaskRouteRefusesAnotherProject(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			reader := &currentAgentTaskStatusStub{}
			canceller := &currentAgentCancellerStub{}
			route := newCurrentApplicationTaskRoute(t, reader, canceller,
				currentStartPermissionResolverFunc(func(
					_ context.Context, _ auth.User, _, projectID string,
				) (auth.PermissionResolution, error) {
					if projectID != "7" {
						return auth.PermissionResolution{UserID: 11}, nil
					}
					return auth.PermissionResolution{UserID: 11, Permissions: []string{
						CurrentApplicationTaskStatusPermission,
						CurrentApplicationTaskCancelPermission,
					}}, nil
				}))
			response := httptest.NewRecorder()
			route.ServeHTTP(response, currentApplicationTaskRequest(
				method, "8", "10000000-0000-4000-8000-000000000053", ""))
			if response.Code != http.StatusForbidden {
				t.Fatalf("status=%d want=403 body=%q", response.Code, response.Body.String())
			}
			if reader.calls != 0 || canceller.calls != 0 {
				t.Fatalf("cross-project request reached the use case: reader=%d canceller=%d",
					reader.calls, canceller.calls)
			}
		})
	}
}

// TestCurrentApplicationTaskRouteRefusesTheDroppedExpansions is the
// issue-128 guard: legacy's `?meta=yes` and `?result=yes` are answered with
// 400, never with a 200 that silently omits what was asked for.
func TestCurrentApplicationTaskRouteRefusesTheDroppedExpansions(t *testing.T) {
	for _, test := range []struct {
		query      string
		wantStatus int
	}{
		{query: "meta=yes", wantStatus: http.StatusBadRequest},
		{query: "meta=TRUE", wantStatus: http.StatusBadRequest},
		{query: "result=true", wantStatus: http.StatusBadRequest},
		{query: "result=%20yes%20", wantStatus: http.StatusBadRequest},
		// Legacy's own default. A client that always sends the falsy form must
		// keep working.
		{query: "meta=no&result=no", wantStatus: http.StatusOK},
		{query: "", wantStatus: http.StatusOK},
	} {
		t.Run(test.query, func(t *testing.T) {
			reader := &currentAgentTaskStatusStub{
				outcome: agentexecutionapp.CurrentAgentTaskStatusOutcome{
					Status: agentexecutionapp.CurrentAgentTaskStatusRunning,
				},
			}
			route := newCurrentApplicationTaskRoute(t, reader, &currentAgentCancellerStub{},
				currentStartPermissionResolverFunc(func(
					context.Context, auth.User, string, string,
				) (auth.PermissionResolution, error) {
					return auth.PermissionResolution{UserID: 11, Permissions: []string{
						CurrentApplicationTaskStatusPermission,
					}}, nil
				}))
			response := httptest.NewRecorder()
			route.ServeHTTP(response, currentApplicationTaskRequest(
				http.MethodGet, "7", "10000000-0000-4000-8000-000000000054", test.query))
			if response.Code != test.wantStatus {
				t.Fatalf("status=%d want=%d body=%q",
					response.Code, test.wantStatus, response.Body.String())
			}
			if test.wantStatus == http.StatusBadRequest {
				if reader.calls != 0 {
					t.Fatalf("refused expansion reached the reader %d times", reader.calls)
				}
				if !strings.Contains(response.Body.String(), "response message items") {
					t.Fatalf("body=%q does not say where the answer lives", response.Body.String())
				}
			}
		})
	}
}

func TestCurrentApplicationTaskRouteMapsUseCaseErrors(t *testing.T) {
	for _, test := range []struct {
		name       string
		responseID string
		statusErr  error
		wantStatus int
		wantBody   string
	}{
		{
			name:       "malformed id",
			responseID: "not-a-uuid",
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "absent foreign or not the owner",
			responseID: "10000000-0000-4000-8000-000000000055",
			statusErr:  agentexecutionapp.ErrCurrentAgentTaskStatusNotFound,
			wantStatus: http.StatusNotFound,
			wantBody:   "Task not found",
		},
		{
			name:       "store unavailable",
			responseID: "10000000-0000-4000-8000-000000000055",
			statusErr:  agentexecutionapp.ErrCurrentAgentTaskStatusFailed,
			wantStatus: http.StatusBadGateway,
		},
		{
			name:       "timed out",
			responseID: "10000000-0000-4000-8000-000000000055",
			statusErr:  context.DeadlineExceeded,
			wantStatus: http.StatusGatewayTimeout,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader := &currentAgentTaskStatusStub{err: test.statusErr}
			route := newCurrentApplicationTaskRoute(t, reader, &currentAgentCancellerStub{},
				currentStartPermissionResolverFunc(func(
					context.Context, auth.User, string, string,
				) (auth.PermissionResolution, error) {
					return auth.PermissionResolution{UserID: 11, Permissions: []string{
						CurrentApplicationTaskStatusPermission,
					}}, nil
				}))
			response := httptest.NewRecorder()
			route.ServeHTTP(response, currentApplicationTaskRequest(
				http.MethodGet, "7", test.responseID, ""))
			if response.Code != test.wantStatus {
				t.Fatalf("status=%d want=%d body=%q",
					response.Code, test.wantStatus, response.Body.String())
			}
			if test.wantBody != "" && !strings.Contains(response.Body.String(), test.wantBody) {
				t.Fatalf("body=%q does not contain %q", response.Body.String(), test.wantBody)
			}
			// The upstream text is never echoed: a caller must not learn which
			// response ids exist from a status probe.
			if strings.Contains(response.Body.String(), "not readable by this actor") {
				t.Fatalf("body=%q leaks the internal error", response.Body.String())
			}
		})
	}
}

// TestCurrentApplicationTaskRouteDeleteRunsTheSameCancellationUseCase is the
// no-second-stop-path claim. The DELETE must reach the canceller the
// /task/prompt_lib route runs, with the identical request model.
func TestCurrentApplicationTaskRouteDeleteRunsTheSameCancellationUseCase(t *testing.T) {
	canceller := &currentAgentCancellerStub{
		outcome: agentexecutionapp.CurrentAgentCancelOutcome{Deleted: true},
	}
	reader := &currentAgentTaskStatusStub{}
	route := newCurrentApplicationTaskRoute(t, reader, canceller,
		currentStartPermissionResolverFunc(func(
			context.Context, auth.User, string, string,
		) (auth.PermissionResolution, error) {
			return auth.PermissionResolution{UserID: 11, Permissions: []string{
				CurrentApplicationTaskCancelPermission,
			}}, nil
		}))
	response := httptest.NewRecorder()
	route.ServeHTTP(response, currentApplicationTaskRequest(
		http.MethodDelete, "7", "10000000-0000-4000-8000-000000000056", ""))
	if response.Code != http.StatusNoContent || response.Body.Len() != 0 || canceller.calls != 1 {
		t.Fatalf("status=%d body=%q calls=%d",
			response.Code, response.Body.String(), canceller.calls)
	}
	want := agentexecutionapp.CurrentAgentCancelRequest{
		ProjectID: 7, ActorUserID: 11,
		ResponseMessageID: "10000000-0000-4000-8000-000000000056",
	}
	if canceller.request != want {
		t.Fatalf("request=%+v want=%+v", canceller.request, want)
	}
	if reader.calls != 0 {
		t.Fatalf("the DELETE consulted the status reader %d times", reader.calls)
	}
}

func TestCurrentApplicationTaskRouteRejectsOtherMethods(t *testing.T) {
	route := newCurrentApplicationTaskRoute(t,
		&currentAgentTaskStatusStub{}, &currentAgentCancellerStub{},
		currentStartPermissionResolverFunc(func(
			context.Context, auth.User, string, string,
		) (auth.PermissionResolution, error) {
			return auth.PermissionResolution{UserID: 11, Permissions: []string{
				CurrentApplicationTaskStatusPermission,
				CurrentApplicationTaskCancelPermission,
			}}, nil
		}))
	response := httptest.NewRecorder()
	route.ServeHTTP(response, currentApplicationTaskRequest(
		http.MethodPost, "7", "10000000-0000-4000-8000-000000000057", ""))
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}
}

func TestCurrentApplicationTaskRouteRejectsIncompleteComposition(t *testing.T) {
	principal := currentStartPrincipalValidatorFunc(
		func(_ context.Context, user auth.User) (auth.User, error) { return user, nil })
	peer := currentStartPeerVerifierFunc(func(*http.Request) error { return nil })
	permissions := currentStartPermissionResolverFunc(func(
		context.Context, auth.User, string, string,
	) (auth.PermissionResolution, error) {
		return auth.PermissionResolution{}, nil
	})
	complete := apimw.AuthConfig{PrincipalValidator: principal, ForwardedIdentityVerifier: peer}
	for name, test := range map[string]struct {
		reader      CurrentAgentTaskStatusReader
		canceller   CurrentAgentCanceller
		authConfig  apimw.AuthConfig
		permissions auth.PermissionResolver
	}{
		"missing reader": {
			canceller: &currentAgentCancellerStub{}, authConfig: complete, permissions: permissions,
		},
		"missing canceller": {
			reader: &currentAgentTaskStatusStub{}, authConfig: complete, permissions: permissions,
		},
		"missing principal": {
			reader: &currentAgentTaskStatusStub{}, canceller: &currentAgentCancellerStub{},
			authConfig:  apimw.AuthConfig{ForwardedIdentityVerifier: peer},
			permissions: permissions,
		},
		"missing peer": {
			reader: &currentAgentTaskStatusStub{}, canceller: &currentAgentCancellerStub{},
			authConfig:  apimw.AuthConfig{PrincipalValidator: principal},
			permissions: permissions,
		},
		"missing permission resolver": {
			reader: &currentAgentTaskStatusStub{}, canceller: &currentAgentCancellerStub{},
			authConfig: complete,
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := NewCurrentApplicationTaskRoute(
				test.reader, test.canceller, test.authConfig, test.permissions)
			if !errors.Is(err, ErrInvalidCurrentApplicationTaskRoute) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func newCurrentApplicationTaskRoute(
	t *testing.T,
	reader CurrentAgentTaskStatusReader,
	canceller CurrentAgentCanceller,
	permissions auth.PermissionResolver,
) *CurrentApplicationTaskRoute {
	t.Helper()
	route, err := NewCurrentApplicationTaskRoute(
		reader,
		canceller,
		apimw.AuthConfig{
			PrincipalValidator: currentStartPrincipalValidatorFunc(
				func(_ context.Context, user auth.User) (auth.User, error) { return user, nil }),
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

func currentApplicationTaskRequest(method, projectID, responseID, query string) *http.Request {
	target := "/api/v2/elitea_core/application_task/prompt_lib/" + projectID + "/" + responseID
	if query != "" {
		target += "?" + query
	}
	request := httptest.NewRequest(method, target, nil)
	request.Header.Set("X-Auth-Type", "user")
	request.Header.Set("X-Auth-ID", "11")
	request.RemoteAddr = "10.0.0.8:43120"
	return request
}
