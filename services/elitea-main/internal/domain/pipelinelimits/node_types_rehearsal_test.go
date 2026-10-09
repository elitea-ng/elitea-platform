//go:build graph_extensions_rehearsal

package pipelinelimits_test

import (
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/pipelinelimits"
)

func TestCheckAdmitsShapingNodesOnARehearsalBuild(t *testing.T) {
	for _, nodeType := range []string{"split_out", "aggregate"} {
		if err := pipelinelimits.Check(pipelineWithNode("split", nodeType)); err != nil {
			t.Errorf("type %s: got %v, want admitted on a rehearsal build", nodeType, err)
		}
	}
}
