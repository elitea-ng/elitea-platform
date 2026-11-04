package pipelinelimits_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/pipelinelimits"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// mapOver is a Map whose worker is a node of workerType; extra adds fields
// to the Map node.
func mapOver(workerType, extra string) string {
	return "entry_point: each\nstate:\n  items: list\n  mapped: list\n  out: str\nnodes:\n" +
		"  - id: each\n    type: map\n    worker: render\n    source: items\n    item: entity\n" +
		"    index: item_index\n    outputs: [out]\n    destination: mapped\n    max_items: 8\n" +
		"    max_concurrency: 2\n" + extra + "    transition: END\n" +
		"  - id: render\n    type: " + workerType + "\n    output: [out]\n"
}

func mapRefusal(t *testing.T, name string, err error) *apierr.APIError {
	t.Helper()
	var api *apierr.APIError
	if !errors.As(err, &api) || api.Code != pipelinelimits.CodeMapFormUnsupported {
		t.Fatalf("%s: got %v, want %s", name, err, pipelinelimits.CodeMapFormUnsupported)
	}
	if pipelinelimits.Refusal(err) != api {
		t.Errorf("%s: Refusal does not answer the Map form refusal", name)
	}
	if !strings.Contains(api.Message, "before saving") && !strings.Contains(api.Message, "Remove") {
		t.Errorf("%s: message %q has no remedy", name, api.Message)
	}
	return api
}

// A Map worker that is not a State Modifier, Agent or LLM node (Code and a
// direct Router wait for Wave 3) is refused at save and at start, naming the
// Map, its worker and the worker's type.
func TestCheckRefusesAMapWorkerOfAnUnsupportedForm(t *testing.T) {
	for _, workerType := range []string{"code", "router", "printer", "hitl", "parallel"} {
		doc := mapOver(workerType, "    reduction: ordered_collection\n")
		for name, err := range map[string]error{
			"save":  pipelinelimits.Check(doc),
			"start": pipelinelimits.CheckStart(doc, nil),
		} {
			api := mapRefusal(t, workerType+" "+name, err)
			for _, want := range []string{`Node "each"`, `node "render"`, `"` + workerType + `" node`} {
				if !strings.Contains(api.Message, want) {
					t.Errorf("%s %s: message %q does not contain %s", workerType, name, api.Message, want)
				}
			}
		}
	}
}

// Every worker family a Map item runs on the shared fan-out adapter is
// admitted, in the v1 and the v2 shape.
func TestCheckAdmitsTheMapWorkerFamilies(t *testing.T) {
	for _, workerType := range []string{"state_modifier", "agent", "llm"} {
		for _, extra := range []string{
			"    reduction: ordered_collection\n",
			"    on_error: continue\n    join: append\n",
		} {
			if err := pipelinelimits.Check(mapOver(workerType, extra)); err != nil {
				t.Fatalf("%s worker, %q: %v", workerType, extra, err)
			}
		}
	}
}

// M2: `reduction` and `join` together are refused readably at save, with the
// fix (the Worker refuses the same document).
func TestCheckRefusesAMapWithBothReductionAndJoin(t *testing.T) {
	doc := mapOver("state_modifier", "    reduction: ordered_collection\n    join: append\n")
	api := mapRefusal(t, "save", pipelinelimits.Check(doc))
	for _, want := range []string{`Node "each"`, "`reduction`", "`join: append`"} {
		if !strings.Contains(api.Message, want) {
			t.Errorf("message %q does not contain %s", api.Message, want)
		}
	}
}
