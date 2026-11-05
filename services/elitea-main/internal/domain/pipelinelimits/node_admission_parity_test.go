package pipelinelimits_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/pipelinelimits"
)

// admissionContract is libs/proto/contracts/pipeline-node-admission-v1.json, the
// node types each layer admits. The Worker compiler and the Web editor assert the
// same file, so a node type added to one layer only fails the others' tests.
type admissionContract struct {
	Admitted []string `json:"admitted"`
	Refused  []string `json:"refused"`
}

func loadAdmissionContract(t *testing.T) admissionContract {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the test file")
	}
	path := filepath.Join(filepath.Dir(file), "../../../../../libs/proto/contracts/pipeline-node-admission-v1.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the shared admission contract: %v", err)
	}
	var contract admissionContract
	if err := json.Unmarshal(raw, &contract); err != nil {
		t.Fatalf("decode the shared admission contract: %v", err)
	}
	return contract
}

func TestSaveAdmissionMatchesTheSharedNodeTypeContract(t *testing.T) {
	contract := loadAdmissionContract(t)
	want := append([]string{}, contract.Admitted...)
	sort.Strings(want)
	if got := pipelinelimits.AdmittedNodeTypes(); !reflect.DeepEqual(got, want) {
		t.Fatalf("admitted node types = %v, contract admitted = %v", got, want)
	}
	for _, nodeType := range want {
		if err := pipelinelimits.Check(pipelineWithNode("n", nodeType)); err != nil {
			t.Errorf("type %s: Check refused a contract type: %v", nodeType, err)
		}
	}
	for _, nodeType := range contract.Refused {
		api := refusal(t, pipelineWithNode("n", nodeType))
		if api.Code != pipelinelimits.CodeNodeTypeUnsupported {
			t.Errorf("type %s: code = %s, want %s", nodeType, api.Code, pipelinelimits.CodeNodeTypeUnsupported)
		}
	}
}
