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
