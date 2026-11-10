package pipelinelimits_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/pipelinelimits"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// familiesContract is libs/proto/contracts/pipeline-fanout-child-families-v1.json:
// the node types each fan-out family runs as a direct child. The Worker
// compiler and the Web editor assert the same file.
type familiesContract struct {
	ParallelBranch     []string `json:"parallel_branch"`
	MapWorker          []string `json:"map_worker"`
	RefusedWithOwnText []string `json:"refused_with_own_text"`
}

func loadFamiliesContract(t *testing.T) familiesContract {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the test file")
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(file), "../../../../../libs/proto/contracts/pipeline-fanout-child-families-v1.json"))
	if err != nil {
		t.Fatalf("read the shared families contract: %v", err)
	}
	var contract familiesContract
	if err := json.Unmarshal(raw, &contract); err != nil {
		t.Fatalf("decode the shared families contract: %v", err)
	}
	return contract
}

// N2: Main admits exactly the contract's families, and refuses a direct
// Router with its own text.
func TestFanoutChildFamiliesMatchTheSharedContract(t *testing.T) {
	contract := loadFamiliesContract(t)
	if got := pipelinelimits.ParallelBranchTypes(); !reflect.DeepEqual(got, contract.ParallelBranch) {
		t.Errorf("Parallel branch types = %v, contract = %v", got, contract.ParallelBranch)
	}
	if got := pipelinelimits.MapWorkerTypes(); !reflect.DeepEqual(got, contract.MapWorker) {
		t.Errorf("Map worker types = %v, contract = %v", got, contract.MapWorker)
	}
	if !reflect.DeepEqual(contract.RefusedWithOwnText, []string{"router"}) {
		t.Errorf("refused with own text = %v", contract.RefusedWithOwnText)
	}
}

// W3-2: a Code node is admitted as a Map worker and as a Parallel branch, at
// save and at start.
func TestCheckAdmitsACodeMapWorkerAndACodeParallelBranch(t *testing.T) {
	for name, doc := range map[string]string{
		"map v1":   mapOver("code", "    reduction: ordered_collection\n"),
		"map v2":   mapOver("code", "    on_error: continue\n    join: append\n"),
		"parallel": parallelOver("code"),
	} {
		if err := pipelinelimits.Check(doc); err != nil {
			t.Errorf("%s save: %v", name, err)
		}
		if err := pipelinelimits.CheckStart(doc, []string{"Research"}); err != nil {
			t.Errorf("%s start: %v", name, err)
		}
	}
}

// Q7: a direct Router child stays refused, with the same code as before and
// its own readable text, naming the fan-out node and the Router.
func TestCheckRefusesADirectRouterChildWithItsOwnText(t *testing.T) {
	for name, tc := range map[string]struct {
		doc  string
		code string
		node string
	}{
		"map worker":      {mapOver("router", "    reduction: ordered_collection\n"), pipelinelimits.CodeMapFormUnsupported, `Node "each"`},
		"parallel branch": {parallelOver("router"), pipelinelimits.CodeParallelBranchUnsupported, `Node "gather"`},
	} {
		for phase, err := range map[string]error{
			"save":  pipelinelimits.Check(tc.doc),
			"start": pipelinelimits.CheckStart(tc.doc, []string{"Research"}),
		} {
			var api *apierr.APIError
			if !errors.As(err, &api) || api.Code != tc.code {
				t.Fatalf("%s %s: got %v, want %s", name, phase, err, tc.code)
			}
			for _, want := range []string{tc.node, `"router" node`, "a Router runs inside a child pipeline, not as a Parallel branch or Map worker"} {
				if !strings.Contains(api.Message, want) {
					t.Errorf("%s %s: message %q does not contain %q", name, phase, api.Message, want)
				}
			}
		}
	}
}
