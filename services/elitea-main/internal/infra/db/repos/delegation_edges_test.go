package repos

import (
	"reflect"
	"testing"

	dg "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/delegationgrant"
)

// The kind an edge is granted as follows its caller: an agent holds an agent
// tool, a pipeline whose YAML runs the attached agent from an Agent node holds
// a pipeline node, and a pipeline that only attaches it holds a pipeline child.
func TestGroupEdgesChoosesTheKindOfEachPair(t *testing.T) {
	t.Parallel()
	pairs, kinds, versions := groupEdges([]edgeCandidate{
		{callerID: 20, callerVersionID: 201, calleeID: 5, calleeVersionID: 51},
		{callerID: 10, callerVersionID: 101, calleeID: 5, calleeVersionID: 51, pipeline: true, alias: "spec"},
		{callerID: 11, callerVersionID: 111, calleeID: 5, calleeVersionID: 51, pipeline: true},
		// Two versions of one pipeline: one runs the attachment from a node, the
		// other does not. The pair is a pipeline node edge.
		{callerID: 12, callerVersionID: 121, calleeID: 5, calleeVersionID: 51, pipeline: true},
		{callerID: 12, callerVersionID: 122, calleeID: 5, calleeVersionID: 52, pipeline: true, alias: "spec"},
	})
	if want := []edgePair{{10, 5}, {11, 5}, {12, 5}, {20, 5}}; !reflect.DeepEqual(pairs, want) {
		t.Fatalf("pairs = %v, want %v in a stable order", pairs, want)
	}
	for pair, kind := range map[edgePair]dg.EdgeKind{
		{10, 5}: dg.EdgePipelineNode, {11, 5}: dg.EdgePipelineChild, {12, 5}: dg.EdgePipelineNode, {20, 5}: dg.EdgeAgentTool,
	} {
		if kinds[pair] != kind {
			t.Errorf("pair %v: kind %s, want %s", pair, kinds[pair], kind)
		}
	}
	if len(versions[edgePair{12, 5}]) != 2 {
		t.Fatalf("both versions of a pair are compared: %+v", versions[edgePair{12, 5}])
	}
}
