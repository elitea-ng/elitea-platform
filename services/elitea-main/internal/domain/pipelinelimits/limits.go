// Package pipelinelimits holds the one source of truth for the size bounds of a
// saved pipeline definition. Main refuses an over-bound definition when it is
// saved and when a run starts, and the Worker compiler enforces the same
// numbers (services/elitea-worker-rust src/agents/graph/compiler.rs
// MAX_PIPELINE_YAML_BYTES / MAX_PIPELINE_NODES).
package pipelinelimits

import (
	"fmt"
	"net/http"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

const (
	// MaxInstructionsBytes is the largest pipeline YAML, in bytes.
	MaxInstructionsBytes = 512 * 1024
	// MaxNodes is the largest number of top-level `nodes` entries.
	MaxNodes = 128
)

// The refusals are shared, immutable *apierr.APIError values so a caller can
// both write them as a 400 and branch on them with errors.Is. Their text names
// the limit and the remedy and never carries pipeline content.
var (
	ErrInstructionsTooLarge error = &apierr.APIError{
		Status:  http.StatusBadRequest,
		Code:    "PIPELINE_INSTRUCTIONS_TOO_LARGE",
		Message: fmt.Sprintf("The pipeline definition exceeds the %d KiB size limit. Reduce the YAML before saving.", MaxInstructionsBytes/1024),
	}
	ErrTooManyNodes error = &apierr.APIError{
		Status:  http.StatusBadRequest,
		Code:    "PIPELINE_TOO_MANY_NODES",
		Message: fmt.Sprintf("The pipeline has more than %d nodes. Split it into smaller pipelines before saving.", MaxNodes),
	}
)

// Check refuses a pipeline definition that exceeds MaxInstructionsBytes or whose
// top-level `nodes` list holds more than MaxNodes entries. The size is tested
// first, on the byte length, so an over-size document is never parsed.
//
// Check is a save-time guard, not a validator. A document that does not parse,
// has no `nodes` sequence, or is not a mapping is not refused here: the Worker
// compiler validates it when the pipeline runs. Only the first YAML document is
// read, the entries of `nodes` are counted but never inspected (so legacy
// numeric ids count like any other), and an aliased `nodes` list is followed
// because the compiler reads it the same way.
func Check(instructions string) error {
	if len(instructions) > MaxInstructionsBytes {
		return ErrInstructionsTooLarge
	}
	var document yaml.Node
	if yaml.NewDecoder(strings.NewReader(instructions)).Decode(&document) != nil || len(document.Content) != 1 {
		return nil
	}
	root := aliased(document.Content[0])
	if root.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		if aliased(root.Content[i]).Value != "nodes" {
			continue
		}
		if nodes := aliased(root.Content[i+1]); nodes.Kind == yaml.SequenceNode && len(nodes.Content) > MaxNodes {
			return ErrTooManyNodes
		}
	}
	return nil
}

func aliased(node *yaml.Node) *yaml.Node {
	if node.Kind == yaml.AliasNode && node.Alias != nil {
		return node.Alias
	}
	return node
}
