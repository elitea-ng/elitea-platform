package agentexecution

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
)

func TestCurrentAdhocStartBuildsCurrentMainChatInputAndTurn(t *testing.T) {
	toolJSON := json.RawMessage(`[{
  "id":51,"type":"aha","name":"product","description":"Aha",
  "author_id":11,"settings":{"selected_tools":["list_products"]},
  "meta":{},"toolkit_name":"product"
}]`)
	resolver := &currentApplicationResolverStub{adhocTarget: CurrentAdhocTarget{
		TargetParticipantID: 21,
		LLMSettings:         json.RawMessage(`{"model_name":"saved","model_project_id":7,"max_tokens":1024}`),
		Instructions:        "Project chat instructions\n\nUser defaults",
		Tools:               toolJSON,
		ChatHistory:         json.RawMessage(`[{"role":"user","content":[{"type":"text","text":"earlier"}],"additional_kwargs":{}}]`),
		ConversationMeta:    json.RawMessage(`{"persona":"qa","steps_limit":12,"internal_tools":["internal_mcp","ask_user"]}`),
	}}
	admittedAt := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	admissions := &currentApplicationAdmissionStub{outcome: executionapp.AdmissionOutcome{
		ExecutionID: "execution-adhoc", CommandID: "command-adhoc", Created: true,
		AdmittedAt: admittedAt, Deadline: admittedAt.Add(time.Minute),
	}}
	freezer := &currentApplicationVersionFreezerStub{}
	service, err := NewCurrentApplicationStartService(resolver, resolver, resolver, resolver, resolver, &currentAgentGuardrailStub{}, freezer, admissions)
	if err != nil {
		t.Fatal(err)
	}
	request := validCurrentAdhocStartRequest()

	outcome, err := service.StartCurrentAdhoc(context.Background(), request)
	if err != nil {
		t.Fatalf("StartCurrentAdhoc() error = %v", err)
	}
	if outcome.ExecutionID != "execution-adhoc" || outcome.CommandID != "command-adhoc" ||
		!outcome.Created || outcome.ResponseMessageID == "" || len(admissions.requests) != 1 {
		t.Fatalf("outcome=%+v admissions=%d", outcome, len(admissions.requests))
	}
	submitted := admissions.requests[0]
	if submitted.CapabilityID != executiondomain.AgentAdhocCapability ||
		submitted.SIOEvent != "chat_predict" || submitted.IdempotencyKey != request.QuestionID ||
		submitted.CurrentTurn != nil || submitted.CurrentAdhocTurn == nil ||
		submitted.CurrentAdhocTurn.Validate() != nil ||
		submitted.CurrentAdhocTurn.TargetParticipantID != resolver.adhocTarget.TargetParticipantID ||
		submitted.CurrentAdhocTurn.ResponseMessageID != outcome.ResponseMessageID {
		t.Fatalf("admission drifted: %+v", submitted)
	}
	if resolver.adhocCalls != 1 || len(resolver.adhocRequests) != 1 || len(freezer.calls) != 1 {
		t.Fatalf("resolver calls=%d requests=%d freezer=%d", resolver.adhocCalls, len(resolver.adhocRequests), len(freezer.calls))
	}
	if submitted.TriggerOrigin != "" {
		t.Fatalf("a composer start carried trigger origin %q", submitted.TriggerOrigin)
	}
	input := submitted.Input
	if input.GetThreadId() != request.ConversationUUID ||
		input.GetConversationId() != request.ConversationUUID ||
		input.GetExecutionGeneration() != request.QuestionID ||
		input.GetPersona() != "qa" || input.GetStepsLimit() != 12 ||
		!bytes.Equal(input.GetUserInput(), []byte(`"follow up"`)) ||
		!bytes.Equal(input.GetChatHistory(), resolver.adhocTarget.ChatHistory) ||
		!bytes.Equal(input.GetInternalTools(), []byte(`["ask_user"]`)) {
		t.Fatalf("ad-hoc input identity drifted: %+v", input)
	}
	var llm map[string]any
	if err := json.Unmarshal(input.GetLlm(), &llm); err != nil {
		t.Fatal(err)
	}
	kwargs, _ := llm["kwargs"].(map[string]any)
	if kwargs["model"] != "requested" || kwargs["model_project_id"] != float64(9) ||
		kwargs["temperature"] != float64(0.2) || kwargs["openai_compatible"] != false ||
		kwargs["stream"] != true {
		t.Fatalf("runtime model=%+v", kwargs)
	}
	if _, leaked := kwargs["api_key"]; leaked || bytes.Contains(input.GetLlm(), []byte("must-not-cross")) {
		t.Fatalf("caller credential leaked into durable model input: %s", input.GetLlm())
	}
	var application map[string]any
	if err := json.Unmarshal(input.GetApplication(), &application); err != nil ||
		application["instructions"] != resolver.adhocTarget.Instructions {
		t.Fatalf("application=%+v error=%v", application, err)
	}
	var tools []map[string]any
	if err := json.Unmarshal(input.GetTools(), &tools); err != nil || len(tools) != 1 ||
		tools[0]["type"] != "aha" {
		t.Fatalf("tools=%+v error=%v", tools, err)
	}
}

func TestCurrentAdhocStartRejectsMalformedConversationOptionsBeforeAdmission(t *testing.T) {
	resolver := &currentApplicationResolverStub{adhocTarget: CurrentAdhocTarget{
		TargetParticipantID: 21,
		LLMSettings:         json.RawMessage(`{"model_name":"saved","model_project_id":7}`),
		Tools:               json.RawMessage(`[]`),
		ChatHistory:         json.RawMessage(`[]`),
		ConversationMeta:    json.RawMessage(`{"persona":"untrusted-persona"}`),
	}}
	admissions := &currentApplicationAdmissionStub{}
	service, err := NewCurrentApplicationStartService(
		resolver, resolver, resolver, resolver, resolver, &currentAgentGuardrailStub{},
		&currentApplicationVersionFreezerStub{}, admissions,
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = service.StartCurrentAdhoc(context.Background(), validCurrentAdhocStartRequest())
	if err == nil || len(admissions.requests) != 0 {
		t.Fatalf("error=%v admissions=%d", err, len(admissions.requests))
	}
}

// The application path refuses an authored step limit above the runtime
// ceiling (maxCurrentAgentStepLimit) so the start fails with a stated reason
// instead of the native runtime's opaque invalid_profile. The adhoc path
// forwards the same field from conversation meta and must refuse at the same
// bound — this pins the cap that closed that divergence.
func TestCurrentAdhocStartRejectsAStepLimitAboveTheRuntimeCeiling(t *testing.T) {
	resolver := &currentApplicationResolverStub{adhocTarget: CurrentAdhocTarget{
		TargetParticipantID: 21,
		LLMSettings:         json.RawMessage(`{"model_name":"saved","model_project_id":7}`),
		Tools:               json.RawMessage(`[]`),
		ChatHistory:         json.RawMessage(`[]`),
		ConversationMeta:    json.RawMessage(`{"steps_limit":1025}`),
	}}
	admissions := &currentApplicationAdmissionStub{}
	service, err := NewCurrentApplicationStartService(
		resolver, resolver, resolver, resolver, resolver, &currentAgentGuardrailStub{},
		&currentApplicationVersionFreezerStub{}, admissions,
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = service.StartCurrentAdhoc(context.Background(), validCurrentAdhocStartRequest())
	if !errors.Is(err, ErrUnsupportedCurrentAgentStart) || len(admissions.requests) != 0 {
		t.Fatalf("error=%v admissions=%d, want ErrUnsupportedCurrentAgentStart before admission", err, len(admissions.requests))
	}
}

func TestCurrentAdhocStartAcceptsCurrentDefaultMaxTokensSentinel(t *testing.T) {
	resolver := &currentApplicationResolverStub{adhocTarget: CurrentAdhocTarget{
		TargetParticipantID: 21,
		LLMSettings:         json.RawMessage(`{"model_name":"saved","model_project_id":7}`),
		Tools:               json.RawMessage(`[]`), ChatHistory: json.RawMessage(`[]`),
		ConversationMeta: json.RawMessage(`{}`),
	}}
	admissions := &currentApplicationAdmissionStub{outcome: executionapp.AdmissionOutcome{
		ExecutionID: "execution-adhoc", CommandID: "command-adhoc", Created: true,
	}}
	freezer := &currentApplicationVersionFreezerStub{result: json.RawMessage(`{
  "llm_settings":{"model_name":"requested","model_project_id":9,"max_tokens":-1,"openai_compatible":true},
  "tools":[]
}`)}
	service, err := NewCurrentApplicationStartService(resolver, resolver, resolver, resolver, resolver, &currentAgentGuardrailStub{}, freezer, admissions)
	if err != nil {
		t.Fatal(err)
	}
	request := validCurrentAdhocStartRequest()
	request.LLMSettings = json.RawMessage(`{
  "model_name":"requested","model_project_id":9,"max_tokens":-1,"reasoning_effort":"none"
}`)
	if _, err = service.StartCurrentAdhoc(context.Background(), request); err != nil {
		t.Fatalf("StartCurrentAdhoc() error = %v", err)
	}
	if len(freezer.calls) != 1 || !bytes.Contains(freezer.calls[0].VersionDetails, []byte(`"max_tokens":-1`)) ||
		len(admissions.requests) != 1 || !bytes.Contains(admissions.requests[0].Input.GetLlm(), []byte(`"max_tokens":-1`)) {
		t.Fatalf("freezer=%s runtime=%s", freezer.calls[0].VersionDetails, admissions.requests[0].Input.GetLlm())
	}
}

func validCurrentAdhocStartRequest() CurrentAdhocStartRequest {
	return CurrentAdhocStartRequest{
		ProjectID: 7, ActorUserID: 11,
		ConversationUUID: "8bc66e50-46c4-4e2c-94ec-daec6c596ac0",
		QuestionID:       "e35ed323-212a-4b79-a6d4-8fac7cbeb9f6",
		UserInput:        "follow up",
		InteractionUUID:  "31df012a-300d-4722-9be2-521d987c63a8",
		LLMSettings: json.RawMessage(`{
  "model_name":"requested","model_project_id":9,"temperature":0.2,
  "api_key":"must-not-cross","base_url":"https://caller.invalid"
}`),
	}
}
