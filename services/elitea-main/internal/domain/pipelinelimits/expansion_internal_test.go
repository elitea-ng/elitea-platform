package pipelinelimits

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The meter allocates nothing: its cost is the visits, bounded by the budget,
// never a copy of the expansion.
func TestExpansionMeterAllocatesNothing(t *testing.T) {
	doc := "a: &x [" + strings.Repeat("0,", 1_000) + "0]\nb: [" + strings.Repeat("*x,", 2_000) + "*x]\n"
	var document yaml.Node
	if err := yaml.Unmarshal([]byte(doc), &document); err != nil {
		t.Fatal(err)
	}
	root := document.Content[0]
	if checkExpansion(root) == nil {
		t.Fatal("bomb accepted")
	}
	if allocs := testing.AllocsPerRun(20, func() { _ = checkExpansion(root) }); allocs != 0 {
		t.Fatalf("checkExpansion allocates %.0f times, want 0", allocs)
	}
}
