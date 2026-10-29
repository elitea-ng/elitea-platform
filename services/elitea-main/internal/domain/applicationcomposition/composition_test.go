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
	nodes, paused, parallel, _ := applicationcomposition.ReadPipeline("entry_point: a\ninterrupt_before: [b]\nnodes:\n  - id: a\n    type: agent\n    tool: worker\n  - id: b\n    type: llm\n  - &n\n    id: c\n    type: agent\n    tool: other\n")
	if !paused || parallel != "" || len(nodes) != 2 || nodes[0] != (applicationcomposition.AgentNode{ID: "a", Alias: "worker"}) || nodes[1].Alias != "other" {
		t.Fatalf("nodes = %+v paused = %v parallel = %q", nodes, paused, parallel)
	}
	for _, text := range []string{"", "nodes: [", "- just a list", "interrupt_after: []\nnodes: []\n"} {
		if nodes, paused, parallel, _ := applicationcomposition.ReadPipeline(text); len(nodes) != 0 || paused || parallel != "" {
			t.Fatalf("%q: nodes = %+v paused = %v parallel = %q", text, nodes, paused, parallel)
		}
	}
	if nodes, _, _, _ := applicationcomposition.ReadPipeline(strings.Repeat("x", 512*1024+1)); nodes != nil {
		t.Fatal("an over-bound definition is not parsed")
	}
}

func TestReadPipelineFindsTheFirstParallelNode(t *testing.T) {
	_, _, parallel, _ := applicationcomposition.ReadPipeline("state:\n  joined: list\nentry_point: inner\nnodes:\n  - id: inner\n    type: parallel\n    branches: [{id: one, node: a}, {id: two, node: b}]\n    max_concurrency: 2\n    wait: all\n    output: [joined]\n    transition: END\n  - id: a\n    type: agent\n    tool: worker\n  - id: b\n    type: agent\n    tool: worker\n")
	if parallel != "inner" {
		t.Fatalf("parallel = %q", parallel)
	}
}

func parallelPipeline(name string, attached ...int64) applicationcomposition.Version {
	version := pipeline(name, attached...)
	version.ParallelNode = "gather"
	return version
}

// branches marks every Agent node of version as a fixed Parallel branch.
func branches(version applicationcomposition.Version) applicationcomposition.Version {
	for i := range version.AgentNodes {
		version.AgentNodes[i].Branch = true
	}
	return version
}

// FU-249: a saved pipeline with a Parallel node runs in the started pipeline and
// in a saved pipeline the root reaches through pipeline Agent nodes only (a
// subgraph of the root graph). As an agent's tool, or inside another Parallel's
// branch, it is refused; the refusal names the pipeline, its node and the path.
func TestAPipelineWithAParallelRunsInTheRootGraphOnly(t *testing.T) {
	for name, source := range map[string]fakeSource{
		"started pipeline":    {1: parallelPipeline("Fan", 2), 2: agent("Worker")},
		"pipeline Agent node": {1: pipeline("Outer", 2), 2: parallelPipeline("Inner")},
		"two pipeline levels": {1: pipeline("Outer", 3), 3: pipeline("Middle", 2), 2: parallelPipeline("Inner")},
		"beside a Parallel":   {1: parallelPipeline("Outer", 2), 2: parallelPipeline("Inner")},
	} {
		if violation := check(t, source, 1); violation != nil {
			t.Fatalf("%s: a Parallel in the root graph saves: %v", name, violation)
		}
	}
	for name, source := range map[string]fakeSource{
		"Parallel branch":          {1: branches(parallelPipeline("Outer", 2)), 2: parallelPipeline("Inner")},
		"below a Parallel branch":  {1: branches(parallelPipeline("Outer", 3)), 3: pipeline("Middle", 2), 2: parallelPipeline("Inner")},
		"agent tool":               {1: agent("Outer", 2), 2: parallelPipeline("Inner")},
		"agent tool, deeper":       {1: agent("Outer", 3), 3: pipeline("Middle", 2), 2: parallelPipeline("Inner")},
		"pipeline Agent node tool": {1: pipeline("Outer", 3), 3: agent("Worker", 2), 2: parallelPipeline("Inner")},
	} {
		violation := check(t, source, 1)
		expect(t, violation, applicationcomposition.BoundNestedParallel)
		if !strings.Contains(violation.Error(), `Pipeline "Inner" has Parallel node "gather"`) || !strings.Contains(violation.Error(), `"Outer" → `) {
			t.Fatalf("%s: message = %s", name, violation)
		}
	}
}

func TestReadPipelineMarksTheAgentNodesAParallelRunsAsBranches(t *testing.T) {
	nodes, _, _, _ := applicationcomposition.ReadPipeline("state:\n  joined: list\nentry_point: fan\nnodes:\n  - id: fan\n    type: parallel\n    branches: [{id: one, node: a}]\n    max_concurrency: 1\n    wait: all\n    output: [joined]\n    transition: other\n  - id: a\n    type: agent\n    tool: worker\n  - id: other\n    type: agent\n    tool: worker\n    transition: END\n")
	if len(nodes) != 2 || !nodes[0].Branch || nodes[1].Branch {
		t.Fatalf("nodes = %+v", nodes)
	}
}

func TestAChildPipelineGainingAParallelIsRefusedThroughAnAgentParent(t *testing.T) {
	source := fakeSource{1: agent("Outer", 2), 2: pipeline("Inner")}
	saved := parallelPipeline("Inner")
	err := applicationcomposition.CheckWithAncestors(context.Background(), ancestrySource{source}, id(2), &saved)
	refusal := applicationcomposition.Refusal(err)
	if refusal == nil || !strings.Contains(refusal.Message, `Parallel node "gather"`) {
		t.Fatalf("refusal = %+v (%v)", refusal, err)
	}
	graph := fakeSource{1: pipeline("Outer", 2), 2: pipeline("Inner")}
	if err := applicationcomposition.CheckWithAncestors(context.Background(), ancestrySource{graph}, id(2), &saved); err != nil {
		t.Fatalf("beneath a pipeline Agent node it saves: %v", err)
	}
}

func TestAStartRefusalNamesANestedParallelAndKeepsTheRegisteredTextOtherwise(t *testing.T) {
	err := applicationcomposition.Check(context.Background(), fakeSource{1: agent("Outer", 2), 2: parallelPipeline("Inner")}, id(1), nil)
	named := applicationcomposition.Refusal(applicationcomposition.AtStart(err))
	if named == nil || named.Status != http.StatusUnprocessableEntity || named.Code != applicationcomposition.Code || !strings.Contains(named.Message, `Parallel node "gather"`) {
		t.Fatalf("start refusal = %+v", named)
	}
	cycle := applicationcomposition.Check(context.Background(), fakeSource{1: agent("Planner", 2), 2: agent("Worker", 1)}, id(1), nil)
	if !errors.Is(applicationcomposition.AtStart(cycle), applicationcomposition.ErrAtStart) {
		t.Fatalf("a cycle keeps the registered start text: %v", applicationcomposition.AtStart(cycle))
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
// per check, and the cycle search reads at most MaxCycleVersions versions, so
// one check costs at most MaxCycleVersions+1 version reads (each one version
// row plus its attachment rows).
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
	for version, reads := range wide.reads {
		if reads != 1 {
			t.Fatalf("version %d read %d times", version, reads)
		}
	}
	if total > applicationcomposition.MaxCycleVersions+1 {
		t.Fatalf("a check read %d versions, budget %d", total, applicationcomposition.MaxCycleVersions+1)
	}
}

// PLAN §8.8: Orchestrator → Sub-orchestrator → Leaf, with several
// sub-orchestrators on one level, is the limit; a leaf with its own agent
// below any of them is one level too many.
func TestSeveralSubOrchestratorsOnOneLevelAreAllowed(t *testing.T) {
	source := fakeSource{
		1: agent("Orchestrator", 2, 3),
		2: agent("Research", 4, 5), 3: agent("Writing", 6),
		4: agent("Search"), 5: agent("Summarize"), 6: agent("Draft"),
	}
	if violation := check(t, source, 1); violation != nil {
		t.Fatalf("three levels with parallel sub-orchestrators must save: %v", violation)
	}
	source[6] = agent("Draft", 7)
	source[7] = agent("Spell check")
	violation := check(t, source, 1)
	expect(t, violation, applicationcomposition.BoundAgentTiers)
	if !strings.Contains(violation.Error(), `"Orchestrator" → "Writing" → "Draft" → "Spell check"`) {
		t.Fatalf("the refusal names the level: %s", violation)
	}
}

// A cycle through an attachment no Agent node runs is still a cycle.
func TestACycleThroughAnUnusedPipelineAttachmentIsRefused(t *testing.T) {
	unused := pipeline("Flow")
	unused.Attachments = attachments([]int64{1})
	unused.AgentNodes = nil
	source := fakeSource{1: agent("Planner", 2), 2: unused}
	expect(t, check(t, source, 1), applicationcomposition.BoundCycle)
}

// A version switch is checked as the version the parent would point to.
func TestAVersionSwitchThatClosesACycleIsRefused(t *testing.T) {
	source := fakeSource{1: agent("Parent", 2), 2: agent("Child v1"), 3: agent("Child v2", 1)}
	if violation := check(t, source, 1); violation != nil {
		t.Fatalf("v1 is valid: %v", violation)
	}
	switched := source[1]
	switched.Attachments = []applicationcomposition.Attachment{{Alias: "a3", Identity: id(3)}}
	err := applicationcomposition.Check(context.Background(), source, id(1), &switched)
	if refusal := applicationcomposition.Refusal(err); refusal == nil || !strings.Contains(refusal.Message, "circular") {
		t.Fatalf("switching to v2 closes a cycle: %v", err)
	}
}

// PLAN §8.8 (ad-hoc): a participant valid as a root is offered to the chat model
// only when it also fits one level deeper; one that does not reports the limit.
func TestChatToolFitOffersOnlyWhatFitsBelowTheChatModel(t *testing.T) {
	cases := []struct {
		name  string
		src   fakeSource
		tiers int
		fits  bool
	}{
		{"a leaf agent", agentChain(1), 1, true},
		{"an orchestrator of leaves", agentChain(2), 2, true},
		{"a three-level agent runs only when selected", agentChain(3), applicationcomposition.MaxAgentTiers, false},
		{"an empty pipeline consumes no tier", fakeSource{1: pipeline("Flow")}, 0, true},
		{"a pipeline of orchestrators", fakeSource{1: pipeline("Flow", 2), 2: agent("Planner", 3), 3: agent("Leaf")}, 2, true},
		{"an agent through a pipeline", fakeSource{1: agent("Planner", 2), 2: pipeline("Flow", 3), 3: agent("Leaf")}, 2, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if violation := check(t, tc.src, 1); violation != nil {
				t.Fatalf("each case is valid as a root: %v", violation)
			}
			fits, err := applicationcomposition.ChatToolFits(context.Background(), tc.src, []applicationcomposition.Identity{id(1)})
			if err != nil || fits[0] != (applicationcomposition.Fit{Tiers: tc.tiers, Fits: tc.fits}) {
				t.Fatalf("ChatToolFits = (%+v, %v), want (%d, %v)", fits, err, tc.tiers, tc.fits)
			}
		})
	}
}

func TestChatToolFitCountsTheChatModelsHop(t *testing.T) {
	source := fakeSource{}
	var attached []int64
	for n := int64(2); n < int64(applicationcomposition.MaxApplicationHops)+2; n++ {
		source[n] = agent(fmt.Sprintf("Leaf %d", n))
		attached = append(attached, n)
	}
	source[1] = agent("Root", attached...)
	if violation := check(t, source, 1); violation != nil {
		t.Fatalf("the hop budget is valid as a root: %v", violation)
	}
	fits, err := applicationcomposition.ChatToolFits(context.Background(), source, []applicationcomposition.Identity{id(1)})
	if err != nil || fits[0] != (applicationcomposition.Fit{Tiers: applicationcomposition.MaxAgentTiers}) {
		t.Fatalf("ChatToolFits = (%+v, %v), want the limit and no fit", fits, err)
	}
}

func TestChatToolFitLeavesAnUnreadableVersionToTheTurn(t *testing.T) {
	fits, err := applicationcomposition.ChatToolFits(context.Background(), fakeSource{}, []applicationcomposition.Identity{id(9)})
	if err != nil || fits[0] != (applicationcomposition.Fit{Fits: true}) {
		t.Fatalf("ChatToolFits = (%+v, %v)", fits, err)
	}
}

// PLAN §8.8, review item 3: the Worker builds every offered participant under one
// chat root sharing its hop budget, so three participants that each fit alone are
// offered only while they fit together; the one that does not is not offered and
// does not consume the budget, so a later small one is still offered.
func TestChatToolFitsShareTheChatModelsHopBudget(t *testing.T) {
	source := fakeSource{}
	wide := func(root, first int64, children int) {
		var attached []int64
		for n := first; n < first+int64(children); n++ {
			source[n] = agent(fmt.Sprintf("Leaf %d", n))
			attached = append(attached, n)
		}
		source[root] = agent(fmt.Sprintf("Root %d", root), attached...)
	}
	// Each participant costs 1 + its children hops below the chat model.
	wide(1, 100, 11) // 12 hops
	wide(2, 200, 11) // 12 hops: 24 together
	wide(3, 300, 11) // 12 hops: would be 36
	source[4] = agent("Small leaf")
	for _, n := range []int64{1, 2, 3, 4} {
		if violation := check(t, source, n); violation != nil {
			t.Fatalf("participant %d is valid alone: %v", n, violation)
		}
	}
	fits, err := applicationcomposition.ChatToolFits(context.Background(), source,
		[]applicationcomposition.Identity{id(1), id(2), id(3), id(4)})
	if err != nil {
		t.Fatal(err)
	}
	want := []applicationcomposition.Fit{{Tiers: 2, Fits: true}, {Tiers: 2, Fits: true}, {Tiers: applicationcomposition.MaxAgentTiers}, {Tiers: 1, Fits: true}}
	for i := range want {
		if fits[i] != want[i] {
			t.Fatalf("fits = %+v, want %+v", fits, want)
		}
	}
}

// Review item 4: the checkpoint-path budget of a saved pipeline composition.
func TestCheckpointPathsAdmitTheLimitAndRefuseOneMore(t *testing.T) {
	withNodes := func(nodes int) fakeSource {
		source := fakeSource{2: agent("Leaf")}
		root := applicationcomposition.Version{Kind: applicationcomposition.KindPipeline, Name: "Wide flow",
			Attachments: []applicationcomposition.Attachment{{Alias: "a2", Identity: id(2)}}}
		for n := 0; n < nodes; n++ {
			root.AgentNodes = append(root.AgentNodes, applicationcomposition.AgentNode{ID: fmt.Sprintf("n%d", n), Alias: "a2"})
		}
		source[1] = root
		return source
	}
	if violation := check(t, withNodes(applicationcomposition.MaxCheckpointPaths), 1); violation != nil {
		t.Fatalf("the path limit must save: %v", violation)
	}
	violation := check(t, withNodes(applicationcomposition.MaxCheckpointPaths+1), 1)
	expect(t, violation, applicationcomposition.BoundCheckpoints)
	if !strings.Contains(violation.Error(), `"Wide flow"`) {
		t.Fatalf("the refusal names the pipeline: %s", violation)
	}
}

// PM-4: an inline LLM Parallel branch runs in its own checkpoint family, so
// Main counts it against the same budget the Worker's collect_paths does.
func TestCheckpointPathsCountInlineLLMBranches(t *testing.T) {
	withNodes := func(agents, branches int) fakeSource {
		root := applicationcomposition.Version{Kind: applicationcomposition.KindPipeline, Name: "Wide flow",
			Attachments: []applicationcomposition.Attachment{{Alias: "a2", Identity: id(2)}}, LLMBranches: branches}
		for n := 0; n < agents; n++ {
			root.AgentNodes = append(root.AgentNodes, applicationcomposition.AgentNode{ID: fmt.Sprintf("n%d", n), Alias: "a2"})
		}
		return fakeSource{1: root, 2: agent("Leaf")}
	}
	limit := applicationcomposition.MaxCheckpointPaths
	if violation := check(t, withNodes(limit-16, 16), 1); violation != nil {
		t.Fatalf("112 Agent nodes and 16 inline branches must save: %v", violation)
	}
	expect(t, check(t, withNodes(limit-15, 16), 1), applicationcomposition.BoundCheckpoints)
}

func TestReadPipelineCountsOnlyParallelOwnedLLMNodes(t *testing.T) {
	_, _, _, branches := applicationcomposition.ReadPipeline("state:\n  joined: list\nentry_point: fan\nnodes:\n  - id: fan\n    type: parallel\n    branches: [{id: one, node: a}, {id: two, node: b}, {id: three, node: c}]\n    max_concurrency: 2\n    wait: all\n    output: [joined]\n    transition: tail\n  - id: a\n    type: llm\n  - id: b\n    type: llm\n  - id: c\n    type: agent\n    tool: worker\n  - id: tail\n    type: llm\n    transition: END\n")
	if branches != 2 {
		t.Fatalf("inline LLM branches = %d, want 2 (the free LLM node and the Agent branch are not counted)", branches)
	}
}

// Paths are counted through the saved pipelines the Agent nodes run.
func TestCheckpointPathsCountNestedPipelines(t *testing.T) {
	inner := applicationcomposition.Version{Kind: applicationcomposition.KindPipeline, Name: "Inner",
		Attachments: []applicationcomposition.Attachment{{Alias: "a3", Identity: id(3)}}}
	for n := 0; n < 64; n++ {
		inner.AgentNodes = append(inner.AgentNodes, applicationcomposition.AgentNode{ID: fmt.Sprintf("i%d", n), Alias: "a3"})
	}
	outer := applicationcomposition.Version{Kind: applicationcomposition.KindPipeline, Name: "Outer",
		Attachments: []applicationcomposition.Attachment{{Alias: "a2", Identity: id(2)}},
		AgentNodes:  []applicationcomposition.AgentNode{{ID: "o1", Alias: "a2"}, {ID: "o2", Alias: "a2"}}}
	// 2 + 2×64 = 130 paths.
	expect(t, check(t, fakeSource{1: outer, 2: inner, 3: agent("Leaf")}, 1), applicationcomposition.BoundCheckpoints)
}

func TestTheDepthRefusalNamesTheAgentNodeOnce(t *testing.T) {
	violation := check(t, pipelineChain(applicationcomposition.MaxPipelineDepth, true), 1)
	expect(t, violation, applicationcomposition.BoundPipelineDepth)
	if !strings.Contains(violation.Error(), `→ Agent node "`) || strings.Contains(violation.Error(), `"Agent node`) {
		t.Fatalf("the node is quoted once: %s", violation)
	}
}

// ancestrySource is a fakeSource that also answers the versions attaching ids.
type ancestrySource struct{ fakeSource }

func (s ancestrySource) Parents(_ context.Context, ids []applicationcomposition.Identity) ([]applicationcomposition.Identity, error) {
	var parents []applicationcomposition.Identity
	for n, version := range s.fakeSource {
		for _, attachment := range version.Attachments {
			for _, wanted := range ids {
				if attachment.Identity == wanted {
					parents = append(parents, id(n))
				}
			}
		}
	}
	return parents, nil
}

// Review round 2 (C): a change below can push an ancestor past a bound although
// the changed version alone fits; every version that attaches it is checked.
func TestCheckWithAncestorsRefusesAChangeThatPushesAnAncestorPastTheLimit(t *testing.T) {
	source := ancestrySource{fakeSource{1: agent("Agent 1", 2), 2: agent("Agent 2", 3), 3: agent("Agent 3"), 4: agent("Agent 4")}}
	// Agent 3 attaching Agent 4 is two levels from Agent 3, but four from Agent 1.
	changed := agent("Agent 3", 4)
	err := applicationcomposition.CheckWithAncestors(context.Background(), source, id(3), &changed)
	var violation *applicationcomposition.Violation
	if !errors.As(err, &violation) || violation.Bound != applicationcomposition.BoundAgentTiers ||
		!strings.Contains(violation.Error(), `"Agent 1" → "Agent 2" → "Agent 3" → "Agent 4"`) {
		t.Fatalf("err = %v", err)
	}
	// The same change one level higher up fits: three levels from Agent 1.
	source = ancestrySource{fakeSource{1: agent("Agent 1", 2), 2: agent("Agent 2"), 4: agent("Agent 4")}}
	changed = agent("Agent 2", 4)
	if err := applicationcomposition.CheckWithAncestors(context.Background(), source, id(2), &changed); err != nil {
		t.Fatalf("three levels must fit: %v", err)
	}
	// Without an ancestry the version is checked alone.
	if err := applicationcomposition.CheckWithAncestors(context.Background(), fakeSource{1: agent("Agent 1", 2), 2: agent("Agent 2", 3), 3: agent("Agent 3"), 4: agent("Agent 4")}, id(3), &changed); err != nil {
		t.Fatalf("no ancestry: %v", err)
	}
}
