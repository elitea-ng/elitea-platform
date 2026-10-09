//go:build !graph_extensions_rehearsal

package pipelinelimits_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/pipelinelimits"
)

func TestCheckRefusesShapingNodesOnAProductionBuildNamingNodeAndType(t *testing.T) {
	for _, nodeType := range []string{"split_out", "aggregate"} {
		api := refusal(t, pipelineWithNode("split", nodeType))
		if api.Status != http.StatusBadRequest || api.Code != pipelinelimits.CodeNodeTypeNotAvailable {
			t.Fatalf("%s: status/code = %d/%s, want 400/%s", nodeType, api.Status, api.Code, pipelinelimits.CodeNodeTypeNotAvailable)
		}
		for _, want := range []string{`Node "split"`, `"` + nodeType + `"`, "not available on this deployment", "before saving"} {
			if !strings.Contains(api.Message, want) {
				t.Errorf("%s: message %q does not contain %q", nodeType, api.Message, want)
			}
		}
		if pipelinelimits.Refusal(api) == nil {
			t.Errorf("%s: Refusal = nil", nodeType)
		}
	}
}

func TestCheckRefusesAShapingNodeAfterAdmittedOnesAndThroughAliases(t *testing.T) {
	doc := "entry_point: a\nshared: &shape\n  id: s\n  type: split_out\nnodes:\n" +
		"  - id: a\n    type: llm\n    transition: s\n  - *shape\n"
	if api := refusal(t, doc); api.Code != pipelinelimits.CodeNodeTypeNotAvailable {
		t.Fatalf("aliased split_out node: code = %s", api.Code)
	}
	aliasedType := "t: &kind aggregate\nentry_point: a\nnodes:\n  - id: a\n    type: *kind\n"
	if api := refusal(t, aliasedType); api.Code != pipelinelimits.CodeNodeTypeNotAvailable {
		t.Fatalf("aliased type value: code = %s", api.Code)
	}
}
