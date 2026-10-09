package httpaction

import (
	"errors"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/pipelinelimits"
)

const scopedHTTPNode = "  - id: send\n    type: http\n    revision: 1\n    request: {method: GET, url: https://api.example.test/items, response: {mode: json}}\n    output: [report]\n    transition: END\n"

func TestHTTPFreezeLeavesNonHTTPPipelinesToTheWorker(t *testing.T) {
	for name, instructions := range map[string]string{
		"100KiB":                             "entry_point: tick\nnodes:\n  - id: tick\n    type: state_modifier\n    template: '" + strings.Repeat("x", 100*1024) + "'\n",
		"numeric id":                         "entry_point: 1\nnodes:\n  - id: 1\n    type: state_modifier\n",
		"anchored id":                        "entry_point: tick\nnodes:\n  - id: &tick tick\n    type: llm\ninterrupt_before: [*tick]\n",
		"numeric entry point and transition": "entry_point: 1\nnodes:\n  - {id: 1, type: llm, transition: 2}\n  - {id: 2, type: llm, transition: END}\n",
		"128 nodes":                          "entry_point: n\nnodes:\n" + strings.Repeat("  - {id: n, type: llm}\n", 128),
	} {
		t.Run(name, func(t *testing.T) {
			if snapshot, err := FreezeSnapshot(31, 41, instructions); err != nil || snapshot != nil {
				t.Fatalf("non-HTTP pipeline entered the HTTP grammar: %v %v", snapshot, err)
			}
		})
	}
}

func TestHTTPFreezeKeepsStrictGrammarAndWorkerBounds(t *testing.T) {
	if snapshot, err := FreezeSnapshot(31, 41, "nodes:\n"+scopedHTTPNode); err != nil || snapshot == nil {
		t.Fatalf("valid HTTP node not frozen: %v", err)
	}
	for name, bad := range map[string]string{
		"HTTP over 64KiB":        "nodes:\n" + scopedHTTPNode + "# " + strings.Repeat("x", 64*1024) + "\n",
		"HTTP beside numeric id": "nodes:\n  - id: 1\n    type: llm\n" + scopedHTTPNode,
		"HTTP beside anchored":   "nodes:\n  - id: &a a\n    type: llm\n" + scopedHTTPNode,
		"HTTP node via alias":    "defs:\n  - &h {id: send, type: http}\nnodes:\n  - *h\n",
		"HTTP node via merge":    "nodes:\n  - <<: {type: http}\n    id: send\n",
		"over Worker bytes":      "nodes: []\n# " + strings.Repeat("x", 512*1024) + "\n",
		"over Worker nodes":      "nodes:\n" + strings.Repeat("  - {id: n, type: llm}\n", 129),
		"multi-document":         "nodes: []\n---\nnodes:\n" + scopedHTTPNode,
		"not a mapping":          "- nodes\n",
		"empty":                  "",
	} {
		t.Run(name, func(t *testing.T) {
			if snapshot, err := FreezeSnapshot(31, 41, bad); err == nil {
				t.Fatalf("admitted without a strict HTTP decision: %v", snapshot)
			}
		})
	}
}

// The start path reports the shared limit it refused, not a generic failure.
func TestHTTPFreezeRefusalsNameTheSharedPipelineLimit(t *testing.T) {
	for name, tc := range map[string]struct {
		instructions string
		want         error
	}{
		"bytes": {"nodes: []\n# " + strings.Repeat("x", pipelinelimits.MaxInstructionsBytes), pipelinelimits.ErrInstructionsTooLarge},
		"nodes": {"nodes:\n" + strings.Repeat("  - {id: n, type: llm}\n", pipelinelimits.MaxNodes+1), pipelinelimits.ErrTooManyNodes},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := FreezeSnapshot(31, 41, tc.instructions); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}
