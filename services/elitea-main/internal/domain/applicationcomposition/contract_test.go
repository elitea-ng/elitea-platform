package applicationcomposition_test

import (
	"encoding/json"
	"errors"
	"fmt"
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
		MaxCheckpointPaths     int    `json:"max_checkpoint_paths"`
		MaxVersionBytes        int    `json:"max_version_bytes"`
		PublicCode             string `json:"public_code"`
		RuntimeMessage         string `json:"runtime_message"`
		SizeRuntimeMessage     string `json:"size_runtime_message"`
	}
	if err := json.Unmarshal(raw, &contract); err != nil {
		t.Fatalf("decode the composition contract: %v", err)
	}
	if contract.MaxAgentTiers != applicationcomposition.MaxAgentTiers ||
		contract.MaxPipelineDepth != applicationcomposition.MaxPipelineDepth ||
		contract.MaxOrdinaryGraphScopes != applicationcomposition.MaxOrdinaryGraphScopes ||
		contract.MaxApplicationHops != applicationcomposition.MaxApplicationHops ||
		contract.MaxCheckpointPaths != applicationcomposition.MaxCheckpointPaths ||
		contract.MaxVersionBytes != applicationcomposition.MaxVersionBytes ||
		contract.PublicCode != applicationcomposition.Code ||
		contract.RuntimeMessage != applicationcomposition.RuntimeMessage ||
		contract.SizeRuntimeMessage != applicationcomposition.SizeRuntimeMessage {
		t.Fatalf("contract %+v drifted from the save check", contract)
	}
	// The output plane refuses a runtime failure whose safe message exceeds
	// 256 bytes (application/output RuntimeFailureFrame.Validate).
	for _, message := range []string{contract.RuntimeMessage, contract.SizeRuntimeMessage} {
		if len(message) > 256 {
			t.Fatalf("a registered message is %d bytes; the output plane admits 256", len(message))
		}
	}
}

// A refused start is the typed composition refusal: 422, the public code and
// the registered data-free message, through Refusal like a save refusal.
func TestARefusedStartIsTheTypedCompositionRefusal(t *testing.T) {
	refusal := applicationcomposition.Refusal(fmt.Errorf("start: %w", applicationcomposition.ErrAtStart))
	if refusal == nil || refusal.Status != 422 || refusal.Code != applicationcomposition.Code || refusal.Message != applicationcomposition.RuntimeMessage {
		t.Fatalf("refusal = %+v", refusal)
	}
	if applicationcomposition.Refusal(errors.New("other")) != nil {
		t.Fatal("an unrelated error is not a composition refusal")
	}
}
