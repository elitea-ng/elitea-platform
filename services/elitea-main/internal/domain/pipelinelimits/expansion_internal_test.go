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

// The refusal of the audit's bomb costs at most the budget in visits, whatever
// the document expands to. Every visit either replays an alias (a jump) or
// consumes a node, and the walk stops at the first one past its bound, so
// nodes+jumps is the whole of its work. Counting it is exact on any machine,
// where a wall-clock bound measured the host; the bomb itself would be about
// 3x10^9 nodes.
func TestExpansionMeterStopsAtTheBudget(t *testing.T) {
	doc := "a: &x [" + strings.Repeat("0,", 50_000) + "0]\nb: [" + strings.Repeat("*x,", 60_000) + "*x]\n"
	var document yaml.Node
	if err := yaml.Unmarshal([]byte(doc), &document); err != nil {
		t.Fatal(err)
	}
	meter, ok := meterExpansion(document.Content[0])
	if ok {
		t.Fatal("bomb accepted")
	}
	if meter.nodes > MaxExpandedNodes+1 {
		t.Errorf("the meter visited %d nodes, past the budget of %d", meter.nodes, MaxExpandedNodes)
	}
	if meter.jumps > meter.jumpLimit+1 {
		t.Errorf("the meter replayed %d aliases, past its limit of %d", meter.jumps, meter.jumpLimit)
	}
	if meter.nodes == 0 {
		t.Fatal("the meter did no work, so this test measured nothing")
	}
	t.Logf("refused after %d nodes and %d alias replays", meter.nodes, meter.jumps)
}
