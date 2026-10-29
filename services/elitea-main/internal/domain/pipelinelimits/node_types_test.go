package pipelinelimits_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/pipelinelimits"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

func pipelineWithNode(id, nodeType string) string {
	return "entry_point: " + id + "\nstate:\n  records: list\nnodes:\n" +
		"  - id: " + id + "\n    type: " + nodeType + "\n    transition: END\n"
}

func refusal(t *testing.T, doc string) *apierr.APIError {
	t.Helper()
	var api *apierr.APIError
	if err := pipelinelimits.Check(doc); !errors.As(err, &api) {
		t.Fatalf("Check(%q) = %v, want an *apierr.APIError", doc, err)
	}
	return api
}

func TestCheckAdmitsEveryNodeTypeTheCompilerAdmits(t *testing.T) {
	for _, nodeType := range []string{
		"code", "decision", "map", "parallel", "agent", "toolkit", "mcp",
		"hitl", "llm", "printer", "router", "state_modifier", "split_out", "aggregate",
	} {
		if err := pipelinelimits.Check(pipelineWithNode("n1", nodeType)); err != nil {
			t.Errorf("type %s: got %v, want admitted (the compiler has an arm for it)", nodeType, err)
		}
	}
}

func TestCheckRefusesUnknownNodeTypes(t *testing.T) {
	for _, nodeType := range []string{"custom", "function", "loop", "Split_Out", "END"} {
		api := refusal(t, pipelineWithNode("7", nodeType))
		if api.Code != pipelinelimits.CodeNodeTypeUnsupported {
			t.Errorf("%s: code = %s, want %s", nodeType, api.Code, pipelinelimits.CodeNodeTypeUnsupported)
		}
		if !strings.Contains(api.Message, `Node "7"`) || !strings.Contains(api.Message, "does not support") {
			t.Errorf("%s: message %q does not name the node and the refusal", nodeType, api.Message)
		}
	}
}

func TestCheckLeavesNonStringNodeTypesToTheCompiler(t *testing.T) {
	for name, doc := range map[string]string{
		"missing type":  "nodes:\n  - id: a\n",
		"integer type":  "nodes:\n  - id: a\n    type: 3\n",
		"list type":     "nodes:\n  - id: a\n    type: [llm]\n",
		"null type":     "nodes:\n  - id: a\n    type: null\n",
		"scalar entry":  "nodes:\n  - split_out\n",
		"agent prose":   "You are a helpful assistant.",
		"nested nodes":  "nodes:\n  - id: a\n    type: llm\n    extra:\n      nodes:\n        - type: split_out\n",
		"second doc":    "nodes: []\n---\nnodes:\n  - id: a\n    type: split_out\n",
		"non-node list": "items:\n  - id: a\n    type: split_out\n",
	} {
		if err := pipelinelimits.Check(doc); err != nil {
			t.Errorf("%s: must not be refused at save, got %v", name, err)
		}
	}
}

func TestNodeTypeRefusalNamesOnlyShortPrintableValues(t *testing.T) {
	secret := "sk-live-very-secret-token-" + strings.Repeat("x", 128)
	cases := map[string]string{
		"long id":       pipelineWithNode(secret, "custom"),
		"long type":     pipelineWithNode("a", secret),
		"control in id": "nodes:\n  - id: \"a\\u0007b\"\n    type: custom\n",
		"quote in type": "nodes:\n  - id: a\n    type: 'x\"y'\n",
		"mapping id":    "nodes:\n  - id: {k: v}\n    type: custom\n",
	}
	for name, doc := range cases {
		api := refusal(t, doc)
		if strings.Contains(api.Message, secret) || strings.ContainsAny(api.Message, "\a{") || strings.Contains(api.Message, `x"y`) {
			t.Errorf("%s: message %q echoes unbounded or unprintable user content", name, api.Message)
		}
		if !strings.Contains(api.Message, "before saving") {
			t.Errorf("%s: message %q has no remedy", name, api.Message)
		}
	}
}

func TestNodeCountIsCheckedBeforeNodeTypes(t *testing.T) {
	doc := strings.Replace(yamlWithNodes(pipelinelimits.MaxNodes+1), "state_modifier", "split_out", 1)
	if err := pipelinelimits.Check(doc); !errors.Is(err, pipelinelimits.ErrTooManyNodes) {
		t.Fatalf("got %v, want ErrTooManyNodes before any node is inspected", err)
	}
}

func parallelOver(secondType string) string {
	return "entry_point: gather\nstate:\n  joined: list\n  out: str\nnodes:\n" +
		"  - id: gather\n    type: parallel\n    branches: [{id: one, node: first}, {id: two, node: second}]\n" +
		"    max_concurrency: 2\n    wait: all\n    output: [joined]\n    transition: END\n" +
		"  - id: first\n    type: agent\n    tool: Research\n    output: [out]\n" +
		"  - id: second\n    type: " + secondType + "\n    output: [out]\n"
}

// An inline LLM node (FU-247) or any other non-Agent node cannot run as a
// fixed Parallel branch: refused at save and at start, naming both nodes.
func TestCheckRefusesAParallelBranchThatIsNotAnAgentNode(t *testing.T) {
	for _, nodeType := range []string{"llm", "state_modifier", "toolkit"} {
		doc := parallelOver(nodeType)
		for name, err := range map[string]error{
			"save":  pipelinelimits.Check(doc),
			"start": pipelinelimits.CheckStart(doc, []string{"Research"}),
		} {
			var api *apierr.APIError
			if !errors.As(err, &api) || api.Code != pipelinelimits.CodeParallelBranchUnsupported {
				t.Fatalf("%s %s: got %v, want %s", nodeType, name, err, pipelinelimits.CodeParallelBranchUnsupported)
			}
			if pipelinelimits.Refusal(err) != api {
				t.Errorf("%s %s: Refusal does not answer the branch refusal", nodeType, name)
			}
			for _, want := range []string{`Node "gather"`, `node "second"`, `"` + nodeType + `" node`} {
				if !strings.Contains(api.Message, want) {
					t.Errorf("%s %s: message %q does not contain %s", nodeType, name, api.Message, want)
				}
			}
		}
	}
}

func TestCheckAdmitsAgentBranchesAndLeavesMalformedBranchesToTheCompiler(t *testing.T) {
	for name, doc := range map[string]string{
		"agent branches":    parallelOver("agent"),
		"missing node":      strings.Replace(parallelOver("llm"), "node: second", "node: absent", 1),
		"non-scalar node":   strings.Replace(parallelOver("llm"), "node: second", "node: [second]", 1),
		"non-list branches": strings.Replace(parallelOver("llm"), "[{id: one, node: first}, {id: two, node: second}]", "first", 1),
		"integer type":      strings.Replace(parallelOver("llm"), "type: llm", "type: 3", 1),
		// The compiler runs the first node with an id, as here.
		"first declaration is the agent": parallelOver("agent") + "  - id: second\n    type: llm\n",
	} {
		if err := pipelinelimits.Check(doc); err != nil {
			t.Errorf("%s: got %v, want admitted", name, err)
		}
	}
}
