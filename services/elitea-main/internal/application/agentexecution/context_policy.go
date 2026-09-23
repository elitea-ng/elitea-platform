package agentexecution

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/contextsettings"
	"google.golang.org/protobuf/proto"
)

// CurrentContextPolicySource reads product settings and immutable admitted inputs.
// It does not read or write worker checkpoints.
type CurrentContextPolicySource interface {
	ContextStrategy(context.Context, int64, int64, string) (contextsettings.Strategy, error)
	ContinuationContextPolicy(context.Context, int64, int64, string, string, string) (FrozenContextPolicy, error)
}

type FrozenContextPolicy struct {
	TaskLLM      json.RawMessage
	Settings     json.RawMessage
	SummaryModel *runtimev1.SummaryModelSnapshotV1
}

// WithContextPolicy wires settings delivery before the service accepts requests.
// Unwired consumers retain the historical empty-input contract.
func (service *CurrentApplicationStartService) WithContextPolicy(source CurrentContextPolicySource) *CurrentApplicationStartService {
	service.contextPolicy = source
	return service
}

func (service *CurrentApplicationStartService) freezeVersionWithContext(
	ctx context.Context, request CurrentApplicationVersionFreezeRequest, conversationID string,
) (json.RawMessage, json.RawMessage, error) {
	settings := json.RawMessage(`{}`)
	if service.contextPolicy != nil {
		strategy, err := service.contextPolicy.ContextStrategy(ctx, int64(request.ProjectID), int64(request.ActorUserID), conversationID)
		if err != nil {
			return nil, nil, fmt.Errorf("resolve execution context policy: %w", err)
		}
		runtimeSettings, summary, fieldErr := strategy.Runtime()
		if fieldErr != nil {
			return nil, nil, unsupportedStartBecause("context policy validation", fieldErr)
		}
		settings, err = json.Marshal(runtimeSettings)
		if err != nil {
			return nil, nil, ErrUnsupportedCurrentAgentStart
		}
		request.SummaryLLMSettings = summary
	}
	frozen, err := service.freezer.FreezeCurrentApplicationVersion(ctx, request)
	return frozen, settings, err
}

func (service *CurrentApplicationStartService) loadContinuationContext(
	ctx context.Context, request CurrentContinuationRequest, target CurrentContinuationTarget,
) (*FrozenContextPolicy, error) {
	if service.contextPolicy == nil {
		return nil, nil
	}
	policy, err := service.contextPolicy.ContinuationContextPolicy(
		ctx, request.ProjectID, request.ActorUserID, request.ConversationUUID,
		request.ResponseMessageID, target.ExecutionGeneration,
	)
	if err != nil {
		return nil, fmt.Errorf("restore admitted context policy: %w", err)
	}
	if !validJSONObject(policy.Settings) {
		return nil, ErrUnsupportedCurrentAgentStart
	}
	return &policy, nil
}

func restoreContinuationContext(policy *FrozenContextPolicy, input *runtimev1.AgentExecutionInputV1) {
	if policy == nil {
		return
	}
	input.ContextSettings = bytes.Clone(policy.Settings)
	input.SummaryModel = nil
	if policy.SummaryModel != nil {
		input.SummaryModel = proto.Clone(policy.SummaryModel).(*runtimev1.SummaryModelSnapshotV1)
	}
}

// Recover only model selection and generation settings. Resolve current authority again.
func continuationAdhocSettings(policy *FrozenContextPolicy) (json.RawMessage, error) {
	if policy == nil {
		return json.RawMessage(`{}`), nil
	}
	var runtime struct {
		Kwargs map[string]json.RawMessage `json:"kwargs"`
	}
	if json.Unmarshal(policy.TaskLLM, &runtime) != nil || len(runtime.Kwargs["model"]) == 0 {
		return nil, ErrUnsupportedCurrentAgentStart
	}
	settings := map[string]json.RawMessage{"model_name": runtime.Kwargs["model"]}
	for _, key := range []string{"model_project_id", "max_tokens", "reasoning_effort", "temperature"} {
		if value, ok := runtime.Kwargs[key]; ok {
			settings[key] = value
		}
	}
	encoded, err := json.Marshal(settings)
	if err != nil {
		return nil, ErrUnsupportedCurrentAgentStart
	}
	if _, err = currentAdhocLLMSettings(encoded); err != nil {
		return nil, err
	}
	return encoded, nil
}

func validateContinuationModelSelection(selected, frozen json.RawMessage) error {
	var original map[string]json.RawMessage
	var snapshot struct {
		Settings map[string]json.RawMessage `json:"llm_settings"`
	}
	if json.Unmarshal(selected, &original) != nil || json.Unmarshal(frozen, &snapshot) != nil {
		return ErrUnsupportedCurrentAgentStart
	}
	for _, key := range []string{"model_name", "model_project_id"} {
		if value, ok := original[key]; ok && !bytes.Equal(bytes.TrimSpace(value), bytes.TrimSpace(snapshot.Settings[key])) {
			return ErrUnsupportedCurrentAgentStart
		}
	}
	return nil
}
