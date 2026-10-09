package pipelinelimits_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/pipelinelimits"
)

// benchPipeline is a worst-case in-bound graph: MaxNodes direct MCP nodes, each
// padded so the document sits just under MaxInstructionsBytes.
func benchPipeline() (string, []string) {
	var b strings.Builder
	b.WriteString("entry_point: n1\nnodes:\n")
	attached := make([]string, 0, 64)
	for i := 1; i <= pipelinelimits.MaxNodes; i++ {
		fmt.Fprintf(&b, "  - id: n%d\n    type: mcp\n    toolkit_name: toolkit %d\n    tool: run\n    transition: END\n", i, i%64)
	}
	for i := 0; i < 64; i++ {
		attached = append(attached, fmt.Sprintf("toolkit_%d", i))
	}
	pad := pipelinelimits.MaxInstructionsBytes - b.Len() - 3
	b.WriteString("# " + strings.Repeat("x", pad) + "\n")
	return b.String(), attached
}

func BenchmarkCheckAtTheBound(b *testing.B) {
	doc, _ := benchPipeline()
	b.ReportAllocs()
	for b.Loop() {
		if err := pipelinelimits.Check(doc); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCheckStartAtTheBound(b *testing.B) {
	doc, attached := benchPipeline()
	b.ReportAllocs()
	for b.Loop() {
		if err := pipelinelimits.CheckStart(doc, attached); err != nil {
			b.Fatal(err)
		}
	}
}
