package agentexecution

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/pipelinelimits"
	"github.com/stretchr/testify/require"
)

// Pipelines without `type: http` nodes are not subject to the HTTP snapshot
// grammar; the Worker owns their validation (compiler.rs).
func nonHTTPPipelinesOutsideHTTPSnapshotGrammar() map[string]string {
	return map[string]string{
		"100KiB":                             "state:\n  count: {type: int, value: 0}\nentry_point: tick\nnodes:\n  - id: tick\n    type: state_modifier\n    template: '" + strings.Repeat("x", 100*1024) + "'\n    input: [count]\n    output: [count]\n    transition: END\n",
		"numeric id":                         "state:\n  count: {type: int, value: 0}\nentry_point: 1\nnodes:\n  - id: 1\n    type: state_modifier\n    template: '{{ count + 1 }}'\n    input: [count]\n    output: [count]\n    transition: END\n",
		"numeric entry point and transition": "state:\n  count: {type: int, value: 0}\nentry_point: 1\nnodes:\n  - id: 1\n    type: state_modifier\n    template: '{{ count + 1 }}'\n    input: [count]\n    output: [count]\n    transition: 2\n  - id: 2\n    type: state_modifier\n    template: '{{ count + 1 }}'\n    input: [count]\n    output: [count]\n    transition: END\n",
		"anchored id":                        "state:\n  count: {type: int, value: 0}\nentry_point: tick\nnodes:\n  - id: &tick tick\n    type: state_modifier\n    template: '{{ count + 1 }}'\n    input: [count]\n    output: [count]\n    transition: END\ninterrupt_before: [*tick]\n",
	}
}

func TestCurrentPipelineStartWithoutHTTPNodesSkipsHTTPSnapshotGrammar(t *testing.T) {
	for name, instructions := range nonHTTPPipelinesOutsideHTTPSnapshotGrammar() {
		t.Run(name, func(t *testing.T) {
			details, err := json.Marshal(map[string]any{"agent_type": "pipeline", "instructions": instructions, "llm_settings": map[string]any{"model_name": "test", "model_project_id": 7, "openai_compatible": false}, "meta": map[string]any{}, "tools": []any{}})
			require.NoError(t, err)
			input, err := currentApplicationInput(validCurrentApplicationStartRequest(), CurrentApplicationTarget{ApplicationID: 31, ApplicationVersionID: 41, Variables: json.RawMessage(`[]`), VersionDetails: details, ChatHistory: json.RawMessage(`[]`), InternalTools: json.RawMessage(`[]`)}, nil, nil, nil, "", "")
			require.NoError(t, err)
			require.NotContains(t, string(input.GetApplication()), "http_action_snapshot")
			require.Contains(t, string(input.GetApplication()), "state_modifier")
		})
	}
}

// A pipeline over the shared save-time bounds is refused at start with the same
// typed error the save path returns, and it stays an unsupported start so every
// existing caller keeps matching on the sentinel.
// The size bound is named before any start work: an over-size pipeline never
// reaches the root-source capture, whose own larger bound refuses with the
// generic unsupported-start text (local version 133, ~8 MB, did exactly that).
// The node count is named later by the freeze pre-scan (test below).
func TestCurrentPipelineStartRefusesTheSharedBoundsBeforeSourceCapture(t *testing.T) {
	for name, tc := range map[string]struct {
		instructions string
		want         error
	}{
		"bytes over the capture bound": {"nodes: []\n# " + strings.Repeat("x", 2*1024*1024), pipelinelimits.ErrInstructionsTooLarge},
		"bytes":                        {"nodes: []\n# " + strings.Repeat("x", pipelinelimits.MaxInstructionsBytes), pipelinelimits.ErrInstructionsTooLarge},
	} {
		t.Run(name, func(t *testing.T) {
			raw, err := json.Marshal(map[string]any{"id": 41, "application_id": 31, "agent_type": "pipeline", "instructions": tc.instructions, "llm_settings": map[string]any{"model_name": "test", "model_project_id": 7, "openai_compatible": false}, "meta": map[string]any{}, "tools": []any{}, "skills": []any{}})
			require.NoError(t, err)
			target := CurrentApplicationTarget{ApplicationID: 31, ApplicationVersionID: 41, Variables: json.RawMessage(`[]`), VersionDetails: raw, SourceVersionDetails: raw, ChatHistory: json.RawMessage(`[]`)}
			owner := &originalSourceOwnerFixture{}
			service, admissions := sourceService(t, &currentApplicationResolverStub{target: target}, owner, &sourceRecordingFreezer{})
			_, err = service.StartCurrentApplication(t.Context(), validCurrentApplicationStartRequest())
			require.ErrorIs(t, err, tc.want)
			require.ErrorIs(t, err, ErrUnsupportedCurrentAgentStart)
			require.Zero(t, len(owner.captures), "an over-bound pipeline reached the source capture")
			require.Zero(t, len(admissions.requests))
		})
	}
}

func TestCurrentPipelineStartOverTheSharedBoundsReturnsTheTypedLimitError(t *testing.T) {
	for name, tc := range map[string]struct {
		instructions string
		want         error
	}{
		"bytes": {"nodes: []\n# " + strings.Repeat("x", pipelinelimits.MaxInstructionsBytes), pipelinelimits.ErrInstructionsTooLarge},
		"nodes": {"nodes:\n" + strings.Repeat("  - {id: n, type: llm}\n", pipelinelimits.MaxNodes+1), pipelinelimits.ErrTooManyNodes},
	} {
		t.Run(name, func(t *testing.T) {
			details, err := json.Marshal(map[string]any{"agent_type": "pipeline", "instructions": tc.instructions, "llm_settings": map[string]any{"model_name": "test", "model_project_id": 7, "openai_compatible": false}, "meta": map[string]any{}, "tools": []any{}})
			require.NoError(t, err)
			_, err = currentApplicationInput(validCurrentApplicationStartRequest(), CurrentApplicationTarget{ApplicationID: 31, ApplicationVersionID: 41, Variables: json.RawMessage(`[]`), VersionDetails: details, ChatHistory: json.RawMessage(`[]`), InternalTools: json.RawMessage(`[]`)}, nil, nil, nil, "", "")
			require.ErrorIs(t, err, tc.want)
			require.ErrorIs(t, err, ErrUnsupportedCurrentAgentStart)
		})
	}
}
