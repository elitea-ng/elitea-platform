package contextsettings_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/contextsettings"
)

// The frozen contract's constants, asserted as VALUES rather than by
// referencing the constants they come from — a test that reads the same
// constant it checks cannot notice the constant changing.
func TestDefaultStrategyIsTheFrozenContract(t *testing.T) {
	encoded, err := json.Marshal(contextsettings.DefaultStrategy())
	if err != nil {
		t.Fatalf("encode default strategy: %v", err)
	}
	const want = `{"name":"default","enabled":true,"enable_summarization":true,` +
		`"enable_context_editing":false,"max_context_tokens":0,"budget_mode":"balanced",` +
		`"preserve_recent_messages":5,"preserve_system_messages":true,` +
		`"summary_instructions":"Generate a concise summary of the following conversation messages",` +
		`"summary_llm_settings":null}`
	if string(encoded) != want {
		t.Fatalf("default strategy is\n\t%s\nwant\n\t%s", encoded, want)
	}
}

// Resolution, outermost tier first.
func TestResolveOrdersTheThreeTiers(t *testing.T) {
	userMax := 20000
	userPreserve := 9
	defaults := contextsettings.UserDefaults{
		ContextManagement: &contextsettings.ContextManagement{
			MaxContextTokens:       &userMax,
			PreserveRecentMessages: &userPreserve,
		},
	}

	// Nothing stored: the user's answer stands, and untouched fields fall to
	// the constants.
	resolved := contextsettings.Resolve(nil, defaults)
	if resolved.MaxContextTokens != 20000 {
		t.Errorf("max_context_tokens = %d, want the user default 20000", resolved.MaxContextTokens)
	}
	if !resolved.Enabled || resolved.SummaryInstructions != contextsettings.DefaultSummaryInstructions {
		t.Errorf("untouched fields did not fall back to the constants: %+v", resolved)
	}

	// A stored key beats the user default; a key the stored document does not
	// carry keeps the user's answer rather than snapping to a constant.
	resolved = contextsettings.Resolve([]byte(`{"max_context_tokens": 3000}`), defaults)
	if resolved.MaxContextTokens != 3000 {
		t.Errorf("max_context_tokens = %d, want the conversation's 3000", resolved.MaxContextTokens)
	}
	if resolved.PreserveRecentMessages != 9 {
		t.Errorf("preserve_recent_messages = %d, want the user default 9 — a key the stored "+
			"strategy does not carry must not fall past the user tier", resolved.PreserveRecentMessages)
	}
}

// A false stored by the user is an answer, not an absence. A non-pointer
// defaults struct would make the two indistinguishable.
func TestResolveHonoursAStoredFalse(t *testing.T) {
	off := false
	resolved := contextsettings.Resolve(nil, contextsettings.UserDefaults{
		ContextManagement: &contextsettings.ContextManagement{Enabled: &off},
	})
	if resolved.Enabled {
		t.Fatal("a user who turned context management off resolved back to enabled")
	}
}

// pylon's set_context_strategy flattens three summarization defaults into
// summary_llm_settings.
func TestResolveFlattensTheSummarizationDefaults(t *testing.T) {
	name := "gpt-4o-mini"
	projectID := 3
	target := 1024
	resolved := contextsettings.Resolve(nil, contextsettings.UserDefaults{
		Summarization: &contextsettings.Summarization{
			SummaryModelName:      &name,
			SummaryModelProjectID: &projectID,
			TargetSummaryTokens:   &target,
		},
	})
	settings := resolved.SummaryLLMSettings
	if settings["model_name"] != name || settings["model_project_id"] != projectID ||
		settings["max_tokens"] != target {
		t.Fatalf("summary_llm_settings = %v, want the three flattened defaults", settings)
	}
}

// A malformed stored document must not take the read down; the resolved
// defaults stand.
func TestResolveSurvivesAMalformedStoredStrategy(t *testing.T) {
	resolved := contextsettings.Resolve([]byte(`{"max_context_tokens":`), contextsettings.UserDefaults{})
	if resolved.MaxContextTokens != contextsettings.DefaultMaxContextTokens {
		t.Fatalf("max_context_tokens = %d, want the default after a malformed document",
			resolved.MaxContextTokens)
	}
}

func TestApplyValidatesTheMergedValue(t *testing.T) {
	// Legacy context numbers do not constrain the independent summary output.
	stored := contextsettings.Resolve(
		[]byte(`{"summary_llm_settings": {"max_tokens": 4000}}`), contextsettings.UserDefaults{})
	smaller := 3000
	if _, fieldErr := stored.Apply(contextsettings.StrategyUpdate{MaxContextTokens: &smaller}); fieldErr != nil {
		t.Fatalf("legacy context number constrained summary output: %v", fieldErr)
	}

	larger := 40000
	merged, fieldErr := stored.Apply(contextsettings.StrategyUpdate{MaxContextTokens: &larger})
	if fieldErr != nil {
		t.Fatalf("a budget above the summary budget was refused: %v", fieldErr)
	}
	if merged.SummaryLLMSettings["max_tokens"] != float64(4000) {
		t.Fatalf("summary_llm_settings was dropped by an update that never mentioned it: %v",
			merged.SummaryLLMSettings)
	}
}

// "No summary model" must serialize as `null`, NEVER as `{}`.
//
// The Rust runtime reads a non-null summary_llm_settings as "use this second
// model", which needs a credential the execution claim does not carry, and
// refuses it with UnsupportedCapability instead of falling back to the main
// model. `{}` is an object, so an empty-but-non-nil map here would fail every
// context-managed turn — for a value that says nothing. An empty map is
// exactly what `encoding/json` hands back from `"summary_llm_settings": {}` in
// a request body, which is why this is asserted on the SERIALIZED bytes of
// every path that can produce one.
func TestSummaryLLMSettingsSerializeAsNullWhenEmpty(t *testing.T) {
	cases := map[string]contextsettings.Strategy{
		"the default": contextsettings.DefaultStrategy(),
		"no summarization fields in the user defaults": contextsettings.Resolve(nil, contextsettings.UserDefaults{
			Summarization: &contextsettings.Summarization{},
		}),
		"a stored strategy that carries an empty object": contextsettings.Resolve(
			[]byte(`{"summary_llm_settings": {}}`), contextsettings.UserDefaults{}),
	}

	empty := map[string]any{}
	for name, strategy := range cases {
		t.Run(name, func(t *testing.T) {
			assertSummaryLLMSettingsNull(t, strategy)
		})
	}

	t.Run("an update that submits an empty object", func(t *testing.T) {
		merged, fieldErr := contextsettings.DefaultStrategy().Apply(
			contextsettings.StrategyUpdate{SummaryLLMSettings: empty})
		if fieldErr != nil {
			t.Fatalf("an empty summary_llm_settings was refused: %v", fieldErr)
		}
		assertSummaryLLMSettingsNull(t, merged)
	})

	t.Run("an update that clears a stored summary model", func(t *testing.T) {
		stored := contextsettings.Resolve(
			[]byte(`{"summary_llm_settings": {"model_name": "gpt-4o-mini"}}`), contextsettings.UserDefaults{})
		merged, fieldErr := stored.Apply(contextsettings.StrategyUpdate{SummaryLLMSettings: empty})
		if fieldErr != nil {
			t.Fatalf("clearing the summary model was refused: %v", fieldErr)
		}
		assertSummaryLLMSettingsNull(t, merged)
	})
}

func assertSummaryLLMSettingsNull(t *testing.T, strategy contextsettings.Strategy) {
	t.Helper()
	if strategy.SummaryLLMSettings != nil {
		t.Errorf("SummaryLLMSettings is %#v, want nil", strategy.SummaryLLMSettings)
	}
	encoded, err := json.Marshal(strategy)
	if err != nil {
		t.Fatalf("encode strategy: %v", err)
	}
	if !strings.Contains(string(encoded), `"summary_llm_settings":null`) {
		t.Fatalf("serialized strategy does not carry a null summary_llm_settings: %s", encoded)
	}
	if strings.Contains(string(encoded), `"summary_llm_settings":{}`) {
		t.Fatalf("serialized strategy carries an EMPTY OBJECT summary_llm_settings, "+
			"which the runtime refuses with UnsupportedCapability: %s", encoded)
	}
}

func TestDecodeStrategyUpdateNamesTheWrongTypedField(t *testing.T) {
	_, fieldErr := contextsettings.DecodeStrategyUpdate([]byte(`{"preserve_recent_messages": "five"}`))
	if fieldErr == nil {
		t.Fatal("a string where the contract says integer was accepted")
	}
	if fieldErr.Field != "preserve_recent_messages" {
		t.Fatalf("field = %q, want preserve_recent_messages", fieldErr.Field)
	}
}

func TestUserDefaultsRangesAreEnforcedOnPresentFieldsOnly(t *testing.T) {
	// Absent means "no opinion" and cannot be out of range.
	if _, fieldErr := contextsettings.DecodeContextManagement([]byte(`{}`)); fieldErr != nil {
		t.Fatalf("an empty block was refused: %v", fieldErr)
	}
	if block, fieldErr := contextsettings.DecodeContextManagement(nil); fieldErr != nil || block != nil {
		t.Fatalf("absent decoded to (%v, %v), want (nil, nil)", block, fieldErr)
	}

	for _, testCase := range []struct{ body, field string }{
		{`{"max_context_tokens": 999}`, "default_context_management.max_context_tokens"},
		{`{"preserve_recent_messages": 100}`, "default_context_management.preserve_recent_messages"},
	} {
		_, fieldErr := contextsettings.DecodeContextManagement([]byte(testCase.body))
		if fieldErr == nil {
			t.Errorf("%s was accepted", testCase.body)
			continue
		}
		if fieldErr.Field != testCase.field {
			t.Errorf("field = %q, want %q", fieldErr.Field, testCase.field)
		}
	}
}

// A cleared form field is an absence, not a type error: the Memory page holds
// a numeric input mid-edit as the empty string.
func TestUserDefaultsTreatAClearedFieldAsAbsent(t *testing.T) {
	block, fieldErr := contextsettings.DecodeContextManagement(
		[]byte(`{"max_context_tokens": "", "preserve_recent_messages": null, "enabled": true}`))
	if fieldErr != nil {
		t.Fatalf("a cleared field was refused: %v", fieldErr)
	}
	if block == nil || block.Enabled == nil || !*block.Enabled {
		t.Fatalf("the fields that WERE set did not survive: %+v", block)
	}
	if block.MaxContextTokens != nil || block.PreserveRecentMessages != nil {
		t.Fatalf("a cleared field decoded to a value: %+v", block)
	}
}

// The status document's refusal path: no runtime record means no invented
// numbers, and the reason is named.
func TestBuildStatusRefusesWhatItCannotCompute(t *testing.T) {
	status := contextsettings.BuildStatus(contextsettings.DefaultStrategy(), nil, 12)
	if status.ContextAnalyticsAvailable {
		t.Fatal("an absent runtime record reported itself as available")
	}
	if status.CurrentTokens != 0 || status.MessageGroupsInContext != 0 || status.SummaryCount != 0 {
		t.Errorf("counters were invented: %+v", status)
	}
	if status.UnavailableReason == "" || len(status.Unavailable) == 0 {
		t.Error("the refusal has to name what is missing and why")
	}
	if status.MessageGroupsTotal != 12 {
		t.Errorf("message_groups_total = %d, want the real count 12", status.MessageGroupsTotal)
	}
	if status.MaxTokens != contextsettings.DefaultMaxContextTokens {
		t.Errorf("max_tokens = %d, want the resolved budget", status.MaxTokens)
	}
}

func TestBuildStatusReportsARatioNotAPercentage(t *testing.T) {
	strategy := contextsettings.DefaultStrategy()
	strategy.MaxContextTokens = 10000
	strategy.BudgetMode = ""
	status := contextsettings.BuildStatus(strategy,
		[]byte(`{"current_context_tokens": 2500, "messages_in_context": 4, "summaries_generated": 1}`), 4)

	if !status.ContextAnalyticsAvailable {
		t.Fatal("a present runtime record reported itself as unavailable")
	}
	if status.Utilization != 0.25 {
		t.Errorf("utilization = %v, want 0.25 (pylon's 0..1 ratio)", status.Utilization)
	}
	if status.CurrentTokens != 2500 || status.MessageGroupsInContext != 4 || status.SummaryCount != 1 {
		t.Errorf("recorded counters were not served: %+v", status)
	}
}
