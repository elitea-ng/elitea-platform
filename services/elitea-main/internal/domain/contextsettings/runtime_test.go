package contextsettings_test

import (
	"encoding/json"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/contextsettings"
)

func TestRuntimePresetsIgnoreLegacyNumbersAndPreserveLayerPrecedence(t *testing.T) {
	full := contextsettings.BudgetFull
	legacyLimit := 64000
	for _, tc := range []struct {
		name   string
		stored string
		user   *contextsettings.ContextManagement
		mode   contextsettings.BudgetMode
		limit  int
	}{
		{"new account", "", nil, contextsettings.BudgetBalanced, 0},
		{"stored legacy", `{"max_context_tokens":64000}`, nil, contextsettings.BudgetBalanced, 64000},
		{"user legacy", "", &contextsettings.ContextManagement{MaxContextTokens: &legacyLimit}, contextsettings.BudgetBalanced, 64000},
		{"user full", "", &contextsettings.ContextManagement{BudgetMode: &full}, full, 0},
		{"conversation override", `{"max_context_tokens":12000}`, &contextsettings.ContextManagement{BudgetMode: &full}, full, 12000},
		{"conversation preset", `{"budget_mode":"balanced"}`, &contextsettings.ContextManagement{MaxContextTokens: &legacyLimit}, contextsettings.BudgetBalanced, 64000},
		{"preset clears previous value", `{"budget_mode":"full","max_context_tokens":64000}`, nil, full, 64000},
		{"partial conversation inherits", `{"preserve_recent_messages":8}`, &contextsettings.ContextManagement{BudgetMode: &full}, full, 0},
		{"malformed has no partial effect", `{"enabled":false,"preserve_recent_messages":"bad"}`, nil, contextsettings.BudgetBalanced, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resolved := contextsettings.Resolve([]byte(tc.stored), contextsettings.UserDefaults{ContextManagement: tc.user})
			if resolved.BudgetMode != tc.mode || resolved.MaxContextTokens != tc.limit || !resolved.Enabled {
				t.Fatalf("unexpected resolved policy: %+v", resolved)
			}
			settings, _, err := resolved.Runtime()
			if err != nil {
				t.Fatal(err)
			}
			wire, encodeErr := json.Marshal(settings)
			if encodeErr != nil {
				t.Fatal(encodeErr)
			}
			var fields map[string]any
			if err := json.Unmarshal(wire, &fields); err != nil {
				t.Fatal(err)
			}
			if fields["max_context_tokens"] != nil || fields["budget_mode"] != string(tc.mode) {
				t.Fatalf("legacy number became a combined limit: %s", wire)
			}

			for _, forbidden := range []string{"name", "summary_llm_settings", "model_context_limits", "summary_model"} {
				if _, found := fields[forbidden]; found {
					t.Fatalf("runtime settings contain %s", forbidden)
				}
			}
			stored, _ := json.Marshal(resolved)
			if roundtrip := contextsettings.Resolve(stored, contextsettings.UserDefaults{}); roundtrip.BudgetMode != tc.mode || roundtrip.MaxContextTokens != tc.limit {
				t.Fatalf("stored policy changed: %+v", roundtrip)
			}
		})
	}
}

func TestRuntimePresetUpdatesAndSummarySelection(t *testing.T) {
	strategy := contextsettings.Resolve([]byte(`{"max_context_tokens":64000,"summary_llm_settings":{"model_name":"summary","model_project_id":7,"max_tokens":4000}}`), contextsettings.UserDefaults{})
	for _, body := range []string{`{"budget_mode":"full"}`, `{"preserve_recent_messages":9}`, `{"budget_mode":"balanced"}`} {
		update, err := contextsettings.DecodeStrategyUpdate([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		strategy, err = strategy.Apply(update)
		if err != nil || strategy.MaxContextTokens != 0 {
			t.Fatalf("preset update failed: %+v, %v", strategy, err)
		}
	}
	_, selection, err := strategy.Runtime()
	if err != nil || selection["model_name"] != "summary" {
		t.Fatalf("summary selection lost: %v, %v", selection, err)
	}
	selection["model_name"] = "changed"
	if strategy.SummaryLLMSettings["model_name"] != "summary" {
		t.Fatal("runtime selection aliases stored settings")
	}
	update, _ := contextsettings.DecodeStrategyUpdate([]byte(`{"max_context_tokens":32000}`))
	strategy, err = strategy.Apply(update)
	if err != nil || strategy.BudgetMode != contextsettings.BudgetBalanced || strategy.MaxContextTokens != 32000 {
		t.Fatalf("legacy numeric update changed preset: %+v, %v", strategy, err)
	}
	strategy.Enabled = false
	if settings, selection, err := strategy.Runtime(); err != nil || settings.Enabled || selection != nil {
		t.Fatalf("disabled policy requests a summary binding: %+v, %v, %v", settings, selection, err)
	}
	for _, body := range []string{`{"budget_mode":"invented"}`, `{"budget_mode":42}`, `{"max_context_tokens":-1}`} {
		update, err := contextsettings.DecodeStrategyUpdate([]byte(body))
		if err == nil {
			_, err = strategy.Apply(update)
		}
		if err == nil {
			t.Fatalf("accepted invalid policy: %s", body)
		}
	}
}

func TestPresetStatusDoesNotInventUsableModelCapacity(t *testing.T) {
	for _, raw := range [][]byte{nil, []byte(`{"current_context_tokens":2500,"messages_in_context":4}`)} {
		status := contextsettings.BuildStatus(contextsettings.DefaultStrategy(), raw, 4)
		if status.MaxTokens != 0 || status.BudgetMode != contextsettings.BudgetBalanced || len(status.Unavailable) == 0 {
			t.Fatalf("preset status invents capacity: %+v", status)
		}
	}
}

func TestDefaultSummaryUsesTaskModelWhenNoModelIsSelected(t *testing.T) {
	empty, owner, output := "", 2, 4096
	strategy := contextsettings.Resolve(nil, contextsettings.UserDefaults{
		Summarization: &contextsettings.Summarization{
			SummaryModelName: &empty, SummaryModelProjectID: &owner, TargetSummaryTokens: &output,
		},
	})
	_, selection, err := strategy.Runtime()
	if err != nil || len(selection) != 1 || selection["max_tokens"] != output {
		t.Fatalf("empty model selection carried a project identity: %v, %v", selection, err)
	}
}
