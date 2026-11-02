package eliteacore

import (
	"errors"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/citations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/outputcontract"
)

// The import and the fork stored meta.citations and the output contracts as
// is. The worker refused such a version, so every chat turn of the agent
// failed. Both paths now refuse the agent before they write it.
func TestImportedVersionsRefusalChecksTheContentControls(t *testing.T) {
	version := func(agentType string, meta map[string]any, instructions string) []any {
		return []any{
			map[string]any{"name": "base", "agent_type": "openai", "instructions": "ok"},
			map[string]any{"name": "latest", "agent_type": agentType, "meta": meta, "instructions": instructions},
		}
	}
	var refusal *outputcontract.Error

	if err := importedVersionsRefusal(version("openai", map[string]any{"citations": "always"}, "x")); !errors.Is(err, citations.ErrInvalid) {
		t.Errorf("citations: got %v, want ErrInvalid", err)
	}
	badContract := map[string]any{"output_contract": map[string]any{"type": "format", "format": "xml"}}
	if err := importedVersionsRefusal(version("openai", badContract, "x")); !errors.As(err, &refusal) {
		t.Errorf("meta contract: got %v", err)
	}
	// The legacy spelling `agent` is stored as `openai`; a pipeline cannot
	// have a meta contract.
	onPipeline := map[string]any{"output_contract": map[string]any{"type": "format", "format": "json"}}
	if err := importedVersionsRefusal(version("pipeline", onPipeline, "")); !errors.As(err, &refusal) ||
		refusal.Reason != outputcontract.ReasonPipelineContractOnNode {
		t.Errorf("pipeline meta contract: got %v", err)
	}
	// A pipeline node's contract was already refused through the pipeline
	// bounds (pipelinelimits), and still is.
	node := "nodes:\n  - id: answer\n    type: llm\n    output_contract:\n      type: regex\n      pattern: \"\"\n"
	if err := importedVersionsRefusal(version("pipeline", nil, node)); err == nil || !strings.Contains(err.Error(), `Node "answer"`) {
		t.Errorf("node contract: got %v", err)
	}

	valid := map[string]any{"citations": "required", "output_contract": map[string]any{"type": "format", "format": "json"}}
	if err := importedVersionsRefusal(version("agent", valid, "Answer briefly.")); err != nil {
		t.Errorf("valid agent: %v", err)
	}
	if err := importedVersionsRefusal([]any{"not an object"}); err != nil {
		t.Errorf("a non-object entry is reported by the insert loop: %v", err)
	}
}
