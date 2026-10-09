package pipelinelimits_test

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/pipelinelimits"
)

func directToolPipeline(nodeType, toolkit string) string {
	return "entry_point: fetch\nnodes:\n" +
		"  - id: fetch\n    type: " + nodeType + "\n    toolkit_name: " + toolkit +
		"\n    tool: list_issues\n    transition: END\n"
}

func TestCheckStartRefusesADirectToolNodeWhoseToolkitIsNotAttached(t *testing.T) {
	for nodeType, panel := range map[string]string{"mcp": "Tools → MCP", "toolkit": "Tools → Toolkit"} {
		err := pipelinelimits.CheckStart(directToolPipeline(nodeType, "github_mcp"), []string{"jira", "Elitea Applications"})
		api := pipelinelimits.Refusal(err)
		if api == nil || api.Code != pipelinelimits.CodeToolkitNotAttached || api.Status != http.StatusUnprocessableEntity {
			t.Fatalf("%s: got %v, want a %s refusal", nodeType, err, pipelinelimits.CodeToolkitNotAttached)
		}
		for _, want := range []string{`Node "fetch"`, `"github_mcp" toolkit`, "not attached to this pipeline", `Attach "github_mcp" under ` + panel} {
			if !strings.Contains(api.Message, want) {
				t.Errorf("%s: message %q does not contain %q", nodeType, api.Message, want)
			}
		}
	}
}

func TestCheckStartAdmitsEveryNameTheWorkerBinds(t *testing.T) {
	for name, tc := range map[string]struct {
		toolkit  string
		attached []string
	}{
		"exact":               {"github_mcp", []string{"github_mcp"}},
		"legacy key spaces":   {"GitHub MCP", []string{"github_mcp"}},
		"legacy key case":     {"githubmcp", []string{"GitHub_MCP"}},
		"internal mcp":        {"Elitea Applications", []string{"Elitea Applications"}},
		"non-ascii alias":     {"Jïra", []string{"jira"}},
		"non-ascii attached":  {"jira", []string{"Jïra"}},
		"quoted numeric name": {`"42"`, []string{"42"}},
	} {
		if err := pipelinelimits.CheckStart(directToolPipeline("mcp", tc.toolkit), tc.attached); err != nil {
			t.Errorf("%s: got %v, want admitted (the Worker binds this alias or may)", name, err)
		}
	}
}

func TestCheckStartLeavesWhatItCannotJudgeToTheWorker(t *testing.T) {
	for name, doc := range map[string]string{
		"llm node toolkit":     "nodes:\n  - id: a\n    type: llm\n    toolkit_name: missing\n",
		"missing toolkit_name": "nodes:\n  - id: a\n    type: mcp\n    tool: x\n",
		"non-string toolkit":   "nodes:\n  - id: a\n    type: mcp\n    toolkit_name: [x]\n",
		"unparseable":          "nodes: [unterminated\n  - : :",
		"integer toolkit_name": "nodes:\n  - id: a\n    type: mcp\n    toolkit_name: 42\n",
		"second document":      "nodes: []\n---\nnodes:\n  - id: a\n    type: mcp\n    toolkit_name: x\n",
	} {
		if err := pipelinelimits.CheckStart(doc, nil); err != nil {
			t.Errorf("%s: got %v, want no start refusal", name, err)
		}
	}
}

func TestCheckStartKeepsTheSaveRefusalsAndTheirOrder(t *testing.T) {
	if err := pipelinelimits.CheckStart(strings.Repeat("#", pipelinelimits.MaxInstructionsBytes+1), nil); !errors.Is(err, pipelinelimits.ErrInstructionsTooLarge) {
		t.Fatalf("size: got %v", err)
	}
	if err := pipelinelimits.CheckStart(yamlWithNodes(pipelinelimits.MaxNodes+1), nil); !errors.Is(err, pipelinelimits.ErrTooManyNodes) {
		t.Fatalf("count: got %v", err)
	}
	// Canvas order: the first offending node is named, whichever rule it breaks.
	doc := "nodes:\n  - id: first\n    type: mcp\n    toolkit_name: absent\n  - id: second\n    type: custom\n"
	if api := pipelinelimits.Refusal(pipelinelimits.CheckStart(doc, nil)); api == nil || api.Code != pipelinelimits.CodeToolkitNotAttached || !strings.Contains(api.Message, `"first"`) {
		t.Fatalf("order: got %+v", api)
	}
	if api := pipelinelimits.Refusal(pipelinelimits.CheckStart(doc, nil)); pipelinelimits.Refusal(fmt.Errorf("wrapped: %w", api)) != api {
		t.Fatal("Refusal must see through wrapping")
	}
}

func TestToolkitRefusalNamesOnlyShortPrintableValues(t *testing.T) {
	long := strings.Repeat("t", 129)
	api := pipelinelimits.Refusal(pipelinelimits.CheckStart(directToolPipeline("mcp", long), nil))
	if api == nil || strings.Contains(api.Message, long) || !strings.Contains(api.Message, "Attach it under Tools → MCP") {
		t.Fatalf("got %+v, want a refusal that does not echo the over-bound name", api)
	}
}

func TestSaveCheckIgnoresToolAttachment(t *testing.T) {
	if err := pipelinelimits.Check(directToolPipeline("mcp", "not_attached_yet")); err != nil {
		t.Fatalf("a save must not require the toolkit to be attached first: %v", err)
	}
}
