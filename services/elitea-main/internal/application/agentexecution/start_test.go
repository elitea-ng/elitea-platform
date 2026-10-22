package agentexecution

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	"google.golang.org/protobuf/proto"
)

type currentApplicationResolverStub struct {
	target               CurrentApplicationTarget
	targets              []CurrentApplicationTarget
	adhocTarget          CurrentAdhocTarget
	regenerationTarget   CurrentRegenerationTarget
	continuationTarget   CurrentContinuationTarget
	err                  error
	calls                int
	adhocCalls           int
	adhocRequests        []CurrentAdhocStartRequest
	regenerationRequests []CurrentRegenerationResolveRequest
	continuationRequests []CurrentContinuationResolveRequest
}

type currentApplicationVersionFreezerStub struct {
	result json.RawMessage
	err    error
	calls  []CurrentApplicationVersionFreezeRequest
}

func (stub *currentApplicationVersionFreezerStub) FreezeCurrentApplicationVersion(
	_ context.Context,
	request CurrentApplicationVersionFreezeRequest,
) (json.RawMessage, error) {
	request.VersionDetails = bytes.Clone(request.VersionDetails)
	stub.calls = append(stub.calls, request)
	if stub.result != nil {
		return bytes.Clone(stub.result), stub.err
	}
	var version map[string]any
	if err := json.Unmarshal(request.VersionDetails, &version); err == nil {
		if settings, ok := version["llm_settings"].(map[string]any); ok {
			settings["openai_compatible"] = false
			encoded, marshalErr := json.Marshal(version)
			if marshalErr == nil {
				return encoded, stub.err
			}
		}
	}
	return bytes.Clone(request.VersionDetails), stub.err
}

func (stub *currentApplicationResolverStub) ResolveCurrentApplication(
	_ context.Context,
	_ CurrentApplicationStartRequest,
) (CurrentApplicationTarget, error) {
	call := stub.calls
	stub.calls++
	if call < len(stub.targets) {
		return stub.targets[call], stub.err
	}
	return stub.target, stub.err
}

func (stub *currentApplicationResolverStub) ResolveCurrentAdhoc(
	_ context.Context,
	request CurrentAdhocStartRequest,
) (CurrentAdhocTarget, error) {
	stub.adhocCalls++
	request.LLMSettings = bytes.Clone(request.LLMSettings)
	stub.adhocRequests = append(stub.adhocRequests, request)
	return stub.adhocTarget, stub.err
}

func (stub *currentApplicationResolverStub) ResolveCurrentRegeneration(
	_ context.Context,
	request CurrentRegenerationResolveRequest,
) (CurrentRegenerationTarget, error) {
	stub.regenerationRequests = append(stub.regenerationRequests, request)
	return stub.regenerationTarget, stub.err
}

func (stub *currentApplicationResolverStub) ResolveCurrentContinuation(
	_ context.Context,
	request CurrentContinuationResolveRequest,
) (CurrentContinuationTarget, error) {
	stub.continuationRequests = append(stub.continuationRequests, request)
	return stub.continuationTarget, stub.err
}

type currentApplicationAdmissionStub struct {
	requests []SubmitRequest
	outcome  executionapp.AdmissionOutcome
	err      error
}

func (stub *currentApplicationAdmissionStub) Submit(
	_ context.Context,
	request SubmitRequest,
) (executionapp.AdmissionOutcome, error) {
	request.Input = proto.Clone(request.Input).(*runtimeAgentInput)
	request.CurrentTurn = request.CurrentTurn.Clone()
	request.CurrentAdhocTurn = request.CurrentAdhocTurn.Clone()
	request.CurrentRegenerateTurn = request.CurrentRegenerateTurn.Clone()
	request.CurrentContinueTurn = request.CurrentContinueTurn.Clone()
	stub.requests = append(stub.requests, request)
	return stub.outcome, stub.err
}

// Alias keeps the clone assertion readable without leaking generated package
// details into the test expectations below.
type runtimeAgentInput = runtimev1.AgentExecutionInputV1

func TestCurrentApplicationStartBuildsAuthoritativeParityInputAndTurn(t *testing.T) {
	resolver := &currentApplicationResolverStub{target: CurrentApplicationTarget{
		ApplicationID: 31, ApplicationVersionID: 41,
		Variables:      json.RawMessage(`[{"name":"region","value":"eu"}]`),
		VersionDetails: json.RawMessage(`{"id":41,"application_id":31,"agent_type":"agent","instructions":"Be concise","llm_settings":{"model_name":"test"},"meta":{},"tools":[]}`),
		ChatHistory:    json.RawMessage(`[{"role":"user","content":[{"type":"text","text":"earlier"}],"additional_kwargs":{}}]`),
		InternalTools:  json.RawMessage(`["internal_mcp","ask_user"]`),
	}}
	admittedAt := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	admissions := &currentApplicationAdmissionStub{outcome: executionapp.AdmissionOutcome{
		ExecutionID: "execution-1", CommandID: "command-1", Created: true,
		AdmittedAt: admittedAt, Deadline: admittedAt.Add(time.Minute),
	}}
	freezer := &currentApplicationVersionFreezerStub{}
	service, err := NewCurrentApplicationStartService(resolver, resolver, resolver, resolver, &currentAgentGuardrailStub{}, freezer, admissions)
	if err != nil {
		t.Fatal(err)
	}
	request := validCurrentApplicationStartRequest()

	outcome, err := service.StartCurrentApplication(context.Background(), request)
	if err != nil {
		t.Fatalf("StartCurrentApplication() error = %v", err)
	}
	if outcome.ExecutionID != "execution-1" || outcome.CommandID != "command-1" ||
		!outcome.Created || outcome.ResponseMessageID == "" || len(admissions.requests) != 1 {
		t.Fatalf("outcome=%+v admissions=%d", outcome, len(admissions.requests))
	}

	submitted := admissions.requests[0]
	if submitted.CapabilityID != executiondomain.AgentApplicationCapability ||
		submitted.SIOEvent != "chat_predict" || submitted.IdempotencyKey != request.QuestionID ||
		submitted.Identity.TenantID != "7" || submitted.Identity.ResourceProjectID != "7" ||
		submitted.Identity.ProjectionProjectID != "7" || submitted.Identity.ActorID != "11" {
		t.Fatalf("admission identity or capability drifted: %+v", submitted)
	}
	if submitted.CurrentTurn == nil || submitted.CurrentTurn.Validate() != nil ||
		submitted.CurrentTurn.ResponseMessageID != outcome.ResponseMessageID ||
		submitted.CurrentTurn.QuestionID != request.QuestionID ||
		submitted.CurrentTurn.UserInput != request.UserInput {
		t.Fatalf("current turn=%+v outcome=%+v", submitted.CurrentTurn, outcome)
	}
	if len(freezer.calls) != 1 || freezer.calls[0].ProjectID != 7 ||
		freezer.calls[0].ActorUserID != 11 {
		t.Fatalf("version freezer calls=%+v", freezer.calls)
	}
	if !bytes.Equal(submitted.Input.GetNextInputSuggestion(), []byte(`null`)) {
		t.Fatalf("next input suggestion=%s", submitted.Input.GetNextInputSuggestion())
	}
	if submitted.Input.GetSchemaRevision() != "elitea.runtime.agent-execution-input.v1" ||
		submitted.Input.GetThreadId() != request.ConversationUUID ||
		submitted.Input.GetConversationId() != request.ConversationUUID ||
		submitted.Input.GetExecutionGeneration() != request.QuestionID ||
		!bytes.Equal(submitted.Input.GetUserInput(), []byte(`"hello"`)) ||
		!bytes.Equal(submitted.Input.GetLlm(), []byte(`{"kwargs":{"openai_compatible":false}}`)) ||
		!bytes.Equal(submitted.Input.GetChatHistory(), resolver.target.ChatHistory) ||
		!bytes.Equal(submitted.Input.GetInternalTools(), []byte(`["ask_user"]`)) ||
		!bytes.Equal(submitted.Input.GetMcpTokens(), []byte(`{}`)) {
		t.Fatalf("agent input drifted: %+v", submitted.Input)
	}
	var application map[string]any
	if err := json.Unmarshal(submitted.Input.GetApplication(), &application); err != nil ||
		application["id"] != float64(31) || application["version_id"] != float64(41) ||
		application["version_details"].(map[string]any)["instructions"] != "Be concise" {
		t.Fatalf("application=%+v error=%v", application, err)
	}
}

func TestCurrentApplicationStartPreservesPipelineYAMLInTheExistingApplicationContract(t *testing.T) {
	pipelineYAML := "nodes:\n  - id: draft\n    type: llm\n  - id: review\n    type: hitl\nedges:\n  - from: draft\n    to: review\n"
	versionDetails, err := json.Marshal(map[string]any{
		"id": 41, "application_id": 31, "agent_type": "pipeline",
		"instructions": pipelineYAML,
		"llm_settings": map[string]any{"model_name": "test"},
		"meta":         map[string]any{}, "tools": []any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	resolver := &currentApplicationResolverStub{target: CurrentApplicationTarget{
		ApplicationID: 31, ApplicationVersionID: 41,
		Variables: json.RawMessage(`[]`), VersionDetails: versionDetails,
		ChatHistory: json.RawMessage(`[]`),
	}}
	admissions := &currentApplicationAdmissionStub{outcome: executionapp.AdmissionOutcome{
		ExecutionID: "pipeline-execution", CommandID: "pipeline-command", Created: true,
	}}
	service, err := NewCurrentApplicationStartService(
		resolver, resolver, resolver, resolver, &currentAgentGuardrailStub{},
		&currentApplicationVersionFreezerStub{}, admissions)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := service.StartCurrentApplication(
		context.Background(), validCurrentApplicationStartRequest(),
	); err != nil {
		t.Fatalf("StartCurrentApplication() pipeline error = %v", err)
	}
	if len(admissions.requests) != 1 {
		t.Fatalf("pipeline admissions = %d", len(admissions.requests))
	}
	var application struct {
		VersionDetails struct {
			AgentType    string `json:"agent_type"`
			Instructions string `json:"instructions"`
		} `json:"version_details"`
	}
	if err := json.Unmarshal(admissions.requests[0].Input.GetApplication(), &application); err != nil {
		t.Fatal(err)
	}
	if application.VersionDetails.AgentType != "pipeline" ||
		application.VersionDetails.Instructions != pipelineYAML {
		t.Fatalf("pipeline application snapshot = %+v", application)
	}
}

// decodedCurrentApplicationVersionForTest mirrors the production entry point:
// currentApplicationInput decodes the frozen document ONCE and hands the map
// to each reader, so the readers' unit tests feed them the same shape.
func decodedCurrentApplicationVersionForTest(t *testing.T, raw string) map[string]any {
	t.Helper()
	version, err := decodeCurrentApplicationVersion(json.RawMessage(raw))
	if err != nil {
		t.Fatalf("decode fixture %s: %v", raw, err)
	}
	return version
}

func TestCurrentApplicationRuntimeLLMBindsDerivedCompatibilityOnly(t *testing.T) {
	result, err := currentApplicationRuntimeLLM(decodedCurrentApplicationVersionForTest(t, `{
  "llm_settings":{"model_name":"model","model_project_id":7,"openai_compatible":true,"temperature":0.6}
}`))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(result, []byte(`{"kwargs":{"openai_compatible":true}}`)) {
		t.Fatalf("runtime llm=%s", result)
	}
	if _, err := currentApplicationRuntimeLLM(decodedCurrentApplicationVersionForTest(t, `{"llm_settings":{"model_name":"model"}}`)); !errors.Is(err, ErrUnsupportedCurrentAgentStart) {
		t.Fatalf("missing derived compatibility error=%v", err)
	}
}

func TestCurrentApplicationStartIsDeterministicAcrossIdempotentRetries(t *testing.T) {
	resolver := &currentApplicationResolverStub{target: CurrentApplicationTarget{
		ApplicationID: 31, ApplicationVersionID: 41, Variables: json.RawMessage(`[]`),
		VersionDetails: json.RawMessage(`{"id":41,"application_id":31,"agent_type":"agent","llm_settings":{"model_name":"test"},"meta":{},"tools":[]}`),
		ChatHistory:    json.RawMessage(`[]`),
	}}
	admittedAt := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	admissions := &currentApplicationAdmissionStub{outcome: executionapp.AdmissionOutcome{
		ExecutionID: "execution-1", CommandID: "command-1", Created: false,
		AdmittedAt: admittedAt, Deadline: admittedAt.Add(time.Minute),
	}}
	service, err := NewCurrentApplicationStartService(
		resolver,
		resolver,
		resolver,
		resolver,
		&currentAgentGuardrailStub{},
		&currentApplicationVersionFreezerStub{},
		admissions,
	)
	if err != nil {
		t.Fatal(err)
	}
	request := validCurrentApplicationStartRequest()

	first, err := service.StartCurrentApplication(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.StartCurrentApplication(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if first.ResponseMessageID != second.ResponseMessageID || len(admissions.requests) != 2 ||
		!proto.Equal(admissions.requests[0].Input, admissions.requests[1].Input) ||
		!reflect.DeepEqual(admissions.requests[0].CurrentTurn, admissions.requests[1].CurrentTurn) {
		t.Fatal("same current turn produced different durable admission material")
	}
}

func TestCurrentApplicationStartKeepsStableThreadAndProjectsHistoryAcrossDistinctTurns(t *testing.T) {
	firstTarget := CurrentApplicationTarget{
		ApplicationID: 31, ApplicationVersionID: 41, Variables: json.RawMessage(`[]`),
		VersionDetails: json.RawMessage(`{"id":41,"application_id":31,"agent_type":"agent","llm_settings":{"model_name":"test"},"meta":{},"tools":[]}`),
		ChatHistory:    json.RawMessage(`[]`),
	}
	secondHistory := json.RawMessage(`[{"role":"user","content":[{"type":"text","text":"hello"}],"additional_kwargs":{}},{"role":"assistant","content":[{"type":"text","text":"hi"}],"additional_kwargs":{}}]`)
	secondTarget := firstTarget
	secondTarget.ChatHistory = secondHistory
	resolver := &currentApplicationResolverStub{targets: []CurrentApplicationTarget{firstTarget, secondTarget}}
	admissions := &currentApplicationAdmissionStub{outcome: executionapp.AdmissionOutcome{
		ExecutionID: "execution", CommandID: "command", Created: true,
		AdmittedAt: time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC),
		Deadline:   time.Date(2026, 8, 3, 12, 1, 0, 0, time.UTC),
	}}
	service, err := NewCurrentApplicationStartService(
		resolver,
		resolver,
		resolver,
		resolver,
		&currentAgentGuardrailStub{},
		&currentApplicationVersionFreezerStub{},
		admissions,
	)
	if err != nil {
		t.Fatal(err)
	}
	first := validCurrentApplicationStartRequest()
	second := first
	second.QuestionID = "e35ed323-212a-4b79-a6d4-8fac7cbeb9f6"
	second.UserInput = "follow up"

	firstOutcome, err := service.StartCurrentApplication(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	secondOutcome, err := service.StartCurrentApplication(context.Background(), second)
	if err != nil {
		t.Fatal(err)
	}
	if resolver.calls != 2 || len(admissions.requests) != 2 {
		t.Fatalf("resolver calls=%d admissions=%d", resolver.calls, len(admissions.requests))
	}
	firstAdmission := admissions.requests[0]
	secondAdmission := admissions.requests[1]
	if firstAdmission.Input.GetThreadId() != first.ConversationUUID ||
		secondAdmission.Input.GetThreadId() != first.ConversationUUID ||
		firstAdmission.Input.GetConversationId() != first.ConversationUUID ||
		secondAdmission.Input.GetConversationId() != first.ConversationUUID ||
		!bytes.Equal(firstAdmission.Input.GetChatHistory(), []byte(`[]`)) ||
		!bytes.Equal(secondAdmission.Input.GetChatHistory(), secondHistory) {
		t.Fatal("distinct turns did not reuse the conversation thread and current chat history")
	}
	if firstAdmission.IdempotencyKey == secondAdmission.IdempotencyKey ||
		firstAdmission.CurrentTurn.QuestionID == secondAdmission.CurrentTurn.QuestionID ||
		firstOutcome.ResponseMessageID == secondOutcome.ResponseMessageID ||
		!bytes.Equal(secondAdmission.Input.GetUserInput(), []byte(`"follow up"`)) {
		t.Fatal("distinct turns lost their independent execution and message identities")
	}
}

func TestCurrentApplicationStartRejectsUnsupportedTargetBeforeAdmission(t *testing.T) {
	resolver := &currentApplicationResolverStub{target: CurrentApplicationTarget{
		ApplicationID: 31, ApplicationVersionID: 41, Variables: json.RawMessage(`{}`),
		VersionDetails: json.RawMessage(`{"id":41}`),
		ChatHistory:    json.RawMessage(`[]`),
	}}
	admissions := &currentApplicationAdmissionStub{}
	service, err := NewCurrentApplicationStartService(
		resolver,
		resolver,
		resolver,
		resolver,
		&currentAgentGuardrailStub{},
		&currentApplicationVersionFreezerStub{},
		admissions,
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = service.StartCurrentApplication(context.Background(), validCurrentApplicationStartRequest())
	if !errors.Is(err, ErrUnsupportedCurrentAgentStart) || len(admissions.requests) != 0 {
		t.Fatalf("error=%v admissions=%d", err, len(admissions.requests))
	}
}

func validCurrentApplicationStartRequest() CurrentApplicationStartRequest {
	return CurrentApplicationStartRequest{
		ProjectID: 7, ActorUserID: 11,
		ConversationUUID:    "8bc66e50-46c4-4e2c-94ec-daec6c596ac0",
		TargetParticipantID: 21,
		QuestionID:          "ee92ccbd-3312-4c72-b20b-fddf224e7c0e",
		UserInput:           "hello",
		InteractionUUID:     "31df012a-300d-4722-9be2-521d987c63a8",
	}
}
