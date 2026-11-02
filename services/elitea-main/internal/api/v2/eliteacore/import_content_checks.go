package eliteacore

import (
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/citations"
)

// importedVersionsRefusal checks every version of one imported or forked
// agent before anything is written: the pipeline bounds
// (pipelineLimitRefusal) and the content controls of ADR-0043 (#1500):
// meta.citations, the meta output contract and the output contract of each
// pipeline LLM node. These are the checks of the version save. Without them
// the import and the fork stored a value that the worker cannot admit, and
// every chat turn of the agent failed. A refused version refuses the whole
// agent, as a pipeline limit does.
func importedVersionsRefusal(versions []any) error {
	if err := pipelineLimitRefusal(versions); err != nil {
		return err
	}
	for _, raw := range versions {
		version, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		agentType, _ := version["agent_type"].(string)
		if stored, known := storedAgentType(agentType); known {
			agentType = stored
		}
		meta, _ := version["meta"].(map[string]any)
		instructions, _ := version["instructions"].(string)
		if err := citations.CheckVersion(agentType, meta, instructions); err != nil {
			return err
		}
	}
	return nil
}
