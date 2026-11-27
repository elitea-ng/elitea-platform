package eliteacore

import (
	"fmt"
	"testing"
)

// Issue 3137 review: model_project_id is checked, not skipped, and an agent
// file carries a bounded number of evaluation datasets.

func TestValidateImportedLLMSettingsModelProjectID(t *testing.T) {
	cases := []struct {
		name  string
		value any
		ok    bool
	}{
		{"string", "7", true},
		{"integer", float64(7), true},
		{"fraction", 7.5, false},
		{"object", map[string]any{"a": float64(1)}, false},
		{"array", []any{"7"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			failure := validateImportedLLMSettings("llm_settings", map[string]any{"model_project_id": tc.value})
			if tc.ok && failure != nil {
				t.Fatalf("refused %v: %v", tc.value, failure)
			}
			if !tc.ok && (failure == nil || failure.field != "llm_settings.model_project_id") {
				t.Fatalf("accepted %v (failure %v)", tc.value, failure)
			}
		})
	}
}

func TestValidateImportedEvaluationBoundsDatasets(t *testing.T) {
	datasets := make([]any, 0, maxImportedDatasets+1)
	for i := 0; i <= maxImportedDatasets; i++ {
		datasets = append(datasets, map[string]any{
			"name":  fmt.Sprintf("set %d", i),
			"cases": []any{map[string]any{"input": "q", "expected_output": "a"}},
		})
	}
	failure := validateImportedEvaluation("evaluation", map[string]any{"datasets": datasets})
	if failure == nil || failure.field != "evaluation.datasets" {
		t.Fatalf("want a refusal of evaluation.datasets, got %v", failure)
	}
	if failure := validateImportedEvaluation("evaluation", map[string]any{"datasets": datasets[:maxImportedDatasets]}); failure != nil {
		t.Fatalf("refused %d datasets: %v", maxImportedDatasets, failure)
	}
}
