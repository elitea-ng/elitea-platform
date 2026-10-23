package applicationcomposition_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/applicationcomposition"
)

// TestBoundsMatchTheSharedContract pins the save check to the Worker's bounds:
// libs/proto/contracts/application-composition-limits-v1.json, which the Worker
// asserts too (materialization_tests.rs composition_bounds_match_the_shared_contract).
func TestBoundsMatchTheSharedContract(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the test file")
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(file), "../../../../../libs/proto/contracts/application-composition-limits-v1.json"))
	if err != nil {
		t.Fatalf("read the composition contract: %v", err)
	}
	var contract struct {
		MaxAgentTiers          int    `json:"max_agent_tiers"`
		MaxPipelineDepth       int    `json:"max_pipeline_depth"`
		MaxOrdinaryGraphScopes int    `json:"max_ordinary_graph_scopes"`
		MaxApplicationHops     int    `json:"max_application_hops"`
		PublicCode             string `json:"public_code"`
	}
	if err := json.Unmarshal(raw, &contract); err != nil {
		t.Fatalf("decode the composition contract: %v", err)
	}
	if contract.MaxAgentTiers != applicationcomposition.MaxAgentTiers ||
		contract.MaxPipelineDepth != applicationcomposition.MaxPipelineDepth ||
		contract.MaxOrdinaryGraphScopes != applicationcomposition.MaxOrdinaryGraphScopes ||
		contract.MaxApplicationHops != applicationcomposition.MaxApplicationHops ||
		contract.PublicCode != applicationcomposition.Code {
		t.Fatalf("contract %+v drifted from the save check", contract)
	}
}
