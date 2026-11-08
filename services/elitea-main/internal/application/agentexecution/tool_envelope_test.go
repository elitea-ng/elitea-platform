package agentexecution

import (
	"context"
	"errors"
	"testing"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"

	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

type agentToolSettingsStub struct {
	settings platformconfig.AgentToolSettings
	err      error
}

func (s agentToolSettingsStub) Get(context.Context) (platformconfig.AgentToolSettings, error) {
	return s.settings, s.err
}

func envelopeInput(application string) *runtimev1.AgentExecutionInputV1 {
	return &runtimev1.AgentExecutionInputV1{SchemaRevision: agentInputSchemaRevision, Application: []byte(application)}
}

// ADR-0050 decision 3: admission freezes the root agent's envelope and the
// platform's ceilings into the input, and does not change the caller's input.
func TestAdmissionFreezesTheToolEnvelope(t *testing.T) {
	t.Parallel()
	input := envelopeInput(`{"version_details":{"meta":{"tool_envelope":{
		"tool_limits":{"github/create_issue":3},
		"unattended_tools":["github/get_issue"],
		"subagent_tools":[]}}}}`)
	service := (&AdmissionService{}).WithToolEnvelope(agentToolSettingsStub{settings: platformconfig.AgentToolSettings{
		MaxToolLimit: 20, UnattendedToolsAllowlist: []string{"github/create_comment"},
	}})
	stamped, err := service.stampToolEnvelope(context.Background(), executiondomain.AgentApplicationCapability, input)
	if err != nil {
		t.Fatal(err)
	}
	frozen := stamped.GetToolEnvelope()
	if input.GetToolEnvelope() != nil {
		t.Fatal("the caller's input changed")
	}
	if len(frozen.GetToolLimits()) != 1 || frozen.GetToolLimits()[0].GetTool() != "github/create_issue" ||
		frozen.GetToolLimits()[0].GetMaxCalls() != 3 || frozen.GetMaxToolLimit() != 20 {
		t.Fatalf("limits = %v", frozen)
	}
	if !frozen.GetUnattendedToolsSet() || len(frozen.GetUnattendedTools()) != 1 ||
		len(frozen.GetUnattendedToolsAllowlist()) != 1 {
		t.Fatalf("unattended = %v", frozen)
	}
	// An empty sub-agent list is set: no tool for a child, not the default.
	if !frozen.GetSubagentToolsSet() || len(frozen.GetSubagentTools()) != 0 {
		t.Fatalf("subagent = %v", frozen)
	}
}

// Neither an envelope nor a ceiling: the input is unchanged byte for byte,
// and an envelope the caller put in the input is dropped, never trusted.
func TestAdmissionLeavesTheInputWithoutAnEnvelope(t *testing.T) {
	t.Parallel()
	input := envelopeInput(`{"version_details":{"meta":{}}}`)
	service := (&AdmissionService{}).WithToolEnvelope(agentToolSettingsStub{})
	if same, err := service.stampToolEnvelope(context.Background(), executiondomain.AgentApplicationCapability, input); err != nil || same != input {
		t.Fatalf("same = %v, err = %v", same, err)
	}
	forged := envelopeInput(`{}`)
	forged.ToolEnvelope = &runtimev1.AgentToolEnvelopeV1{UnattendedToolsSet: true, UnattendedToolsAllowlist: []string{"github/*"}}
	for _, service := range []*AdmissionService{service, {}} {
		stamped, err := service.stampToolEnvelope(context.Background(), executiondomain.AgentApplicationCapability, forged)
		if err != nil || stamped.GetToolEnvelope() != nil {
			t.Fatalf("stamped = %v, err = %v", stamped.GetToolEnvelope(), err)
		}
	}
}

// An ad-hoc chat has no agent version: only the ceilings are frozen.
func TestAdmissionFreezesOnlyTheCeilingsForAnAdhocTurn(t *testing.T) {
	t.Parallel()
	input := envelopeInput(`{"version_details":{"meta":{"tool_envelope":{"tool_limits":{"github/x":1}}}}}`)
	service := (&AdmissionService{}).WithToolEnvelope(agentToolSettingsStub{settings: platformconfig.AgentToolSettings{MaxToolLimit: 4}})
	stamped, err := service.stampToolEnvelope(context.Background(), executiondomain.AgentAdhocCapability, input)
	if err != nil {
		t.Fatal(err)
	}
	if frozen := stamped.GetToolEnvelope(); frozen.GetMaxToolLimit() != 4 || len(frozen.GetToolLimits()) != 0 ||
		frozen.GetUnattendedToolsSet() || frozen.GetSubagentToolsSet() {
		t.Fatalf("frozen = %v", frozen)
	}
}

// A ceiling that cannot be read, or an envelope that is not valid, refuses
// the turn: failing open would loosen what was narrowed.
func TestAdmissionRefusesWithoutAReadableEnvelope(t *testing.T) {
	t.Parallel()
	down := (&AdmissionService{}).WithToolEnvelope(agentToolSettingsStub{err: errors.New("down")})
	if _, err := down.stampToolEnvelope(context.Background(), executiondomain.AgentApplicationCapability, envelopeInput(`{}`)); err == nil {
		t.Error("read error: want a refusal")
	}
	service := (&AdmissionService{}).WithToolEnvelope(agentToolSettingsStub{})
	for _, application := range []string{
		`{"version_details":{"meta":{"tool_envelope":{"tool_limits":{"github/x":0}}}}}`,
		`{"version_details":{"meta":{"tool_envelope":{"unattended_tools":["github"]}}}}`,
		`{"version_details":{"meta":{"tool_envelope":{"other":1}}}}`,
		`not json`,
	} {
		_, err := service.stampToolEnvelope(context.Background(), executiondomain.AgentApplicationCapability, envelopeInput(application))
		if !errors.Is(err, ErrInvalidAuthoritativeAgentInput) {
			t.Errorf("%s: err = %v", application, err)
		}
	}
}
