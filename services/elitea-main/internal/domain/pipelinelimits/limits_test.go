package pipelinelimits_test

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/pipelinelimits"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

// yamlWithNodes builds a pipeline whose `nodes` list holds n entries. Ids are
// bare numbers on purpose: the count must not depend on id typing.
func yamlWithNodes(n int) string {
	var b strings.Builder
	b.WriteString("entry_point: 1\nnodes:\n")
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "  - id: %d\n    type: state_modifier\n    transition: %d\n", i, i+1)
	}
	return b.String()
}

func padTo(prefix string, size int) string {
	return prefix + strings.Repeat("#", size-len(prefix))
}

func TestCheckBytesBoundary(t *testing.T) {
	base := "nodes:\n  - id: a\n    type: state_modifier\n# "
	if err := pipelinelimits.Check(padTo(base, pipelinelimits.MaxInstructionsBytes)); err != nil {
		t.Fatalf("exactly %d bytes must be accepted: %v", pipelinelimits.MaxInstructionsBytes, err)
	}
	err := pipelinelimits.Check(padTo(base, pipelinelimits.MaxInstructionsBytes+1))
	if !errors.Is(err, pipelinelimits.ErrInstructionsTooLarge) {
		t.Fatalf("limit+1 bytes: got %v, want ErrInstructionsTooLarge", err)
	}
}

func TestCheckNodesBoundary(t *testing.T) {
	if err := pipelinelimits.Check(yamlWithNodes(pipelinelimits.MaxNodes)); err != nil {
		t.Fatalf("%d nodes must be accepted: %v", pipelinelimits.MaxNodes, err)
	}
	err := pipelinelimits.Check(yamlWithNodes(pipelinelimits.MaxNodes + 1))
	if !errors.Is(err, pipelinelimits.ErrTooManyNodes) {
		t.Fatalf("%d nodes: got %v, want ErrTooManyNodes", pipelinelimits.MaxNodes+1, err)
	}
}

func TestCheckNodesBoundaryFollowsAliases(t *testing.T) {
	list := strings.TrimPrefix(yamlWithNodes(pipelinelimits.MaxNodes+1), "entry_point: 1\nnodes:\n")
	doc := "defs: &n\n" + list + "nodes: *n\n"
	if err := pipelinelimits.Check(doc); !errors.Is(err, pipelinelimits.ErrTooManyNodes) {
		t.Fatalf("aliased nodes list over the bound: got %v, want ErrTooManyNodes", err)
	}
}

func TestCheckLeavesUnparseableAndShapelessYAMLToTheWorker(t *testing.T) {
	for name, doc := range map[string]string{
		"empty":            "",
		"not yaml":         "nodes: [unterminated\n  - : :",
		"prose":            "You are a helpful assistant.\nAnswer briefly.",
		"scalar root":      "42",
		"sequence root":    "- a\n- b",
		"nodes not a list": "nodes:\n  a: 1\n  b: 2\n",
		"no nodes":         "entry_point: 1\n",
		"multi document":   "nodes: []\n---\nnodes: []\n",
	} {
		if err := pipelinelimits.Check(doc); err != nil {
			t.Errorf("%s: must not be refused at save, got %v", name, err)
		}
	}
}

func TestCheckSizeIsRefusedWithoutParsing(t *testing.T) {
	// Not YAML at all: the size refusal must not depend on a parse.
	doc := strings.Repeat("\x00\xff", pipelinelimits.MaxInstructionsBytes)
	if err := pipelinelimits.Check(doc); !errors.Is(err, pipelinelimits.ErrInstructionsTooLarge) {
		t.Fatalf("got %v, want ErrInstructionsTooLarge", err)
	}
}

func TestRefusalsAreReadableTypedAPIErrorsWithoutUserContent(t *testing.T) {
	secret := "sk-live-very-secret-token"
	over := padTo("# "+secret+"\n", pipelinelimits.MaxInstructionsBytes+1)
	tooMany := strings.Replace(yamlWithNodes(pipelinelimits.MaxNodes+1), "state_modifier", secret, 1)
	for name, tc := range map[string]struct {
		doc  string
		code string
		text string
	}{
		"size":  {over, "PIPELINE_INSTRUCTIONS_TOO_LARGE", "512 KiB"},
		"nodes": {tooMany, "PIPELINE_TOO_MANY_NODES", "128 nodes"},
	} {
		err := pipelinelimits.Check(tc.doc)
		var api *apierr.APIError
		if !errors.As(err, &api) {
			t.Fatalf("%s: %v is not an *apierr.APIError", name, err)
		}
		if api.Status != http.StatusBadRequest || api.Code != tc.code {
			t.Errorf("%s: status/code = %d/%s, want 400/%s", name, api.Status, api.Code, tc.code)
		}
		if !strings.Contains(api.Message, tc.text) || !strings.Contains(api.Message, "before saving") {
			t.Errorf("%s: message %q does not name the limit and the remedy", name, api.Message)
		}
		if strings.Contains(api.Message, secret) {
			t.Errorf("%s: message echoes user content", name)
		}
	}
}
