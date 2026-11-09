package agentexecution

import (
	"context"
	"encoding/json"
	"fmt"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"google.golang.org/protobuf/proto"

	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/toolenvelope"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// The per-agent tool envelope (ADR-0050 decision 3, #1613): admission
// freezes the root agent's meta.tool_envelope and the platform's `agents`
// ceilings into the run's input (AgentExecutionInputV1.tool_envelope, field
// 74), and the worker enforces them. Like the platform preamble it is
// stamped after the request digest, so a retry of the same request stays
// the same request when an administrator changed a ceiling in between, and
// before the bundle, so the bundle digest covers what the worker enforces.

// AgentToolSettingsSource reads the platform's `agents` section (in
// production a platformconfig.CachedSection that an admin save drops).
type AgentToolSettingsSource interface {
	Get(ctx context.Context) (platformconfig.AgentToolSettings, error)
}

// WithToolEnvelope makes admission freeze the tool envelope into each
// turn's input.
func (s *AdmissionService) WithToolEnvelope(source AgentToolSettingsSource) *AdmissionService {
	if s != nil && source != nil {
		s.toolSettings = source
	}
	return s
}

// stampToolEnvelope returns the input with the frozen envelope. The caller
// never supplies one: whatever the input carried is replaced. A read error
// refuses the turn, because a ceiling that fails open would loosen what the
// administrator narrowed. The caller's input is not changed.
func (s *AdmissionService) stampToolEnvelope(
	ctx context.Context, capabilityID string, input *runtimev1.AgentExecutionInputV1,
) (*runtimev1.AgentExecutionInputV1, error) {
	if s.toolSettings == nil {
		if input.GetToolEnvelope() == nil {
			return input, nil
		}
		return withToolEnvelope(input, nil)
	}
	settings, err := s.toolSettings.Get(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the agent tool settings: %w", err)
	}
	var envelope toolenvelope.Envelope
	if capabilityID == executiondomain.AgentApplicationCapability {
		if envelope, err = applicationToolEnvelope(input.GetApplication()); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidAuthoritativeAgentInput, err)
		}
	}
	frozen := FrozenToolEnvelope(envelope, settings)
	if frozen == nil && input.GetToolEnvelope() == nil {
		return input, nil
	}
	return withToolEnvelope(input, frozen)
}

func withToolEnvelope(
	input *runtimev1.AgentExecutionInputV1, envelope *runtimev1.AgentToolEnvelopeV1,
) (*runtimev1.AgentExecutionInputV1, error) {
	stamped, ok := proto.Clone(input).(*runtimev1.AgentExecutionInputV1)
	if !ok {
		return nil, ErrInvalidAuthoritativeAgentInput
	}
	stamped.ToolEnvelope = envelope
	return stamped, nil
}

// applicationToolEnvelope reads version_details.meta.tool_envelope of the
// frozen application. Sub-agents' own envelopes are not frozen: the root
// agent's envelope decides what a run may do (ADR-0050 decision 3).
func applicationToolEnvelope(application []byte) (toolenvelope.Envelope, error) {
	if len(application) == 0 {
		return toolenvelope.Envelope{}, nil
	}
	var frozen struct {
		VersionDetails *struct {
			Meta map[string]any `json:"meta"`
		} `json:"version_details"`
	}
	if err := json.Unmarshal(application, &frozen); err != nil {
		return toolenvelope.Envelope{}, fmt.Errorf("the frozen application is not readable: %w", err)
	}
	if frozen.VersionDetails == nil {
		return toolenvelope.Envelope{}, nil
	}
	return toolenvelope.Parse(frozen.VersionDetails.Meta[toolenvelope.MetaKey])
}

// FrozenToolEnvelope is the input field for an agent's envelope under the
// platform's ceilings; nil when neither sets anything, so a run of an agent
// without an envelope on a platform without ceilings is unchanged byte for
// byte.
func FrozenToolEnvelope(
	envelope toolenvelope.Envelope, settings platformconfig.AgentToolSettings,
) *runtimev1.AgentToolEnvelopeV1 {
	if envelope.Empty() && settings.Empty() {
		return nil
	}
	frozen := &runtimev1.AgentToolEnvelopeV1{
		MaxToolLimit:             settings.MaxToolLimit,
		UnattendedToolsSet:       envelope.UnattendedTools != nil,
		UnattendedTools:          append([]string(nil), envelope.UnattendedTools...),
		UnattendedToolsAllowlist: append([]string(nil), settings.UnattendedToolsAllowlist...),
		SubagentToolsSet:         envelope.SubagentTools != nil,
		SubagentTools:            append([]string(nil), envelope.SubagentTools...),
	}
	for _, limit := range envelope.ToolLimits {
		frozen.ToolLimits = append(frozen.ToolLimits, &runtimev1.AgentToolLimitV1{Tool: limit.Tool, MaxCalls: limit.MaxCalls})
	}
	return frozen
}
