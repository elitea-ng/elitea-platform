package agentexecution

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestSessionTokensStayInExecutionInputForStartAndRegeneration(t *testing.T) {
	for _, kind := range []string{"application", "adhoc", "regenerate_application", "regenerate_adhoc"} {
		t.Run(kind, func(t *testing.T) {
			regenerationKind := CurrentRegenerationApplication
			if kind == "regenerate_adhoc" {
				regenerationKind = CurrentRegenerationAdhoc
			}
			resolver := &currentApplicationResolverStub{
				target: CurrentApplicationTarget{
					ApplicationID: 31, ApplicationVersionID: 41, Variables: json.RawMessage(`[]`),
					VersionDetails: json.RawMessage(`{"id":41,"application_id":31,"agent_type":"agent","instructions":"Be concise","llm_settings":{"model_name":"test"},"meta":{},"tools":[]}`),
					ChatHistory:    json.RawMessage(`[]`),
				},
				adhocTarget: CurrentAdhocTarget{
					TargetParticipantID: 21, LLMSettings: json.RawMessage(`{"model_name":"test","model_project_id":7}`),
					Tools: json.RawMessage(`[]`), ChatHistory: json.RawMessage(`[]`), ConversationMeta: json.RawMessage(`{}`),
				},
				regenerationTarget: CurrentRegenerationTarget{
					Kind: regenerationKind, ConversationUUID: "8bc66e50-46c4-4e2c-94ec-daec6c596ac0",
					TargetParticipantID: 21, QuestionID: "ee92ccbd-3312-4c72-b20b-fddf224e7c0e", UserInput: "original question",
				},
			}
			admissions := &currentApplicationAdmissionStub{}
			service, err := NewCurrentApplicationStartService(resolver, resolver, resolver, resolver, resolver,
				&currentAgentGuardrailStub{}, &currentApplicationVersionFreezerStub{}, admissions)
			if err != nil {
				t.Fatal(err)
			}
			tokens := json.RawMessage(`{"credential:https://issuer.example":{"access_token":"test-access","session_id":"session"}}`)
			switch kind {
			case "application":
				request := validCurrentApplicationStartRequest()
				request.MCPTokens = tokens
				_, err = service.StartCurrentApplication(context.Background(), request)
			case "adhoc":
				request := validCurrentAdhocStartRequest()
				request.MCPTokens = tokens
				_, err = service.StartCurrentAdhoc(context.Background(), request)
			default:
				request := validCurrentRegenerationRequest()
				request.MCPTokens = tokens
				_, err = service.RegenerateCurrentAgent(context.Background(), request)
			}
			if err != nil || len(admissions.requests) != 1 {
				t.Fatalf("admission failed: %v", err)
			}
			input := admissions.requests[0].Input
			if !bytes.Equal(input.McpTokens, tokens) || bytes.Contains(input.ChatHistory, []byte("test-access")) {
				t.Fatal("session token input boundary changed")
			}
		})
	}
}

func TestSessionTokenValidationAndOwnership(t *testing.T) {
	for _, raw := range []string{"", "null", "{}", `{"key":{"access_token":"value"}}`} {
		tokens := json.RawMessage(raw)
		if !validCurrentMCPTokens(tokens) {
			t.Fatalf("valid token object rejected: %q", raw)
		}
		copy := currentMCPTokens(tokens)
		if len(tokens) > 0 {
			tokens[0] = '!'
			if copy[0] == '!' {
				t.Fatal("token input was not copied")
			}
		}
	}
	for _, raw := range []string{"[]", `"token"`, "42", "{", `{"key":"` + strings.Repeat("x", 64*1024) + `"}`} {
		if validCurrentMCPTokens(json.RawMessage(raw)) {
			t.Fatal("invalid or oversized token map accepted")
		}
		app, adhoc, regen := validCurrentApplicationStartRequest(), validCurrentAdhocStartRequest(), validCurrentRegenerationRequest()
		app.MCPTokens, adhoc.MCPTokens, regen.MCPTokens = json.RawMessage(raw), json.RawMessage(raw), json.RawMessage(raw)
		if app.Validate() == nil || adhoc.Validate() == nil || regen.Validate() == nil {
			t.Fatal("request validation accepted invalid session tokens")
		}
	}
}
