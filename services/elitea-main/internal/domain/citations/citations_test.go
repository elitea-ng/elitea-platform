package citations

import (
	"errors"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/outputcontract"
)

func TestCheckAcceptsOffRequiredAndNull(t *testing.T) {
	for name, meta := range map[string]map[string]any{
		"nil meta": nil,
		"absent":   {"step_limit": 5},
		"null":     {"citations": nil},
		"off":      {"citations": "off"},
		"required": {"citations": "required"},
	} {
		if err := Check(meta); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestCheckRefusesOtherValues(t *testing.T) {
	for _, value := range []any{"Required", "always", "", true, 1, map[string]any{}} {
		if err := Check(map[string]any{"citations": value}); !errors.Is(err, ErrInvalid) {
			t.Errorf("%v: got %v, want ErrInvalid", value, err)
		}
	}
}

func TestCheckVersionChecksEveryContentControl(t *testing.T) {
	var refusal *outputcontract.Error
	if err := CheckVersion("react", map[string]any{"citations": "always"}, ""); !errors.Is(err, ErrInvalid) {
		t.Errorf("citations: got %v", err)
	}
	bad := map[string]any{"output_contract": map[string]any{"type": "nope"}}
	if err := CheckVersion("react", bad, ""); !errors.As(err, &refusal) {
		t.Errorf("meta contract: got %v", err)
	}
	onPipeline := map[string]any{"output_contract": map[string]any{"type": "format", "format": "json"}}
	if err := CheckVersion("pipeline", onPipeline, ""); !errors.As(err, &refusal) || refusal.Reason != outputcontract.ReasonPipelineContractOnNode {
		t.Errorf("pipeline meta contract: got %v", err)
	}
	yaml := "nodes:\n  - id: answer\n    type: llm\n    output_contract:\n      type: format\n      format: xml\n"
	if err := CheckVersion("pipeline", nil, yaml); !errors.As(err, &refusal) || refusal.NodeID != "answer" {
		t.Errorf("node contract: got %v", err)
	}
	good := map[string]any{"citations": "required", "output_contract": map[string]any{"type": "format", "format": "json"}}
	if err := CheckVersion("react", good, "Answer briefly."); err != nil {
		t.Errorf("valid version: %v", err)
	}
}
