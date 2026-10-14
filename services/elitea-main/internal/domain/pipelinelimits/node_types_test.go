package pipelinelimits_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/pipelinelimits"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
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

func mapPipeline(workerType, extraNode string) string {
	return "entry_point: fan\nstate:\n  items: list\nnodes:\n" +
		"  - id: fan\n    type: map\n    worker: w\n    transition: END\n" +
		"  - id: w\n    type: " + workerType + "\n" + extraNode
}

func TestCheckAdmitsAMapNodeWithAStateModifierWorker(t *testing.T) {
	if err := pipelinelimits.Check(mapPipeline("state_modifier", "")); err != nil {
		t.Fatalf("a Map node with a State modifier worker must be admitted: %v", err)
	}
}

func TestCheckRefusesMapFormsTheRuntimeCannotRecoverSafely(t *testing.T) {
	cases := map[string]string{
		"agent worker":     mapPipeline("agent", ""),
		"llm worker":       mapPipeline("llm", ""),
		"toolkit worker":   mapPipeline("toolkit", ""),
		"agent elsewhere":  mapPipeline("state_modifier", "  - id: other\n    type: agent\n"),
		"agent before map": "nodes:\n  - id: a\n    type: agent\n" + mapPipeline("state_modifier", "")[len("entry_point: fan\nstate:\n  items: list\nnodes:\n"):],
	}
	for name, doc := range cases {
		api := refusal(t, doc)
		if api.Code != pipelinelimits.CodeMapFormUnsupported {
			t.Errorf("%s: code = %s, want %s", name, api.Code, pipelinelimits.CodeMapFormUnsupported)
		}
		if !strings.Contains(api.Message, `Node "fan"`) || !strings.Contains(api.Message, "Map node") {
			t.Errorf("%s: message %q does not name the node and the form", name, api.Message)
		}
		if pipelinelimits.Refusal(api) == nil {
			t.Errorf("%s: Refusal must recognise the Map form code", name)
		}
	}
}

func TestCheckLeavesUnreadableMapWorkersToTheCompiler(t *testing.T) {
	for name, doc := range map[string]string{
		"no worker":      "nodes:\n  - id: fan\n    type: map\n",
		"unknown worker": "nodes:\n  - id: fan\n    type: map\n    worker: ghost\n",
		"non-string":     "nodes:\n  - id: fan\n    type: map\n    worker: [w]\n  - id: w\n    type: llm\n",
	} {
		if err := pipelinelimits.Check(doc); err != nil {
			t.Errorf("%s: must not be refused at save, got %v", name, err)
		}
	}
}
