package agentexecution

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apimw "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware/authtest"
	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/audit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

const steeringTestResponse = "10000000-0000-4000-8000-000000000061"

type steeringUseCaseStub struct {
	queued  []agentexecutionapp.SteeringQueueRequest
	edited  []agentexecutionapp.SteeringEditRequest
	removed []agentexecutionapp.SteeringRemoveRequest
	listed  int
	err     error
}

func (stub *steeringUseCaseStub) message(content string) agentexecutionapp.SteeringMessage {
	at := time.Date(2026, 10, 26, 10, 15, 2, 0, time.UTC)
	return agentexecutionapp.SteeringMessage{
		ID: "7f0c3c9a-2d4e-4b8f-9a51-0d6c2f1e8b11", ResponseMessageID: steeringTestResponse,
		AuthorUserID: 11, Content: content, State: agentexecutionapp.SteeringPending, Revision: 1,
		CreatedAt: at, UpdatedAt: at,
	}
}

func (stub *steeringUseCaseStub) Queue(_ context.Context, request agentexecutionapp.SteeringQueueRequest) (agentexecutionapp.SteeringMessage, error) {
	stub.queued = append(stub.queued, request)
	return stub.message(request.Content), stub.err
}

func (stub *steeringUseCaseStub) List(context.Context, agentexecutionapp.SteeringTarget) ([]agentexecutionapp.SteeringMessage, error) {
	stub.listed++
	return []agentexecutionapp.SteeringMessage{stub.message("listed")}, stub.err
}

func (stub *steeringUseCaseStub) Edit(_ context.Context, request agentexecutionapp.SteeringEditRequest) (agentexecutionapp.SteeringMessage, error) {
	stub.edited = append(stub.edited, request)
	return stub.message(request.Content), stub.err
}

func (stub *steeringUseCaseStub) Remove(_ context.Context, request agentexecutionapp.SteeringRemoveRequest) error {
	stub.removed = append(stub.removed, request)
	return stub.err
}

type steeringAuditStub struct{ events []audit.Event }

func (stub *steeringAuditStub) Record(_ context.Context, event audit.Event) {
	stub.events = append(stub.events, event)
}

func newSteeringTestRoute(t *testing.T, useCase SteeringUseCase, permissions []string, recorder audit.Recorder) http.Handler {
	t.Helper()
	route, err := NewCurrentAgentSteeringRoute(
		useCase,
		apimw.AuthConfig{
			PrincipalValidator: currentStartPrincipalValidatorFunc(func(_ context.Context, user auth.User) (auth.User, error) { return user, nil }),
			FormSessions:       authtest.FormSessions{},
		},
		currentStartPermissionResolverFunc(func(context.Context, auth.User, string, string) (auth.PermissionResolution, error) {
			return auth.PermissionResolution{UserID: 11, Permissions: permissions}, nil
		}),
		recorder,
	)
	if err != nil {
		t.Fatal(err)
	}
	return route
}

func steeringRequest(method, suffix, body, key string) *http.Request {
	request := httptest.NewRequest(method,
		"/api/v2/elitea_core/task/prompt_lib/7/"+steeringTestResponse+"/steering"+suffix, strings.NewReader(body))
	if key != "" {
		request.Header.Set("Idempotency-Key", key)
	}
	authtest.SignIn(request, "11")
	request.RemoteAddr = "10.0.0.8:43120"
	return request
}

func TestSteeringRouteQueuesWithTheSendPermissionAndAuditsADigest(t *testing.T) {
	if CurrentAgentSteeringPermission != "models.chat.messages.create" || CurrentAgentSteeringMode != auth.PermissionModeDefault {
		t.Fatalf("permission=%q mode=%q", CurrentAgentSteeringPermission, CurrentAgentSteeringMode)
	}
	useCase := &steeringUseCaseStub{}
	recorder := &steeringAuditStub{}
	route := newSteeringTestRoute(t, useCase, []string{CurrentAgentSteeringPermission}, recorder)

	response := httptest.NewRecorder()
	route.ServeHTTP(response, steeringRequest(http.MethodPost, "", `{"content":"skip the tests"}`, "key-1"))
	if response.Code != http.StatusAccepted || len(useCase.queued) != 1 {
		t.Fatalf("status=%d body=%q queued=%d", response.Code, response.Body.String(), len(useCase.queued))
	}
	got := useCase.queued[0]
	if got.ProjectID != 7 || got.ActorUserID != 11 || got.ResponseMessageID != steeringTestResponse ||
		got.IdempotencyKey != "key-1" || got.Content != "skip the tests" {
		t.Fatalf("request=%+v", got)
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"id", "response_message_id", "author_id", "content", "state", "revision", "created_at", "updated_at", "injected_at", "message_id"} {
		if _, ok := body[field]; !ok {
			t.Fatalf("body has no %q: %v", field, body)
		}
	}
	if len(recorder.events) != 1 || strings.Contains(recorder.events[0].Action, "skip the tests") ||
		!strings.Contains(recorder.events[0].Action, "sha256") {
		t.Fatalf("audit=%+v", recorder.events)
	}
}

func TestSteeringRouteRefusesBeforeTheUseCase(t *testing.T) {
	for _, test := range []struct {
		name        string
		method      string
		suffix      string
		body        string
		permissions []string
		want        int
		wantCode    string
	}{
		{name: "no permission", method: http.MethodPost, body: `{"content":"x"}`, want: http.StatusForbidden},
		{name: "empty content", method: http.MethodPost, body: `{"content":"   "}`, permissions: []string{CurrentAgentSteeringPermission}, want: http.StatusBadRequest, wantCode: "steering_invalid"},
		{name: "too large", method: http.MethodPost, body: `{"content":"` + strings.Repeat("a", agentexecutionapp.MaxSteeringContentBytes+1) + `"}`, permissions: []string{CurrentAgentSteeringPermission}, want: http.StatusBadRequest, wantCode: "steering_invalid"},
		{name: "bad steering id", method: http.MethodDelete, suffix: "/not-a-uuid", permissions: []string{CurrentAgentSteeringPermission}, want: http.StatusBadRequest, wantCode: "steering_invalid"},
	} {
		t.Run(test.name, func(t *testing.T) {
			useCase := &steeringUseCaseStub{}
			route := newSteeringTestRoute(t, useCase, test.permissions, nil)
			response := httptest.NewRecorder()
			route.ServeHTTP(response, steeringRequest(test.method, test.suffix, test.body, "key-1"))
			if response.Code != test.want {
				t.Fatalf("status=%d want=%d body=%q", response.Code, test.want, response.Body.String())
			}
			if test.wantCode != "" && !strings.Contains(response.Body.String(), `"error":"`+test.wantCode+`"`) {
				t.Fatalf("body=%q has no code %q", response.Body.String(), test.wantCode)
			}
			if len(useCase.queued)+len(useCase.removed) != 0 {
				t.Fatal("a refused request reached the use case")
			}
		})
	}
}

func TestSteeringRouteMapsTypedErrorsToCodes(t *testing.T) {
	for _, test := range []struct {
		err  error
		want int
		code string
	}{
		{agentexecutionapp.ErrSteeringIdempotencyRequired, http.StatusBadRequest, "idempotency_key_required"},
		{agentexecutionapp.ErrSteeringTurnNotRunning, http.StatusConflict, "turn_not_running"},
		{agentexecutionapp.ErrSteeringNotSupported, http.StatusConflict, "steering_not_supported"},
		{agentexecutionapp.ErrSteeringQueueFull, http.StatusConflict, "steering_queue_full"},
		{agentexecutionapp.ErrSteeringIdempotencyConflict, http.StatusConflict, "idempotency_conflict"},
		{agentexecutionapp.ErrSteeringNotFound, http.StatusNotFound, "steering_not_found"},
		{agentexecutionapp.ErrSteeringNotPending, http.StatusConflict, "steering_not_pending"},
		{agentexecutionapp.ErrSteeringFailed, http.StatusBadGateway, ""},
		{context.DeadlineExceeded, http.StatusGatewayTimeout, ""},
	} {
		t.Run(test.err.Error(), func(t *testing.T) {
			route := newSteeringTestRoute(t, &steeringUseCaseStub{err: test.err}, []string{CurrentAgentSteeringPermission}, nil)
			response := httptest.NewRecorder()
			route.ServeHTTP(response, steeringRequest(http.MethodPatch, "/7f0c3c9a-2d4e-4b8f-9a51-0d6c2f1e8b11", `{"content":"edit"}`, ""))
			if response.Code != test.want {
				t.Fatalf("status=%d want=%d body=%q", response.Code, test.want, response.Body.String())
			}
			if test.code != "" && !strings.Contains(response.Body.String(), `"error":"`+test.code+`"`) {
				t.Fatalf("body=%q", response.Body.String())
			}
		})
	}
}

func TestSteeringRouteListsEditsRemovesAndRateLimits(t *testing.T) {
	useCase := &steeringUseCaseStub{}
	route := newSteeringTestRoute(t, useCase, []string{CurrentAgentSteeringPermission}, nil)

	response := httptest.NewRecorder()
	route.ServeHTTP(response, steeringRequest(http.MethodGet, "", "", ""))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"items":[`) || useCase.listed != 1 {
		t.Fatalf("list status=%d body=%q", response.Code, response.Body.String())
	}
	response = httptest.NewRecorder()
	// An upper-case id is folded to the stored spelling.
	route.ServeHTTP(response, steeringRequest(http.MethodDelete, "/7F0C3C9A-2D4E-4B8F-9A51-0D6C2F1E8B11", "", ""))
	if response.Code != http.StatusNoContent || len(useCase.removed) != 1 ||
		useCase.removed[0].SteeringID != "7f0c3c9a-2d4e-4b8f-9a51-0d6c2f1e8b11" {
		t.Fatalf("remove status=%d removed=%+v", response.Code, useCase.removed)
	}

	limited := 0
	for range steeringRatePerMinute + 1 {
		response = httptest.NewRecorder()
		route.ServeHTTP(response, steeringRequest(http.MethodPost, "", `{"content":"again"}`, "key-rate"))
		if response.Code == http.StatusTooManyRequests {
			limited++
			if response.Header().Get("Retry-After") == "" || !strings.Contains(response.Body.String(), "rate_limited") {
				t.Fatalf("429 without Retry-After or code: %q", response.Body.String())
			}
		}
	}
	if limited != 1 {
		t.Fatalf("limited=%d", limited)
	}
}
