package applicationcomposition_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/applicationcomposition"
)

type fakeSource map[int64]applicationcomposition.Version

func (s fakeSource) Version(_ context.Context, id applicationcomposition.Identity) (applicationcomposition.Version, bool, error) {
	version, ok := s[id.VersionID]
	return version, ok, nil
}

func id(n int64) applicationcomposition.Identity {
	return applicationcomposition.Identity{ApplicationID: n, VersionID: n}
}

func agent(name string, attached ...int64) applicationcomposition.Version {
	return applicationcomposition.Version{Kind: applicationcomposition.KindAgent, Name: name, Attachments: attachments(attached)}
}

// pipeline runs every attachment from its own Agent node.
func pipeline(name string, attached ...int64) applicationcomposition.Version {
	version := applicationcomposition.Version{Kind: applicationcomposition.KindPipeline, Name: name, Attachments: attachments(attached)}
	for _, attachment := range version.Attachments {
		version.AgentNodes = append(version.AgentNodes, applicationcomposition.AgentNode{ID: "node_" + attachment.Alias, Alias: attachment.Alias})
	}
	return version
}

func paused(version applicationcomposition.Version) applicationcomposition.Version {
	version.StaticPause = true
	return version
}

func attachments(ids []int64) []applicationcomposition.Attachment {
	out := make([]applicationcomposition.Attachment, 0, len(ids))
	for _, n := range ids {
		out = append(out, applicationcomposition.Attachment{Alias: fmt.Sprintf("a%d", n), Identity: id(n)})
	}
	return out
}

func check(t *testing.T, source fakeSource, root int64) *applicationcomposition.Violation {
	t.Helper()
	err := applicationcomposition.Check(context.Background(), source, id(root), nil)
	if err == nil {
		return nil
	}
	var violation *applicationcomposition.Violation
	if !errors.As(err, &violation) {
		t.Fatalf("read error, not a violation: %v", err)
	}
	refusal := applicationcomposition.Refusal(err)
	if refusal == nil || refusal.Status != http.StatusBadRequest || refusal.Code != applicationcomposition.Code || refusal.Message != err.Error() {
		t.Fatalf("refusal = %+v", refusal)
	}
	return violation
}

func expect(t *testing.T, violation *applicationcomposition.Violation, bound applicationcomposition.Bound) {
	t.Helper()
	if violation == nil || violation.Bound != bound {
		t.Fatalf("violation = %v, want %s", violation, bound)
	}
}

// agentChain is an agent root with `tiers` agents in one chain, the root included.
func agentChain(tiers int) fakeSource {
	source := fakeSource{}
	for n := int64(1); n <= int64(tiers); n++ {
		if n == int64(tiers) {
			source[n] = agent(fmt.Sprintf("Agent %d", n))
		} else {
			source[n] = agent(fmt.Sprintf("Agent %d", n), n+1)
		}
	}
	return source
}

func TestAgentTiersAdmitTheLimitAndRefuseOneMore(t *testing.T) {
	if violation := check(t, agentChain(applicationcomposition.MaxAgentTiers), 1); violation != nil {
		t.Fatalf("the tier limit must save: %v", violation)
	}
	violation := check(t, agentChain(applicationcomposition.MaxAgentTiers+1), 1)
	expect(t, violation, applicationcomposition.BoundAgentTiers)
	if !strings.Contains(violation.Error(), `"Agent 1" → "Agent 2" → "Agent 3" → "Agent 4"`) {
		t.Fatalf("the refusal names the path: %s", violation)
	}
}

func TestACycleIsRefusedAndNamed(t *testing.T) {
	source := fakeSource{1: agent("Planner", 2), 2: pipeline("Flow", 1)}
	violation := check(t, source, 1)
	expect(t, violation, applicationcomposition.BoundCycle)
	if !strings.Contains(violation.Error(), "circular") || !strings.Contains(violation.Error(), `"Planner" → "Flow" → "Planner"`) {
		t.Fatalf("message = %s", violation)
	}
	expect(t, check(t, fakeSource{1: agent("Self", 1)}, 1), applicationcomposition.BoundCycle)
	// A cycle through a pipeline root's own Agent node.
	expect(t, check(t, fakeSource{1: pipeline("Root", 2), 2: agent("Worker", 1)}, 1), applicationcomposition.BoundCycle)
}

// pipelineChain is a pipeline root whose Agent nodes nest `levels` saved
// pipelines; the deepest one has an Agent node when withAgentNode.
func pipelineChain(levels int, withAgentNode bool) fakeSource {
	source := fakeSource{}
	for n := int64(1); n <= int64(levels)+1; n++ {
		source[n] = pipeline(fmt.Sprintf("Pipeline %d", n), n+1)
	}
	last := int64(levels) + 1
	if withAgentNode {
		source[last+1] = agent("Leaf")
	} else {
		source[last] = pipeline(fmt.Sprintf("Pipeline %d", last))
	}
	return source
}

func TestPipelineDepthAdmitsTheLimitAndRefusesOneMore(t *testing.T) {
	if violation := check(t, pipelineChain(applicationcomposition.MaxPipelineDepth, false), 1); violation != nil {
		t.Fatalf("the depth limit must save: %v", violation)
	}
	// One more level: the deepest pipeline runs an Agent node.
	expect(t, check(t, pipelineChain(applicationcomposition.MaxPipelineDepth, true), 1), applicationcomposition.BoundPipelineDepth)
	expect(t, check(t, pipelineChain(applicationcomposition.MaxPipelineDepth+1, false), 1), applicationcomposition.BoundPipelineDepth)
}

// scopeChain is the Worker's scope_chain: each scope is a pipeline whose Agent
// node runs an agent that carries the next pipeline as a tool.
func scopeChain(scopes int) fakeSource {
	source := fakeSource{}
	for scope := int64(0); scope < int64(scopes); scope++ {
		pipelineID, agentID := 10+scope*2, 11+scope*2
		source[pipelineID] = pipeline(fmt.Sprintf("Scope %d", scope), agentID)
		if scope+1 < int64(scopes) {
			source[agentID] = agent(fmt.Sprintf("Agent %d", scope), agentID+1)
		} else {
			source[agentID] = agent(fmt.Sprintf("Agent %d", scope))
		}
	}
	return source
}

func TestOrdinaryGraphScopesAdmitTheLimitAndRefuseOneMore(t *testing.T) {
	if violation := check(t, scopeChain(applicationcomposition.MaxOrdinaryGraphScopes), 10); violation != nil {
		t.Fatalf("the scope limit must save: %v", violation)
	}
	violation := check(t, scopeChain(applicationcomposition.MaxOrdinaryGraphScopes+1), 10)
	expect(t, violation, applicationcomposition.BoundGraphScopes)
	if !strings.Contains(violation.Error(), `Agent node "node_a15" of pipeline "Scope 2"`) {
		t.Fatalf("the refusal names the node: %s", violation)
	}
}

func TestTheHopBudgetAdmitsTheLimitAndRefusesOneMore(t *testing.T) {
	wide := func(children int) fakeSource {
		source := fakeSource{}
		var attached []int64
		for n := int64(2); n < int64(children)+2; n++ {
			source[n] = agent(fmt.Sprintf("Leaf %d", n))
			attached = append(attached, n)
		}
		source[1] = agent("Root", attached...)
		return source
	}
	if violation := check(t, wide(applicationcomposition.MaxApplicationHops), 1); violation != nil {
		t.Fatalf("the hop budget must save: %v", violation)
	}
	expect(t, check(t, wide(applicationcomposition.MaxApplicationHops+1), 1), applicationcomposition.BoundHops)
}

func TestAPausingPipelineToolIsRefusedOnlyBeneathAPipelineRoot(t *testing.T) {
	source := fakeSource{1: pipeline("Root", 2), 2: agent("Reviewer", 3), 3: paused(pipeline("Approval")), 4: agent("Chat", 3)}
	violation := check(t, source, 1)
	expect(t, violation, applicationcomposition.BoundStaticPause)
	if !strings.Contains(violation.Error(), `Pipeline "Approval"`) || !strings.Contains(violation.Error(), `agent "Reviewer"`) {
		t.Fatalf("message = %s", violation)
	}
	// An ordinary agent continues the same pause.
	if violation := check(t, source, 4); violation != nil {
		t.Fatalf("an agent root continues a static pause: %v", violation)
	}
	// A pause one Agent node below the tool counts too.
	nested := fakeSource{1: pipeline("Root", 2), 2: agent("Reviewer", 3), 3: pipeline("Outer", 5), 5: paused(pipeline("Inner"))}
	expect(t, check(t, nested, 1), applicationcomposition.BoundStaticPause)
	// A pipeline root's own nested pipeline may pause (the graph continues it).
	direct := fakeSource{1: pipeline("Root", 5), 5: paused(pipeline("Inner"))}
	if violation := check(t, direct, 1); violation != nil {
		t.Fatalf("a nested pipeline participant may pause: %v", violation)
	}
}

func TestAnUnreadableAttachmentIsNotFollowed(t *testing.T) {
	if violation := check(t, fakeSource{1: agent("Root", 99)}, 1); violation != nil {
		t.Fatalf("a deleted attachment degrades at run time, it is not refused: %v", violation)
	}
}

func TestTheOverrideIsTheVersionAboutToBeSaved(t *testing.T) {
	source := fakeSource{1: pipeline("Root"), 2: agent("Worker", 1)}
	override := pipeline("Root", 2)
	err := applicationcomposition.Check(context.Background(), source, id(1), &override)
	if applicationcomposition.Refusal(err) == nil {
		t.Fatalf("the new Agent node closes a cycle: %v", err)
	}
}

func TestReadPipelineFindsAgentNodesAndStaticPauses(t *testing.T) {
	nodes, paused := applicationcomposition.ReadPipeline("entry_point: a\ninterrupt_before: [b]\nnodes:\n  - id: a\n    type: agent\n    tool: worker\n  - id: b\n    type: llm\n  - &n\n    id: c\n    type: agent\n    tool: other\n")
	if !paused || len(nodes) != 2 || nodes[0] != (applicationcomposition.AgentNode{ID: "a", Alias: "worker"}) || nodes[1].Alias != "other" {
		t.Fatalf("nodes = %+v paused = %v", nodes, paused)
	}
	for _, text := range []string{"", "nodes: [", "- just a list", "interrupt_after: []\nnodes: []\n"} {
		if nodes, paused := applicationcomposition.ReadPipeline(text); len(nodes) != 0 || paused {
			t.Fatalf("%q: nodes = %+v paused = %v", text, nodes, paused)
		}
	}
	if nodes, _ := applicationcomposition.ReadPipeline(strings.Repeat("x", 512*1024+1)); nodes != nil {
		t.Fatal("an over-bound definition is not parsed")
	}
}

type countingSource struct {
	fakeSource
	reads map[int64]int
}

func (s *countingSource) Version(ctx context.Context, id applicationcomposition.Identity) (applicationcomposition.Version, bool, error) {
	s.reads[id.VersionID]++
	return s.fakeSource.Version(ctx, id)
}

// The save check's database budget: each saved version is read at most once
// per walk, so one check costs at most MaxApplicationHops+1 version reads
// (the root plus every entered attachment), each one version row plus its
// attachment rows.
func TestEachVersionIsReadAtMostOnceWithinTheHopBudget(t *testing.T) {
	// A diamond: both pipelines run the same agent, which carries the same tool.
	diamond := &countingSource{fakeSource: fakeSource{
		1: pipeline("Root", 2, 3), 2: pipeline("Left", 4), 3: pipeline("Right", 4), 4: agent("Shared", 5), 5: agent("Tool"),
	}, reads: map[int64]int{}}
	if err := applicationcomposition.Check(context.Background(), diamond, id(1), nil); err != nil {
		t.Fatalf("diamond: %v", err)
	}
	for version, reads := range diamond.reads {
		if reads != 1 {
			t.Fatalf("version %d read %d times", version, reads)
		}
	}
	wide := &countingSource{fakeSource: fakeSource{}, reads: map[int64]int{}}
	var attached []int64
	for n := int64(2); n < applicationcomposition.MaxApplicationHops+12; n++ {
		wide.fakeSource[n] = agent(fmt.Sprintf("Leaf %d", n))
		attached = append(attached, n)
	}
	wide.fakeSource[1] = agent("Root", attached...)
	if applicationcomposition.Refusal(applicationcomposition.Check(context.Background(), wide, id(1), nil)) == nil {
		t.Fatal("past the hop budget must be refused")
	}
	total := 0
	for _, reads := range wide.reads {
		total += reads
	}
	if total > applicationcomposition.MaxApplicationHops+2 {
		t.Fatalf("a refused walk read %d versions, budget %d", total, applicationcomposition.MaxApplicationHops+2)
	}
}
