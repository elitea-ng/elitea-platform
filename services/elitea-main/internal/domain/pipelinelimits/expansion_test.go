package pipelinelimits_test

// SEC-11: Main refuses at save a pipeline definition whose anchor and alias
// expansion exceeds the budget the Worker parses it under, with the same
// numbers and the same boundaries. testdata/pipeline-yaml-budget/cases.json is
// shared with the Rust suite (libs/rust/agent-runtime/src/bounded_yaml_tests.rs),
// which proves the Worker reaches the same verdict on every case.

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/pipelinelimits"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

const repoRoot = "../../../../.."

type budgetCase struct {
	Name    string `json:"name"`
	Verdict string `json:"verdict"`
	Limit   string `json:"limit"`
	YAML    string `json:"yaml"`
}

type budgetCases struct {
	Budget struct {
		Nodes       int `json:"nodes"`
		ScalarBytes int `json:"scalar_bytes"`
		Depth       int `json:"depth"`
	} `json:"budget"`
	Cases []budgetCase `json:"cases"`
}

func readBudgetCases(t *testing.T) budgetCases {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot, "testdata", "pipeline-yaml-budget", "cases.json"))
	if err != nil {
		t.Fatalf("read shared cases: %v", err)
	}
	var cases budgetCases
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatalf("parse shared cases: %v", err)
	}
	if len(cases.Cases) == 0 {
		t.Fatal("the shared case file holds no case")
	}
	return cases
}

// The Go constants, the shared fixture and the Rust constant name one budget.
func TestExpansionBudgetMatchesTheWorker(t *testing.T) {
	source, err := os.ReadFile(filepath.Join(repoRoot, "libs", "rust", "agent-runtime", "src", "graph", "mod.rs"))
	if err != nil {
		t.Fatalf("read the Worker budget: %v", err)
	}
	block := regexp.MustCompile(`(?s)pub const PIPELINE_YAML_BUDGET: YamlBudget = YamlBudget \{(.*?)\};`).FindSubmatch(source)
	if block == nil {
		t.Fatal("PIPELINE_YAML_BUDGET not found in libs/rust/agent-runtime/src/graph/mod.rs")
	}
	body := strings.Join(strings.Fields(string(block[1])), " ")
	if want := "nodes: 131_072, scalar_bytes: 1024 * 1024, depth: 64,"; body != want {
		t.Fatalf("the Worker budget is %q; update expansion.go, generate.py and this test together (want %q)", body, want)
	}
	if pipelinelimits.MaxExpandedNodes != 131_072 || pipelinelimits.MaxExpandedScalarBytes != 1024*1024 ||
		pipelinelimits.MaxExpandedDepth != 64 {
		t.Fatal("expansion.go drifted from the Worker budget")
	}
	cases := readBudgetCases(t)
	if cases.Budget.Nodes != pipelinelimits.MaxExpandedNodes || cases.Budget.ScalarBytes != pipelinelimits.MaxExpandedScalarBytes ||
		cases.Budget.Depth != pipelinelimits.MaxExpandedDepth {
		t.Fatalf("cases.json budget %+v drifted from expansion.go", cases.Budget)
	}
}

// Every shared case gets the verdict the Worker gives it: at each limit the
// definition saves, one past it the save is refused with the typed 400.
func TestExpansionBudgetSharedCases(t *testing.T) {
	for _, tc := range readBudgetCases(t).Cases {
		t.Run(tc.Name, func(t *testing.T) {
			err := pipelinelimits.Check(tc.YAML)
			switch tc.Verdict {
			case "accept":
				if err != nil {
					t.Fatalf("Check refused a definition the Worker accepts: %v", err)
				}
			case "refuse":
				if !errors.Is(err, pipelinelimits.ErrExpansionTooLarge) {
					t.Fatalf("Check = %v, want ErrExpansionTooLarge", err)
				}
				var api *apierr.APIError
				if !errors.As(err, &api) || api.Status != http.StatusBadRequest ||
					api.Code != "PIPELINE_YAML_EXPANSION_TOO_LARGE" || pipelinelimits.Refusal(err) == nil {
					t.Fatalf("refusal %#v is not the typed 400", err)
				}
			default:
				t.Fatalf("bad verdict %q", tc.Verdict)
			}
		})
	}
}

// expanded is a plain, uncapped count of the expansion over the parsed tree,
// used only to show the fixture's boundary cases sit exactly on their limit.
// (yaml.v3 refuses to decode them into Go values: its own aliasing guard is
// stricter than the Worker's budget.) The independent check of the verdicts
// is the Rust suite, which runs the Worker's parser over the same file.
type expanded struct{ nodes, scalarBytes, depth int }

func (e *expanded) count(node *yaml.Node, depth int) {
	switch node.Kind {
	case yaml.AliasNode:
		e.count(node.Alias, depth)
	case yaml.SequenceNode, yaml.MappingNode:
		e.nodes++
		e.depth = max(e.depth, depth+1)
		for _, child := range node.Content {
			e.count(child, depth+1)
		}
	case yaml.ScalarNode:
		e.nodes++
		if node.ShortTag() == "!!str" {
			e.scalarBytes += len(node.Value)
		}
	}
}

func TestExpansionBoundaryCasesSitExactlyOnTheLimit(t *testing.T) {
	want := map[string]func(expanded) bool{
		"nodes at the limit":        func(e expanded) bool { return e.nodes == pipelinelimits.MaxExpandedNodes },
		"scalar bytes at the limit": func(e expanded) bool { return e.scalarBytes == pipelinelimits.MaxExpandedScalarBytes },
		"depth at the limit":        func(e expanded) bool { return e.depth == pipelinelimits.MaxExpandedDepth },
	}
	seen := 0
	for _, tc := range readBudgetCases(t).Cases {
		check, ok := want[tc.Name]
		if !ok {
			continue
		}
		seen++
		var document yaml.Node
		if err := yaml.Unmarshal([]byte(tc.YAML), &document); err != nil {
			t.Fatalf("%s: %v", tc.Name, err)
		}
		var e expanded
		e.count(document.Content[0], 0)
		if !check(e) {
			t.Errorf("%s expands to %+v, which is not exactly on its limit", tc.Name, e)
		}
	}
	if seen != len(want) {
		t.Fatalf("found %d of %d boundary cases in cases.json", seen, len(want))
	}
}

// The audit's shape (about 3x10^9 nodes from a document inside the byte
// bound) is refused through Check, the save-time entry point. That the refusal
// costs at most the budget in visits, never the expansion, is asserted by
// counting the visits in TestExpansionMeterStopsAtTheBudget
// (expansion_internal_test.go). The wall-clock bound this test used to carry
// measured the host, not the meter: under race instrumentation on a loaded
// machine the 330 KB parse alone took over two seconds.
func TestExpansionRefusesALargeBombWithinTheBudget(t *testing.T) {
	doc := expansionBomb()
	if len(doc) > pipelinelimits.MaxInstructionsBytes {
		t.Fatalf("bomb is %d bytes, over the byte bound; the test would not reach the expansion check", len(doc))
	}
	if err := pipelinelimits.Check(doc); !errors.Is(err, pipelinelimits.ErrExpansionTooLarge) {
		t.Fatalf("Check = %v, want ErrExpansionTooLarge", err)
	}
}

// expansionBomb is 50,001 scalars replayed by 60,001 aliases: about 3x10^9
// nodes once expanded, from roughly 330 KB of text.
func expansionBomb() string {
	return "a: &x [" + strings.Repeat("0,", 50_000) + "0]\nb: [" + strings.Repeat("*x,", 60_000) + "*x]\n"
}

func BenchmarkCheckRefusesBomb(b *testing.B) {
	doc := expansionBomb()
	b.ReportAllocs()
	for b.Loop() {
		if !errors.Is(pipelinelimits.Check(doc), pipelinelimits.ErrExpansionTooLarge) {
			b.Fatal("bomb accepted")
		}
	}
}
