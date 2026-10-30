package approvalpolicy

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/toolcategory"
)

func TestDefaultPolicyIsTheADRDefault(t *testing.T) {
	t.Parallel()
	policy := DefaultPolicy()
	gated := []toolcategory.Category{toolcategory.Delete, toolcategory.ExternalSend, toolcategory.Payment}
	for _, tier := range []agentlifecycle.Tier{agentlifecycle.TierHigh, agentlifecycle.TierCritical} {
		if got := policy.CategoriesFor(tier); !reflect.DeepEqual(got, gated) {
			t.Errorf("%s: %v", tier, got)
		}
	}
	for _, tier := range []agentlifecycle.Tier{agentlifecycle.TierLow, agentlifecycle.TierMedium} {
		if got := policy.CategoriesFor(tier); len(got) != 0 {
			t.Errorf("%s: %v", tier, got)
		}
	}
	if policy.RequiresHITL(agentlifecycle.TierCritical) {
		t.Error("the default requires a hitl node")
	}
	if !policy.Requires(agentlifecycle.TierHigh, toolcategory.ExternalSend) || policy.Requires(agentlifecycle.TierHigh, toolcategory.Write) {
		t.Error("Requires disagrees with the default")
	}
	// An unknown tier is the lowest.
	if got := policy.CategoriesFor("extreme"); len(got) != 0 {
		t.Errorf("unknown tier: %v", got)
	}
}

func TestNormalizeOrdersAndDeduplicates(t *testing.T) {
	t.Parallel()
	got := Normalize([]toolcategory.Category{"payment", "admin", "delete", "payment", "read"})
	want := []toolcategory.Category{toolcategory.Read, toolcategory.Delete, toolcategory.Payment}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Normalize = %v", got)
	}
}

var toolkits = []PipelineToolkit{
	{Name: "chat", Type: "slack"},
	{Name: "repo", Type: "github"},
	{Name: "vendor", Type: "mcp", PinsCategories: map[string][]toolcategory.Category{
		"charge": {toolcategory.Payment},
		"lookup": nil,
	}},
}

const skipsTheHITL = `
entry_point: draft
nodes:
  - id: draft
    type: llm
    transition: route
  - id: route
    type: router
    condition: "{{ x }}"
    routes: [review, post]
    default_output: END
  - id: review
    type: hitl
    routes: {approve: post, reject: END}
  - id: post
    type: toolkit
    toolkit_name: chat
    tool: send_message
    transition: END
`

const passesTheHITL = `
entry_point: draft
nodes:
  - id: draft
    type: llm
    transition: review
  - id: review
    type: hitl
    routes: {approve: post, reject: END}
  - id: post
    type: toolkit
    toolkit_name: chat
    tool: send_message
    transition: END
`

func TestCheckPipelineHITL(t *testing.T) {
	t.Parallel()
	// Negative: the router reaches the send node past the hitl node.
	err := CheckPipelineHITL(skipsTheHITL, toolkits, agentlifecycle.TierHigh)
	refusal := Refusal(fmt.Errorf("wrapped: %w", err))
	if refusal == nil {
		t.Fatalf("a path that skips the hitl node is admitted: %v", err)
	}
	for _, part := range []string{`Node "post"`, `"send_message"`, "external_send", "high-tier", `"draft" -> "route" -> "post"`} {
		if !strings.Contains(refusal.Message, part) {
			t.Errorf("the refusal %q does not name %s", refusal.Message, part)
		}
	}
	if refusal.Status != 422 {
		t.Errorf("status = %d", refusal.Status)
	}
	// Positive: every path to the send node passes the hitl node.
	if err := CheckPipelineHITL(passesTheHITL, toolkits, agentlifecycle.TierHigh); err != nil {
		t.Fatalf("a pipeline with the hitl node is refused: %v", err)
	}
}

func TestCheckPipelineHITLCases(t *testing.T) {
	t.Parallel()
	for name, test := range map[string]struct {
		yaml    string
		refused bool
	}{
		"a write tool needs no hitl": {`
entry_point: a
nodes:
  - {id: a, type: toolkit, toolkit_name: repo, tool: create_issue, transition: END}
`, false},
		"a comment is external_send": {`
entry_point: a
nodes:
  - {id: a, type: toolkit, toolkit_name: repo, tool: comment_on_issue}
`, true},
		"a pinned MCP payment tool": {`
entry_point: a
nodes:
  - {id: a, type: code, transition: b}
  - {id: b, type: mcp, toolkit_name: vendor, tool: charge}
`, true},
		"a pin without a category is write": {`
entry_point: a
nodes:
  - {id: a, type: mcp, toolkit_name: vendor, tool: lookup}
`, false},
		"an unreachable send node": {`
entry_point: a
nodes:
  - {id: a, type: llm, transition: END}
  - {id: b, type: toolkit, toolkit_name: chat, tool: send_message}
`, false},
		"a decision node's route": {`
entry_point: a
nodes:
  - {id: a, type: decision, nodes: [c], default_output: END}
  - {id: c, type: toolkit, toolkit_name: chat, tool: send_message}
`, true},
		"a parallel branch": {`
entry_point: p
nodes:
  - id: p
    type: parallel
    branches: [{id: one, node: s}]
    max_concurrency: 1
    wait: all
    output: [x]
  - {id: s, type: toolkit, toolkit_name: chat, tool: send_message}
`, true},
		"a recovery route": {`
entry_point: a
nodes:
  - id: a
    type: code
    recovery: {on_failure: {route: s}}
  - {id: s, type: toolkit, toolkit_name: chat, tool: send_message}
`, true},
		"a legacy toolkit and node spelling": {`
entry_point: Start Node
nodes:
  - {id: start_node, type: toolkit, toolkit_name: Chat, tool: send_message}
`, true},
		"an unattached toolkit is left to the worker": {`
entry_point: a
nodes:
  - {id: a, type: toolkit, toolkit_name: other, tool: send_message}
`, false},
		"malformed yaml is left to the compiler": {`entry_point: [`, false},
	} {
		err := CheckPipelineHITL(test.yaml, toolkits, agentlifecycle.TierCritical)
		if (err != nil) != test.refused {
			t.Errorf("%s: err = %v, want refused %t", name, err, test.refused)
		}
	}
}

// A static pause is a person's approval (review finding 3): a send node the
// pipeline's interrupt_before names, or one after a node its interrupt_after
// names, is gated.
func TestCheckPipelineHITLStaticPauses(t *testing.T) {
	t.Parallel()
	for name, test := range map[string]struct {
		yaml    string
		refused bool
	}{
		"interrupt_before the send node": {`
entry_point: a
interrupt_before: [s]
nodes:
  - {id: a, type: llm, transition: s}
  - {id: s, type: toolkit, toolkit_name: chat, tool: send_message}
`, false},
		"interrupt_after a node before the send node": {`
entry_point: a
interrupt_after: [a]
nodes:
  - {id: a, type: llm, transition: s}
  - {id: s, type: toolkit, toolkit_name: chat, tool: send_message}
`, false},
		"interrupt_after the send node itself runs it first": {`
entry_point: a
interrupt_after: [s]
nodes:
  - {id: a, type: llm, transition: s}
  - {id: s, type: toolkit, toolkit_name: chat, tool: send_message}
`, true},
		"interrupt_before another node leaves a path open": {`
entry_point: a
interrupt_before: [b]
nodes:
  - {id: a, type: decision, nodes: [b, s]}
  - {id: b, type: llm, transition: s}
  - {id: s, type: toolkit, toolkit_name: chat, tool: send_message}
`, true},
	} {
		err := CheckPipelineHITL(test.yaml, toolkits, agentlifecycle.TierHigh)
		if (err != nil) != test.refused {
			t.Errorf("%s: err = %v, want refused %t", name, err, test.refused)
		}
	}
}

const sendsDirectly = `
entry_point: post
nodes:
  - {id: post, type: toolkit, toolkit_name: chat, tool: send_message}
`

// A saved child pipeline is part of the path that reaches it (review finding
// 2): a hitl node before the agent node covers the child; otherwise the
// child's own paths need one. A child no agent node selects is checked from
// its own entry point.
func TestCheckPipelineGraphChildren(t *testing.T) {
	t.Parallel()
	child := func(instructions string, tier agentlifecycle.Tier) *PipelineGraph {
		return &PipelineGraph{IsPipeline: true, Instructions: instructions, Toolkits: toolkits, Tier: tier}
	}
	runsChild := `
entry_point: a
nodes:
  - {id: a, type: llm, transition: run}
  - {id: run, type: agent, tool: Sender, transition: END}
`
	reviewsThenRunsChild := `
entry_point: review
nodes:
  - {id: review, type: hitl, routes: {approve: run, reject: END}}
  - {id: run, type: agent, tool: Sender, transition: END}
`
	pausesBeforeChild := `
entry_point: a
interrupt_before: [run]
nodes:
  - {id: a, type: llm, transition: run}
  - {id: run, type: agent, tool: Sender, transition: END}
`
	for name, test := range map[string]struct {
		graph   *PipelineGraph
		refused bool
		names   string
	}{
		"an agent node runs a child that sends": {
			&PipelineGraph{IsPipeline: true, Instructions: runsChild, Tier: agentlifecycle.TierHigh,
				Children: map[string]*PipelineGraph{"Sender": child(sendsDirectly, agentlifecycle.TierHigh)}},
			true, `"a" -> "run" -> "post"`,
		},
		"a hitl node before the agent node covers the child": {
			&PipelineGraph{IsPipeline: true, Instructions: reviewsThenRunsChild, Tier: agentlifecycle.TierHigh,
				Children: map[string]*PipelineGraph{"Sender": child(sendsDirectly, agentlifecycle.TierHigh)}},
			false, "",
		},
		"a static pause before the agent node covers the child": {
			&PipelineGraph{IsPipeline: true, Instructions: pausesBeforeChild, Tier: agentlifecycle.TierHigh,
				Children: map[string]*PipelineGraph{"Sender": child(sendsDirectly, agentlifecycle.TierHigh)}},
			false, "",
		},
		"a child with its own hitl node": {
			&PipelineGraph{IsPipeline: true, Instructions: runsChild, Tier: agentlifecycle.TierHigh,
				Children: map[string]*PipelineGraph{"Sender": child(passesTheHITL, agentlifecycle.TierHigh)}},
			false, "",
		},
		"a grandchild that sends": {
			&PipelineGraph{IsPipeline: true, Instructions: runsChild, Tier: agentlifecycle.TierHigh,
				Children: map[string]*PipelineGraph{"Sender": {
					IsPipeline: true, Instructions: runsChild, Tier: agentlifecycle.TierHigh, Toolkits: toolkits,
					Children: map[string]*PipelineGraph{"Sender": child(sendsDirectly, agentlifecycle.TierHigh)},
				}}},
			true, `"a" -> "run" -> "a" -> "run" -> "post"`,
		},
		"a child the rule does not cover": {
			&PipelineGraph{IsPipeline: true, Instructions: runsChild,
				Children: map[string]*PipelineGraph{"Sender": child(sendsDirectly, "")}},
			false, "",
		},
		"a pipeline attached to an agent that is not a pipeline": {
			&PipelineGraph{Children: map[string]*PipelineGraph{"Sender": child(sendsDirectly, agentlifecycle.TierHigh)}},
			true, `"Sender" -> "post"`,
		},
		"gated on the agent node path, ungated through an llm node": {
			&PipelineGraph{IsPipeline: true, Tier: agentlifecycle.TierHigh, Instructions: `
entry_point: route
nodes:
  - {id: route, type: decision, nodes: [review, ask]}
  - {id: review, type: hitl, routes: {approve: run, reject: END}}
  - {id: run, type: agent, tool: Sender, transition: END}
  - {id: ask, type: llm, tool_names: {Sender: [Sender]}, transition: END}
`,
				Children: map[string]*PipelineGraph{"Sender": child(sendsDirectly, agentlifecycle.TierHigh)}},
			true, `"route" -> "ask" -> "post"`,
		},
		"an llm node behind a hitl node covers the child": {
			&PipelineGraph{IsPipeline: true, Tier: agentlifecycle.TierHigh, Instructions: `
entry_point: review
nodes:
  - {id: review, type: hitl, routes: {approve: ask, reject: END}}
  - {id: ask, type: llm, tool_names: {Sender: [Sender]}, transition: END}
`,
				Children: map[string]*PipelineGraph{"Sender": child(sendsDirectly, agentlifecycle.TierHigh)}},
			false, "",
		},
		"an llm node behind a static pause covers the child": {
			&PipelineGraph{IsPipeline: true, Tier: agentlifecycle.TierHigh, Instructions: `
entry_point: a
interrupt_after: [a]
nodes:
  - {id: a, type: llm, transition: ask}
  - {id: ask, type: llm, tool_names: {sender: [Sender]}, transition: END}
`,
				Children: map[string]*PipelineGraph{"Sender": child(sendsDirectly, agentlifecycle.TierHigh)}},
			false, "",
		},
		"an llm node whose tool list for the child is empty": {
			&PipelineGraph{IsPipeline: true, Tier: agentlifecycle.TierHigh, Instructions: `
entry_point: ask
nodes:
  - {id: ask, type: llm, tool_names: {Sender: []}, transition: review}
  - {id: review, type: hitl, routes: {approve: run, reject: END}}
  - {id: run, type: agent, tool: Sender, transition: END}
`,
				Children: map[string]*PipelineGraph{"Sender": child(sendsDirectly, agentlifecycle.TierHigh)}},
			false, "",
		},
		"a pipeline attached to a pipeline and no agent node": {
			&PipelineGraph{IsPipeline: true, Instructions: passesTheHITL, Tier: agentlifecycle.TierHigh, Toolkits: toolkits,
				Children: map[string]*PipelineGraph{"Other": child(sendsDirectly, agentlifecycle.TierHigh)}},
			true, `"Other" -> "post"`,
		},
	} {
		err := CheckPipelineGraph(test.graph)
		if (err != nil) != test.refused {
			t.Errorf("%s: err = %v, want refused %t", name, err, test.refused)
			continue
		}
		if test.refused {
			if refusal := Refusal(err); refusal == nil || !strings.Contains(refusal.Message, test.names) {
				t.Errorf("%s: the refusal %v does not name the path %s", name, err, test.names)
			}
		}
	}
}

func TestCompositionRefusalIsTyped(t *testing.T) {
	t.Parallel()
	if refusal := Refusal(CompositionRefusal("its saved pipelines form a cycle")); refusal == nil || refusal.Status != 422 {
		t.Fatalf("refusal = %v", refusal)
	}
}
