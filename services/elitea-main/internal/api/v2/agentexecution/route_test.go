package agentexecution

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

type currentStartUseCaseStub struct {
	request             agentexecutionapp.CurrentApplicationStartRequest
	adhocRequest        agentexecutionapp.CurrentAdhocStartRequest
	regenerationRequest agentexecutionapp.CurrentRegenerationRequest
	continuationRequest agentexecutionapp.CurrentContinuationRequest
	outcome             agentexecutionapp.CurrentApplicationStartOutcome
	err                 error
	calls               int
	adhocCalls          int
	regenerationCalls   int
	continuationCalls   int
}

func (stub *currentStartUseCaseStub) ContinueCurrentAgent(
	_ context.Context,
	request agentexecutionapp.CurrentContinuationRequest,
) (agentexecutionapp.CurrentApplicationStartOutcome, error) {
	stub.continuationCalls++
	stub.continuationRequest = request
	return stub.outcome, stub.err
}

func (stub *currentStartUseCaseStub) RegenerateCurrentAgent(
	_ context.Context,
	request agentexecutionapp.CurrentRegenerationRequest,
) (agentexecutionapp.CurrentApplicationStartOutcome, error) {
	stub.regenerationCalls++
	stub.regenerationRequest = request
	return stub.outcome, stub.err
}

func (stub *currentStartUseCaseStub) StartCurrentAdhoc(
	_ context.Context,
	request agentexecutionapp.CurrentAdhocStartRequest,
) (agentexecutionapp.CurrentApplicationStartOutcome, error) {
	stub.adhocCalls++
	stub.adhocRequest = request
	return stub.outcome, stub.err
}

func (stub *currentStartUseCaseStub) StartCurrentApplication(
	_ context.Context,
	request agentexecutionapp.CurrentApplicationStartRequest,
) (agentexecutionapp.CurrentApplicationStartOutcome, error) {
	stub.calls++
	stub.request = request
	return stub.outcome, stub.err
}

type currentStartPrincipalValidatorFunc func(context.Context, auth.User) (auth.User, error)

func (function currentStartPrincipalValidatorFunc) ValidatePrincipal(
	ctx context.Context,
	user auth.User,
) (auth.User, error) {
	return function(ctx, user)
}

type currentStartPeerVerifierFunc func(*http.Request) error

func (function currentStartPeerVerifierFunc) VerifyForwardedIdentityPeer(request *http.Request) error {
	return function(request)
}

type currentStartPermissionResolverFunc func(
	context.Context,
	auth.User,
	string,
	string,
) (auth.PermissionResolution, error)

func (function currentStartPermissionResolverFunc) ResolvePermissions(
	ctx context.Context,
	user auth.User,
	mode,
	projectID string,
) (auth.PermissionResolution, error) {
	return function(ctx, user, mode, projectID)
}

func TestCurrentApplicationStartRoutePreservesPathRBACAndResponseContract(t *testing.T) {
	if CurrentApplicationStartPath != "/api/v2/elitea_core/messages/prompt_lib/{projectID}/{conversationID}" ||
		CurrentApplicationStartPermission != "models.chat.messages.create" ||
		CurrentApplicationStartMode != auth.PermissionModeDefault {
		t.Fatalf("route contract drifted: path=%q permission=%q mode=%q",
			CurrentApplicationStartPath, CurrentApplicationStartPermission, CurrentApplicationStartMode)
	}
	useCase := &currentStartUseCaseStub{outcome: agentexecutionapp.CurrentApplicationStartOutcome{
		ExecutionID: "execution-1", CommandID: "command-1",
		ResponseMessageID: "response-1", Created: true,
	}}
	permissions := currentStartPermissionResolverFunc(func(
		_ context.Context,
		user auth.User,
		mode,
		projectID string,
	) (auth.PermissionResolution, error) {
		if user.UserID != "11" || mode != auth.PermissionModeDefault || projectID != "7" {
			t.Fatalf("permission input user=%+v mode=%q project=%q", user, mode, projectID)
		}
		return auth.PermissionResolution{UserID: 11, Permissions: []string{CurrentApplicationStartPermission}}, nil
	})
	route := newCurrentStartRoute(t, useCase, permissions)

	response := httptest.NewRecorder()
	route.ServeHTTP(response, currentStartRequest(validCurrentStartBody()))

	if response.Code != http.StatusOK || useCase.calls != 1 ||
		useCase.request.ProjectID != 7 || useCase.request.ActorUserID != 11 ||
		useCase.request.ConversationUUID != "8bc66e50-46c4-4e2c-94ec-daec6c596ac0" ||
		useCase.request.TargetParticipantID != 21 ||
		useCase.request.QuestionID != "ee92ccbd-3312-4c72-b20b-fddf224e7c0e" ||
		useCase.request.UserInput != "hello" {
		t.Fatalf("status=%d calls=%d request=%+v body=%s",
			response.Code, useCase.calls, useCase.request, response.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil ||
		body["execution_id"] != "execution-1" || body["task_id"] != "execution-1" ||
		body["response_message_id"] != "response-1" ||
		body["events_url"] != "/api/v2/executions/7/execution-1/events" {
		t.Fatalf("response=%+v error=%v", body, err)
	}
}

func TestCurrentApplicationStartRouteAdmitsBoundedAdhocMainChatTurn(t *testing.T) {
	useCase := &currentStartUseCaseStub{outcome: agentexecutionapp.CurrentApplicationStartOutcome{
		ExecutionID: "execution-adhoc", CommandID: "command-adhoc",
		ResponseMessageID: "response-adhoc", Created: true,
	}}
	route := newCurrentStartRoute(t, useCase, allowCurrentStartPermission())

	response := httptest.NewRecorder()
	route.ServeHTTP(response, currentAdhocStartRequest(validCurrentAdhocStartBody()))

	if response.Code != http.StatusOK || useCase.calls != 0 || useCase.adhocCalls != 1 ||
		useCase.adhocRequest.ProjectID != 7 || useCase.adhocRequest.ActorUserID != 11 ||
		useCase.adhocRequest.TargetParticipantID != 0 ||
		useCase.adhocRequest.UserInput != "hello from main chat" ||
		!bytes.Equal(useCase.adhocRequest.LLMSettings, []byte(`{"model_name":"model","model_project_id":7}`)) {
		t.Fatalf("status=%d app_calls=%d adhoc_calls=%d request=%+v body=%s",
			response.Code, useCase.calls, useCase.adhocCalls, useCase.adhocRequest, response.Body.String())
	}
}

func TestCurrentRegenerationRoutePreservesPathRBACAndResponseIdentity(t *testing.T) {
	if CurrentRegenerationPath != "/api/v2/elitea_core/regenerate/prompt_lib/{projectID}/{responseMessageID}" ||
		CurrentRegenerationPermission != "models.chat.conversations.regenerate" ||
		CurrentRegenerationContract != "agent.regenerate.v1" {
		t.Fatalf("regeneration contract drifted: path=%q permission=%q contract=%q",
			CurrentRegenerationPath, CurrentRegenerationPermission, CurrentRegenerationContract)
	}
	useCase := &currentStartUseCaseStub{outcome: agentexecutionapp.CurrentApplicationStartOutcome{
		ExecutionID: "execution-regenerate", CommandID: "command-regenerate",
		ResponseMessageID: "30e0913e-10d4-43db-b8d0-c7b79480935a", Created: true,
	}}
	permissions := currentStartPermissionResolverFunc(func(
		_ context.Context,
		_ auth.User,
		mode,
		projectID string,
	) (auth.PermissionResolution, error) {
		if mode != auth.PermissionModeDefault || projectID != "7" {
			t.Fatalf("permission mode=%q project=%q", mode, projectID)
		}
		return auth.PermissionResolution{
			UserID: 11, Permissions: []string{CurrentRegenerationPermission},
		}, nil
	})
	route := newCurrentStartRoute(t, useCase, permissions)
	response := httptest.NewRecorder()
	route.ServeHTTP(response, currentRegenerationRequest(validCurrentRegenerationBody()))

	request := useCase.regenerationRequest
	if response.Code != http.StatusOK || useCase.regenerationCalls != 1 ||
		request.ProjectID != 7 || request.ActorUserID != 11 ||
		request.ConversationUUID != "8bc66e50-46c4-4e2c-94ec-daec6c596ac0" ||
		request.QuestionID != "ee92ccbd-3312-4c72-b20b-fddf224e7c0e" ||
		request.ResponseMessageID != "30e0913e-10d4-43db-b8d0-c7b79480935a" ||
		request.RegenerationID != "9fba0a08-5049-42bb-9019-c2f3df686010" ||
		request.RequestedParticipantID != 21 ||
		!bytes.Equal(request.LLMSettings, []byte(`{"model_name":"model","model_project_id":7}`)) {
		t.Fatalf("status=%d calls=%d request=%+v body=%s",
			response.Code, useCase.regenerationCalls, request, response.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil ||
		body["execution_id"] != "execution-regenerate" ||
		body["response_message_id"] != "30e0913e-10d4-43db-b8d0-c7b79480935a" ||
		body["events_url"] != "/api/v2/executions/7/execution-regenerate/events" {
		t.Fatalf("response=%+v error=%v", body, err)
	}
}

func TestCurrentContinuationRoutePreservesCurrentPathRBACAndResponseIdentity(t *testing.T) {
	if CurrentContinuationPath != "/api/v2/elitea_core/continue_predict/prompt_lib/{projectID}/{conversationID}" ||
		CurrentContinuationContract != "agent.continue.hitl.v1" {
		t.Fatalf("continuation contract drifted: path=%q contract=%q", CurrentContinuationPath, CurrentContinuationContract)
	}
	useCase := &currentStartUseCaseStub{outcome: agentexecutionapp.CurrentApplicationStartOutcome{
		ExecutionID: "execution-continue", CommandID: "command-continue",
		ResponseMessageID: "30e0913e-10d4-43db-b8d0-c7b79480935a", Created: true,
	}}
	route := newCurrentStartRoute(t, useCase, allowCurrentStartPermission())
	response := httptest.NewRecorder()
	route.ServeHTTP(response, currentContinuationRequest(validCurrentContinuationBody()))

	request := useCase.continuationRequest
	if response.Code != http.StatusOK || useCase.continuationCalls != 1 ||
		request.ProjectID != 7 || request.ActorUserID != 11 ||
		request.ConversationUUID != "8bc66e50-46c4-4e2c-94ec-daec6c596ac0" ||
		request.ResponseMessageID != "30e0913e-10d4-43db-b8d0-c7b79480935a" ||
		request.ThreadID != "thread-current-1" || request.Action != "edit" ||
		request.Value != "delete only merged branches" {
		t.Fatalf("status=%d calls=%d request=%+v body=%s", response.Code, useCase.continuationCalls, request, response.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil ||
		body["execution_id"] != "execution-continue" ||
		body["response_message_id"] != "30e0913e-10d4-43db-b8d0-c7b79480935a" ||
		body["events_url"] != "/api/v2/executions/7/execution-continue/events" {
		t.Fatalf("response=%+v error=%v", body, err)
	}
}

func TestCurrentOutputLimitContinuationRouteCarriesOnlyResponseIdentity(t *testing.T) {
	if CurrentOutputLimitContinuationContract != "agent.continue.output-limit.v1" {
		t.Fatalf("output continuation contract drifted: %q", CurrentOutputLimitContinuationContract)
	}
	useCase := &currentStartUseCaseStub{outcome: agentexecutionapp.CurrentApplicationStartOutcome{
		ExecutionID: "execution-output-continue", CommandID: "command-output-continue",
		ResponseMessageID: "30e0913e-10d4-43db-b8d0-c7b79480935a", Created: true,
	}}
	route := newCurrentStartRoute(t, useCase, allowCurrentStartPermission())
	response := httptest.NewRecorder()
	route.ServeHTTP(response, currentOutputContinuationRequest(validCurrentOutputContinuationBody()))

	request := useCase.continuationRequest
	if response.Code != http.StatusOK || useCase.continuationCalls != 1 ||
		request.Kind != agentexecutionapp.CurrentContinuationOutputLimit ||
		request.ProjectID != 7 || request.ActorUserID != 11 ||
		request.ConversationUUID != "8bc66e50-46c4-4e2c-94ec-daec6c596ac0" ||
		request.ResponseMessageID != "30e0913e-10d4-43db-b8d0-c7b79480935a" ||
		request.ThreadID != "" || request.Action != "" || request.Value != "" ||
		request.AuthorizationID != "" || len(request.HITLDecisions) != 0 {
		t.Fatalf("status=%d calls=%d request=%+v body=%s",
			response.Code, useCase.continuationCalls, request, response.Body.String())
	}
}

func TestCurrentOutputLimitContinuationRouteRejectsBrowserSelectedRuntimeState(t *testing.T) {
	for name, body := range map[string]string{
		"thread": strings.Replace(validCurrentOutputContinuationBody(), `"message_id":`, `"thread_id":"thread-stale","message_id":`, 1),
		"hitl":   strings.Replace(validCurrentOutputContinuationBody(), `"message_id":`, `"hitl_resume":true,"message_id":`, 1),
		"tokens": strings.Replace(validCurrentOutputContinuationBody(), `"message_id":`, `"mcp_tokens":{},"message_id":`, 1),
		"input":  strings.Replace(validCurrentOutputContinuationBody(), `"message_id":`, `"user_input":"continue","message_id":`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			useCase := &currentStartUseCaseStub{}
			route := newCurrentStartRoute(t, useCase, allowCurrentStartPermission())
			response := httptest.NewRecorder()
			route.ServeHTTP(response, currentOutputContinuationRequest(body))
			if response.Code != http.StatusUnprocessableEntity || useCase.continuationCalls != 0 {
				t.Fatalf("status=%d calls=%d body=%s", response.Code, useCase.continuationCalls, response.Body.String())
			}
		})
	}
}

func TestCurrentContinuationRouteCarriesBlockWithComment(t *testing.T) {
	useCase := &currentStartUseCaseStub{outcome: agentexecutionapp.CurrentApplicationStartOutcome{
		ExecutionID: "execution-comment", CommandID: "command-comment",
		ResponseMessageID: "30e0913e-10d4-43db-b8d0-c7b79480935a", Created: true,
	}}
	route := newCurrentStartRoute(t, useCase, allowCurrentStartPermission())
	body := strings.Replace(validCurrentContinuationBody(), `"hitl_action":"edit"`, `"hitl_action":"block_with_comment"`, 1)
	body = strings.Replace(body, `"hitl_value":"delete only merged branches"`, `"hitl_value":"append data before retrying delete"`, 1)
	response := httptest.NewRecorder()
	route.ServeHTTP(response, currentContinuationRequest(body))

	request := useCase.continuationRequest
	if response.Code != http.StatusOK || useCase.continuationCalls != 1 ||
		request.Action != "block_with_comment" || request.Value != "append data before retrying delete" {
		t.Fatalf("status=%d calls=%d request=%+v body=%s",
			response.Code, useCase.continuationCalls, request, response.Body.String())
	}
}

func TestCurrentContinuationRouteCanonicalizesStructuredClarificationAnswer(t *testing.T) {
	useCase := &currentStartUseCaseStub{outcome: agentexecutionapp.CurrentApplicationStartOutcome{
		ExecutionID: "execution-answer", CommandID: "command-answer",
		ResponseMessageID: "30e0913e-10d4-43db-b8d0-c7b79480935a", Created: true,
	}}
	route := newCurrentStartRoute(t, useCase, allowCurrentStartPermission())
	body := strings.Replace(validCurrentContinuationBody(), `"hitl_action":"edit"`, `"hitl_action":"answer"`, 1)
	body = strings.Replace(body, `"hitl_value":"delete only merged branches"`, `"hitl_value":{"q2":["Safe","Fast"],"q1":"Staging"}`, 1)
	response := httptest.NewRecorder()
	route.ServeHTTP(response, currentContinuationRequest(body))

	request := useCase.continuationRequest
	if response.Code != http.StatusOK || useCase.continuationCalls != 1 ||
		request.Action != "answer" || request.Value != `{"q1":"Staging","q2":["Safe","Fast"]}` {
		t.Fatalf("status=%d calls=%d request=%+v body=%s",
			response.Code, useCase.continuationCalls, request, response.Body.String())
	}
}

func TestCurrentAuthorizationContinuationRouteCarriesExactInvocationAndCredentials(t *testing.T) {
	if CurrentAuthorizationContinuationContract != "agent.continue.authorization.v1" {
		t.Fatalf("authorization continuation contract drifted: %q", CurrentAuthorizationContinuationContract)
	}
	useCase := &currentStartUseCaseStub{outcome: agentexecutionapp.CurrentApplicationStartOutcome{
		ExecutionID: "execution-authorization", CommandID: "command-authorization",
		ResponseMessageID: "30e0913e-10d4-43db-b8d0-c7b79480935a", Created: true,
	}}
	route := newCurrentStartRoute(t, useCase, allowCurrentStartPermission())
	response := httptest.NewRecorder()
	route.ServeHTTP(response, currentAuthorizationContinuationRequest(validCurrentAuthorizationContinuationBody()))

	request := useCase.continuationRequest
	if response.Code != http.StatusOK || useCase.continuationCalls != 1 ||
		request.Kind != agentexecutionapp.CurrentContinuationAuthorization ||
		request.AuthorizationID != "tool-run-sharepoint-1" || request.Action != "authorize" ||
		request.ProjectID != 7 || request.ActorUserID != 11 ||
		request.ConversationUUID != "8bc66e50-46c4-4e2c-94ec-daec6c596ac0" ||
		request.ResponseMessageID != "30e0913e-10d4-43db-b8d0-c7b79480935a" ||
		!bytes.Equal(request.MCPTokens, []byte(`{"https://sharepoint.example.test":{"access_token":"runtime-secret"}}`)) ||
		!bytes.Equal(request.IgnoredMCPServers, []byte(`[]`)) ||
		!bytes.Equal(request.DeclinedMCPServers, []byte(`[]`)) {
		t.Fatalf("status=%d calls=%d request=%+v body=%s",
			response.Code, useCase.continuationCalls, request, response.Body.String())
	}
}

func TestCurrentAuthorizationContinuationRouteAcceptsBoundedExactDecisionSet(t *testing.T) {
	useCase := &currentStartUseCaseStub{outcome: agentexecutionapp.CurrentApplicationStartOutcome{
		ExecutionID: "execution-parallel-authorization", CommandID: "command-parallel-authorization",
		ResponseMessageID: "30e0913e-10d4-43db-b8d0-c7b79480935a", Created: true,
	}}
	route := newCurrentStartRoute(t, useCase, allowCurrentStartPermission())
	body := strings.Replace(validCurrentAuthorizationContinuationBody(), `"hitl_resume":false`, `"hitl_resume":true`, 1)
	body = strings.Replace(
		body,
		`"hitl_decisions":[]`,
		`"hitl_decisions":[{"interrupt_id":"auth-2","tool_call_id":"call-2","guardrail_type":"mcp_auth","action":"skip"},{"interrupt_id":"auth-1","tool_call_id":"call-1","guardrail_type":"mcp_auth","action":"authorize"}]`,
		1,
	)
	body = strings.Replace(body, `"authorization_request_id":"tool-run-sharepoint-1"`, `"authorization_request_id":""`, 1)
	body = strings.Replace(body, `"authorization_action":"authorize"`, `"authorization_action":""`, 1)
	response := httptest.NewRecorder()
	route.ServeHTTP(response, currentAuthorizationContinuationRequest(body))

	request := useCase.continuationRequest
	if response.Code != http.StatusOK || useCase.continuationCalls != 1 ||
		request.Kind != agentexecutionapp.CurrentContinuationAuthorization ||
		request.AuthorizationID != "" || request.Action != "" ||
		len(request.HITLDecisions) != 2 ||
		request.HITLDecisions[0].InterruptID != "auth-2" ||
		request.HITLDecisions[1].GuardrailType != "mcp_auth" {
		t.Fatalf("status=%d calls=%d request=%+v body=%s",
			response.Code, useCase.continuationCalls, request, response.Body.String())
	}
}

func TestCurrentAuthorizationContinuationRouteRejectsAmbiguousAndMissingIdentity(t *testing.T) {
	useCase := &currentStartUseCaseStub{}
	route := newCurrentStartRoute(t, useCase, allowCurrentStartPermission())
	for name, body := range map[string]string{
		"missing exact identity": strings.Replace(
			validCurrentAuthorizationContinuationBody(),
			`"authorization_request_id":"tool-run-sharepoint-1"`,
			`"authorization_request_id":""`,
			1,
		),
		"mixed HITL resume": strings.Replace(
			validCurrentAuthorizationContinuationBody(),
			`"hitl_resume":false`,
			`"hitl_resume":true`,
			1,
		),
		"unsupported action": strings.Replace(
			validCurrentAuthorizationContinuationBody(),
			`"authorization_action":"authorize"`,
			`"authorization_action":"approve"`,
			1,
		),
	} {
		t.Run(name, func(t *testing.T) {
			response := httptest.NewRecorder()
			route.ServeHTTP(response, currentAuthorizationContinuationRequest(body))
			if response.Code != http.StatusUnprocessableEntity || useCase.continuationCalls != 0 {
				t.Fatalf("status=%d calls=%d body=%s",
					response.Code, useCase.continuationCalls, response.Body.String())
			}
		})
	}
}

func TestCurrentContinuationRouteAcceptsBoundedParallelDecisions(t *testing.T) {
	useCase := &currentStartUseCaseStub{outcome: agentexecutionapp.CurrentApplicationStartOutcome{
		ExecutionID: "execution-parallel", CommandID: "command-parallel",
		ResponseMessageID: "30e0913e-10d4-43db-b8d0-c7b79480935a", Created: true,
	}}
	route := newCurrentStartRoute(t, useCase, allowCurrentStartPermission())
	body := strings.Replace(validCurrentContinuationBody(), `"hitl_action":"edit",`, `"hitl_action":"",`, 1)
	body = strings.Replace(body, `"hitl_value":"delete only merged branches",`, ``, 1)
	body = strings.Replace(
		body,
		`"hitl_decisions":[]`,
		`"hitl_decisions":[{"interrupt_id":"interrupt-1","action":"approve"},{"interrupt_id":"interrupt-2","tool_call_id":"tool-2","action":"block_with_comment","value":"archive first"}]`,
		1,
	)
	response := httptest.NewRecorder()
	route.ServeHTTP(response, currentContinuationRequest(body))
	if response.Code != http.StatusOK || useCase.continuationCalls != 1 ||
		len(useCase.continuationRequest.HITLDecisions) != 2 ||
		useCase.continuationRequest.HITLDecisions[1].InterruptID != "interrupt-2" ||
		useCase.continuationRequest.HITLDecisions[1].ToolCallID != "tool-2" ||
		useCase.continuationRequest.HITLDecisions[1].Value != "archive first" {
		t.Fatalf("status=%d calls=%d request=%+v body=%s",
			response.Code, useCase.continuationCalls, useCase.continuationRequest, response.Body.String())
	}
}

func TestCurrentContinuationRouteRejectsAmbiguousParallelAndMCPResumeShapes(t *testing.T) {
	useCase := &currentStartUseCaseStub{}
	route := newCurrentStartRoute(t, useCase, allowCurrentStartPermission())
	for name, body := range map[string]string{
		"plural with scalar": strings.Replace(validCurrentContinuationBody(), `"hitl_decisions":[]`, `"hitl_decisions":[{"interrupt_id":"child","action":"approve"}]`, 1),
		"private child route": strings.Replace(
			strings.Replace(strings.Replace(validCurrentContinuationBody(), `"hitl_action":"edit",`, `"hitl_action":"",`, 1), `"hitl_value":"delete only merged branches",`, ``, 1),
			`"hitl_decisions":[]`,
			`"hitl_decisions":[{"interrupt_id":"child","action":"approve","child_thread_id":"private"}]`,
			1,
		),
		"mcp": strings.Replace(validCurrentContinuationBody(), `"mcp_tokens":{}`, `"mcp_tokens":{"server":{"access_token":"not-forwarded"}}`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			response := httptest.NewRecorder()
			route.ServeHTTP(response, currentContinuationRequest(body))
			if response.Code != http.StatusUnprocessableEntity || useCase.continuationCalls != 0 {
				t.Fatalf("status=%d calls=%d body=%s", response.Code, useCase.continuationCalls, response.Body.String())
			}
		})
	}
}

func TestCurrentRegenerationRouteRejectsEditedItemsBeforeUseCase(t *testing.T) {
	useCase := &currentStartUseCaseStub{}
	route := newCurrentStartRoute(t, useCase, currentStartPermissionResolverFunc(func(
		context.Context, auth.User, string, string,
	) (auth.PermissionResolution, error) {
		return auth.PermissionResolution{
			UserID: 11, Permissions: []string{CurrentRegenerationPermission},
		}, nil
	}))
	body := strings.Replace(
		validCurrentRegenerationBody(),
		`"updated_items":[]`,
		`"updated_items":[{"uuid":"item","content":"edited"}]`,
		1,
	)
	response := httptest.NewRecorder()
	route.ServeHTTP(response, currentRegenerationRequest(body))
	if response.Code != http.StatusUnprocessableEntity || useCase.regenerationCalls != 0 {
		t.Fatalf("status=%d calls=%d body=%s", response.Code, useCase.regenerationCalls, response.Body.String())
	}
}

func TestCurrentRegenerationRouteReportsFinalizingResponseAsRetryable(t *testing.T) {
	useCase := &currentStartUseCaseStub{
		err: agentexecutionapp.ErrCurrentAgentRegenerationStillFinalizing,
	}
	route := newCurrentStartRoute(t, useCase, currentStartPermissionResolverFunc(func(
		context.Context, auth.User, string, string,
	) (auth.PermissionResolution, error) {
		return auth.PermissionResolution{
			UserID: 11, Permissions: []string{CurrentRegenerationPermission},
		}, nil
	}))

	response := httptest.NewRecorder()
	route.ServeHTTP(response, currentRegenerationRequest(validCurrentRegenerationBody()))

	if response.Code != http.StatusConflict || response.Header().Get("Retry-After") != "1" ||
		useCase.regenerationCalls != 1 {
		t.Fatalf("status=%d retry_after=%q calls=%d body=%s",
			response.Code, response.Header().Get("Retry-After"), useCase.regenerationCalls, response.Body.String())
	}
	var body struct {
		Error     string `json:"error"`
		Message   string `json:"message"`
		Retryable bool   `json:"retryable"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil ||
		body.Error != "agent_regeneration_pending" || !body.Retryable ||
		!strings.Contains(body.Message, "still being finalized") {
		t.Fatalf("body=%+v error=%v", body, err)
	}
}

func TestCurrentApplicationStartRouteFallsBackBeforeUseCaseForUnsupportedTurn(t *testing.T) {
	useCase := &currentStartUseCaseStub{}
	permissions := allowCurrentStartPermission()
	route := newCurrentStartRoute(t, useCase, permissions)
	body := strings.Replace(validCurrentStartBody(), `"mcp_tokens":{}`, `"mcp_tokens":[]`, 1)

	response := httptest.NewRecorder()
	route.ServeHTTP(response, currentStartRequest(body))

	if response.Code != http.StatusUnprocessableEntity || useCase.calls != 0 ||
		!strings.Contains(response.Body.String(), "unsupported_agent_execution") {
		t.Fatalf("status=%d calls=%d body=%s", response.Code, useCase.calls, response.Body.String())
	}
}

func TestCurrentApplicationStartRouteAuthorizesBeforeExecution(t *testing.T) {
	useCase := &currentStartUseCaseStub{}
	permissions := currentStartPermissionResolverFunc(func(
		context.Context,
		auth.User,
		string,
		string,
	) (auth.PermissionResolution, error) {
		return auth.PermissionResolution{UserID: 11}, nil
	})
	route := newCurrentStartRoute(t, useCase, permissions)

	response := httptest.NewRecorder()
	route.ServeHTTP(response, currentStartRequest(validCurrentStartBody()))

	if response.Code != http.StatusForbidden || useCase.calls != 0 {
		t.Fatalf("status=%d calls=%d body=%s", response.Code, useCase.calls, response.Body.String())
	}
}

func newCurrentStartRoute(
	t *testing.T,
	useCase StartUseCase,
	permissions auth.PermissionResolver,
) *CurrentApplicationStartRoute {
	t.Helper()
	route, err := NewCurrentApplicationStartRoute(
		useCase,
		apimw.AuthConfig{
			PrincipalValidator: currentStartPrincipalValidatorFunc(func(
				_ context.Context,
				user auth.User,
			) (auth.User, error) {
				if user.ID != "11" {
					return auth.User{}, errors.New("unexpected user")
				}
				return user, nil
			}),
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

func allowCurrentStartPermission() auth.PermissionResolver {
	return currentStartPermissionResolverFunc(func(
		context.Context,
		auth.User,
		string,
		string,
	) (auth.PermissionResolution, error) {
		return auth.PermissionResolution{UserID: 11, Permissions: []string{CurrentApplicationStartPermission}}, nil
	})
}

func currentStartRequest(body string) *http.Request {
	request := httptest.NewRequest(
		http.MethodPost,
		"/api/v2/elitea_core/messages/prompt_lib/7/8bc66e50-46c4-4e2c-94ec-daec6c596ac0?execution_contract="+CurrentApplicationStartContract,
		strings.NewReader(body),
	)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Auth-Type", "user")
	request.Header.Set("X-Auth-ID", "11")
	request.RemoteAddr = "10.0.0.8:43120"
	return request
}

func currentAdhocStartRequest(body string) *http.Request {
	request := httptest.NewRequest(
		http.MethodPost,
		"/api/v2/elitea_core/messages/prompt_lib/7/8bc66e50-46c4-4e2c-94ec-daec6c596ac0?execution_contract="+CurrentAdhocStartContract,
		strings.NewReader(body),
	)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Auth-Type", "user")
	request.Header.Set("X-Auth-ID", "11")
	request.RemoteAddr = "10.0.0.8:43120"
	return request
}

func currentRegenerationRequest(body string) *http.Request {
	request := httptest.NewRequest(
		http.MethodPost,
		"/api/v2/elitea_core/regenerate/prompt_lib/7/30e0913e-10d4-43db-b8d0-c7b79480935a?execution_contract="+CurrentRegenerationContract,
		strings.NewReader(body),
	)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Auth-Type", "user")
	request.Header.Set("X-Auth-ID", "11")
	request.RemoteAddr = "10.0.0.8:43120"
	return request
}

func currentContinuationRequest(body string) *http.Request {
	request := httptest.NewRequest(
		http.MethodPost,
		"/api/v2/elitea_core/continue_predict/prompt_lib/7/8bc66e50-46c4-4e2c-94ec-daec6c596ac0?execution_contract="+CurrentContinuationContract,
		strings.NewReader(body),
	)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Auth-Type", "user")
	request.Header.Set("X-Auth-ID", "11")
	request.RemoteAddr = "10.0.0.8:43120"
	return request
}

func currentAuthorizationContinuationRequest(body string) *http.Request {
	request := httptest.NewRequest(
		http.MethodPost,
		"/api/v2/elitea_core/continue_predict/prompt_lib/7/8bc66e50-46c4-4e2c-94ec-daec6c596ac0?execution_contract="+CurrentAuthorizationContinuationContract,
		strings.NewReader(body),
	)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Auth-Type", "user")
	request.Header.Set("X-Auth-ID", "11")
	request.RemoteAddr = "10.0.0.8:43120"
	return request
}

func currentOutputContinuationRequest(body string) *http.Request {
	request := httptest.NewRequest(
		http.MethodPost,
		"/api/v2/elitea_core/continue_predict/prompt_lib/7/8bc66e50-46c4-4e2c-94ec-daec6c596ac0?execution_contract="+CurrentOutputLimitContinuationContract,
		strings.NewReader(body),
	)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Auth-Type", "user")
	request.Header.Set("X-Auth-ID", "11")
	request.RemoteAddr = "10.0.0.8:43120"
	return request
}

func validCurrentOutputContinuationBody() string {
	return `{
  "project_id":7,
  "conversation_uuid":"8bc66e50-46c4-4e2c-94ec-daec6c596ac0",
  "message_id":"30e0913e-10d4-43db-b8d0-c7b79480935a"
}`
}

func validCurrentContinuationBody() string {
	return `{
  "project_id":7,
  "conversation_uuid":"8bc66e50-46c4-4e2c-94ec-daec6c596ac0",
  "message_id":"30e0913e-10d4-43db-b8d0-c7b79480935a",
  "thread_id":"thread-current-1",
  "hitl_resume":true,
  "hitl_action":"edit",
  "hitl_value":"delete only merged branches",
  "hitl_decisions":[],
  "mcp_tokens":{},
  "ignored_mcp_servers":[],
  "user_declined_mcp_servers":[],
  "user_input":"edit"
}`
}

func validCurrentAuthorizationContinuationBody() string {
	return `{
  "project_id":7,
  "conversation_uuid":"8bc66e50-46c4-4e2c-94ec-daec6c596ac0",
  "message_id":"30e0913e-10d4-43db-b8d0-c7b79480935a",
  "thread_id":"thread-current-1",
  "hitl_resume":false,
  "hitl_decisions":[],
  "mcp_tokens":{"https://sharepoint.example.test":{"access_token":"runtime-secret"}},
  "ignored_mcp_servers":[],
  "user_declined_mcp_servers":[],
  "authorization_request_id":"tool-run-sharepoint-1",
  "authorization_action":"authorize"
}`
}

func validCurrentRegenerationBody() string {
	return `{
  "payload":{
    "user_input":"hello again",
    "llm_settings":{"model_name":"model","model_project_id":7},
    "attachments_info":[],
    "mcp_tokens":{}
  },
  "project_id":7,
  "participant_id":21,
  "conversation_uuid":"8bc66e50-46c4-4e2c-94ec-daec6c596ac0",
  "question_id":"ee92ccbd-3312-4c72-b20b-fddf224e7c0e",
  "message_id":"30e0913e-10d4-43db-b8d0-c7b79480935a",
  "stream_id":"30e0913e-10d4-43db-b8d0-c7b79480935a",
  "regeneration_id":"9fba0a08-5049-42bb-9019-c2f3df686010",
  "updated_items":[]
}`
}

func validCurrentStartBody() string {
	return `{"payload":{"user_input":"hello"},"project_id":7,"participant_id":21,"conversation_uuid":"8bc66e50-46c4-4e2c-94ec-daec6c596ac0","question_id":"ee92ccbd-3312-4c72-b20b-fddf224e7c0e","interaction_uuid":"31df012a-300d-4722-9be2-521d987c63a8","attachments_info":[],"mcp_tokens":{}}`
}

func validCurrentAdhocStartBody() string {
	return `{"payload":{"user_input":"hello from main chat"},"project_id":7,"conversation_uuid":"8bc66e50-46c4-4e2c-94ec-daec6c596ac0","question_id":"ee92ccbd-3312-4c72-b20b-fddf224e7c0e","interaction_uuid":"31df012a-300d-4722-9be2-521d987c63a8","attachments_info":[],"llm_settings":{"model_name":"model","model_project_id":7},"mcp_tokens":{}}`
}
