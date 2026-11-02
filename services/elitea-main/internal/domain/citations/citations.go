// Package citations checks the citation setting of an agent version when it
// is written (ADR-0043 decision 5, #1500).
//
// The setting is the version meta key `citations`. "required" makes the
// worker ask the model to cite its sources and report a grounding indicator
// on the final answer. "off", null or absent turn it off. Pipelines and
// nested agents ignore it.
//
// Every path that writes version meta from caller input checks it: the
// application create, update and new-version routes, the import and the
// fork, and the internal MCP version tools. Paths that copy a stored row
// (publish, the catalogue mirror, the embedded sub-agent clone, the MCP
// backup) copy what one of those paths wrote. The worker turns citations off
// for a stored value that it cannot read, so an older row does not stop the
// chat.
package citations

import (
	"errors"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/outputcontract"
)

// MetaKey is the agent version meta key of the setting.
const MetaKey = "citations"

// ErrInvalid is a meta.citations value other than "off", "required" or null.
var ErrInvalid = errors.New(`meta.citations must be "off", "required" or null`)

// Check checks meta.citations of a version meta. A nil meta, an absent key
// and JSON null are off.
func Check(meta map[string]any) error {
	raw, present := meta[MetaKey]
	if !present || raw == nil {
		return nil
	}
	if mode, ok := raw.(string); ok && (mode == "off" || mode == "required") {
		return nil
	}
	return ErrInvalid
}

// CheckVersion checks the content controls that a version write carries:
// meta.citations, the output contract of the meta (refused on a pipeline)
// and the output contract of every LLM node of a pipeline's instructions.
// It answers ErrInvalid or an *outputcontract.Error.
func CheckVersion(agentType string, meta map[string]any, instructions string) error {
	if err := Check(meta); err != nil {
		return err
	}
	if meta != nil {
		if err := outputcontract.CheckMeta(agentType, meta); err != nil {
			return err
		}
	}
	return outputcontract.CheckPipeline(instructions)
}
