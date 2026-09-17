package agentexecution

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/contextsettings"
	"google.golang.org/protobuf/proto"
)

type contextPolicyStub struct {
	strategy        contextsettings.Strategy
	frozen          FrozenContextPolicy
	err             error
	reads, restores int
}

func (s *contextPolicyStub) ContextStrategy(ctx context.Context, project, actor int64, conversation string) (contextsettings.Strategy, error) {
	s.reads++
	if project != 7 || actor != 11 || conversation != validCurrentApplicationStartRequest().ConversationUUID {
		return contextsettings.Strategy{}, ErrInvalidCurrentAgentStart
	}
	if err := ctx.Err(); err != nil {
		return contextsettings.Strategy{}, err
	}
	return s.strategy, s.err
}
func (s *contextPolicyStub) ContinuationContextPolicy(_ context.Context, project, actor int64, conversation, response, generation string) (FrozenContextPolicy, error) {
	s.restores++
	if project != 7 || actor != 11 || conversation != validCurrentApplicationStartRequest().ConversationUUID || response != validCurrentRegenerationRequest().ResponseMessageID || generation != validCurrentRegenerationRequest().RegenerationID {
		return FrozenContextPolicy{}, ErrInvalidCurrentAgentStart
	}
	return s.frozen, s.err
}

func contextDeliveryFixture(t *testing.T, source *contextPolicyStub) (*CurrentApplicationStartService, *currentApplicationAdmissionStub, *currentApplicationResolverStub) {
	t.Helper()
	request := validCurrentApplicationStartRequest()
	resolver := &currentApplicationResolverStub{
		target:             CurrentApplicationTarget{ApplicationID: 31, ApplicationVersionID: 41, Variables: json.RawMessage(`[]`), ChatHistory: json.RawMessage(`[]`), VersionDetails: json.RawMessage(`{"id":41,"application_id":31,"agent_type":"agent","instructions":"Keep this instruction","llm_settings":{"model_name":"model","model_project_id":7},"tools":[]}`)},
		adhocTarget:        CurrentAdhocTarget{TargetParticipantID: 21, LLMSettings: json.RawMessage(`{"model_name":"model","model_project_id":7}`), Tools: json.RawMessage(`[]`), ChatHistory: json.RawMessage(`[]`), ConversationMeta: json.RawMessage(`{}`)},
		regenerationTarget: CurrentRegenerationTarget{Kind: CurrentRegenerationApplication, ConversationUUID: request.ConversationUUID, TargetParticipantID: 21, QuestionID: request.QuestionID, UserInput: request.UserInput},
		continuationTarget: CurrentContinuationTarget{ContinuationKind: CurrentContinuationOutputLimit, Kind: CurrentRegenerationApplication, TargetParticipantID: 21, QuestionID: request.QuestionID, UserInput: request.UserInput, ThreadID: request.ConversationUUID, ExecutionGeneration: validCurrentRegenerationRequest().RegenerationID, OutputLimitSequence: 1},
	}
	admissions := &currentApplicationAdmissionStub{}
	freezer, _ := summaryFreezer(t)
	service, err := NewCurrentApplicationStartService(resolver, resolver, resolver, resolver, resolver, &currentAgentGuardrailStub{}, freezer, admissions)
	if err != nil {
		t.Fatal(err)
	}
	service.WithContextPolicy(source)
	return service, admissions, resolver
}

func TestContextPolicyReachesStartAndRegenerateWithAuthorizedSummary(t *testing.T) {
	for _, mode := range []string{"application", "adhoc", "regenerate_application", "regenerate_adhoc"} {
		t.Run(mode, func(t *testing.T) {
			source := &contextPolicyStub{strategy: contextsettings.Resolve([]byte(`{"budget_mode":"full","preserve_recent_messages":9,"summary_llm_settings":{"model_name":"claude-summary","model_project_id":1,"max_tokens":1024}}`), contextsettings.UserDefaults{})}
			service, admissions, resolver := contextDeliveryFixture(t, source)
			var err error
			switch mode {
			case "application":
				_, err = service.StartCurrentApplication(t.Context(), validCurrentApplicationStartRequest())
			case "adhoc":
				request := validCurrentAdhocStartRequest()
				request.LLMSettings = json.RawMessage(`{}`)
				_, err = service.StartCurrentAdhoc(t.Context(), request)
			default:
				if mode == "regenerate_adhoc" {
					resolver.regenerationTarget.Kind = CurrentRegenerationAdhoc
				}
				_, err = service.RegenerateCurrentAgent(t.Context(), validCurrentRegenerationRequest())
			}
			if err != nil || len(admissions.requests) != 1 {
				t.Fatalf("delivery failed: %v, admissions=%d", err, len(admissions.requests))
			}
			input := admissions.requests[0].Input
			var settings map[string]any
			if err := json.Unmarshal(input.ContextSettings, &settings); err != nil {
				t.Fatal(err)
			}
			if settings["budget_mode"] != "full" || settings["max_context_tokens"] != nil || settings["preserve_recent_messages"] != float64(9) || settings["summary_llm_settings"] != nil {
				t.Fatalf("incorrect worker projection: %s", input.ContextSettings)
			}
			if input.SummaryModel == nil || input.SummaryModel.ModelContextLimits.ContextWindowTokens != 32000 || input.ModelContextLimits == nil {
				t.Fatal("authorized limits missing")
			}
			var model map[string]any
			if err := json.Unmarshal(input.SummaryModel.LlmSettings, &model); err != nil {
				t.Fatal(err)
			}
			if model["model_name"] != "claude-summary" || model["max_tokens"] != float64(1024) || source.reads != 1 || source.restores != 0 {
				t.Fatalf("incorrect summary or source: %v, %+v", model, source)
			}
		})
	}
}

func TestContinuationRestoresFrozenPolicyWithoutReadingChangedDefaults(t *testing.T) {
	for _, kind := range []CurrentRegenerationKind{CurrentRegenerationApplication, CurrentRegenerationAdhoc} {
		source := &contextPolicyStub{strategy: contextsettings.Resolve([]byte(`{"enabled":false,"budget_mode":"full"}`), contextsettings.UserDefaults{}), frozen: FrozenContextPolicy{Settings: json.RawMessage(`{"enabled":true,"budget_mode":"balanced","preserve_recent_messages":9}`), SummaryModel: &runtimev1.SummaryModelSnapshotV1{LlmSettings: []byte(`{"model_name":"prior-summary"}`), ModelContextLimits: &runtimev1.ModelContextLimitsV1{ContextWindowTokens: 32000, MaxOutputTokens: 4000}}}}
		service, admissions, resolver := contextDeliveryFixture(t, source)
		resolver.continuationTarget.Kind = kind
		request := CurrentContinuationRequest{ProjectID: 7, ActorUserID: 11, ConversationUUID: validCurrentApplicationStartRequest().ConversationUUID, ResponseMessageID: validCurrentRegenerationRequest().ResponseMessageID, Kind: CurrentContinuationOutputLimit}
		if _, err := service.ContinueCurrentAgent(t.Context(), request); err != nil {
			t.Fatal(err)
		}
		if len(admissions.requests) != 1 || source.reads != 0 || source.restores != 1 {
			t.Fatalf("continuation read new settings: %+v", source)
		}
		input := admissions.requests[0].Input
		if string(input.ContextSettings) != string(source.frozen.Settings) || !proto.Equal(input.SummaryModel, source.frozen.SummaryModel) {
			t.Fatal("continuation changed frozen policy")
		}
	}
}

func TestContextPolicyFailurePreventsAdmission(t *testing.T) {
	for _, failure := range []error{context.Canceled, errors.New("settings unavailable")} {
		source := &contextPolicyStub{strategy: contextsettings.DefaultStrategy(), err: failure}
		service, admissions, _ := contextDeliveryFixture(t, source)
		if _, err := service.StartCurrentApplication(t.Context(), validCurrentApplicationStartRequest()); !errors.Is(err, failure) || len(admissions.requests) != 0 {
			t.Fatalf("failure ignored: %v", err)
		}
	}
}
