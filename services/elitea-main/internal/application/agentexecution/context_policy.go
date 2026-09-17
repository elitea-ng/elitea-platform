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

func (service *CurrentApplicationStartService) restoreContinuationContext(
	ctx context.Context, request CurrentContinuationRequest, target CurrentContinuationTarget, input *runtimev1.AgentExecutionInputV1,
) error {
	if service.contextPolicy == nil {
		return nil
	}
	policy, err := service.contextPolicy.ContinuationContextPolicy(
		ctx, request.ProjectID, request.ActorUserID, request.ConversationUUID,
		request.ResponseMessageID, target.ExecutionGeneration,
	)
	if err != nil {
		return fmt.Errorf("restore admitted context policy: %w", err)
	}
	if !validJSONObject(policy.Settings) {
		return ErrUnsupportedCurrentAgentStart
	}
	input.ContextSettings = bytes.Clone(policy.Settings)
	input.SummaryModel = nil
	if policy.SummaryModel != nil {
		input.SummaryModel = proto.Clone(policy.SummaryModel).(*runtimev1.SummaryModelSnapshotV1)
	}
	return nil
}
