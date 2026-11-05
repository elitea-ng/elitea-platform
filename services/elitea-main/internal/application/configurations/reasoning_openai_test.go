package configurations

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The Go translation must match the Rust table: both read the fixture both
// Rust callers assert (conformance/llm-caller/contract.json, "thinking").
type thinkingFixture struct {
	Thinking struct {
		Settings        map[string]string `json:"settings"`
		RefusedSettings []string          `json:"refused_settings"`
		Conflicts       []struct {
			Control string `json:"control"`
			Setting string `json:"setting"`
		} `json:"conflicts"`
		OpenAICompatible []struct {
			Control         string  `json:"control"`
			Step            string  `json:"step"`
			ReasoningEffort *string `json:"reasoning_effort"`
			EnableThinking  *bool   `json:"enable_thinking"`
		} `json:"openai_compatible"`
	} `json:"thinking"`
}

func readThinkingFixture(t *testing.T) thinkingFixture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "..", "conformance", "llm-caller", "contract.json"))
	if err != nil {
		t.Fatalf("read the caller contract: %v", err)
	}
	var fixture thinkingFixture
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("decode the caller contract: %v", err)
	}
	if len(fixture.Thinking.OpenAICompatible) == 0 {
		t.Fatal("the caller contract has no thinking cases")
	}
	return fixture
}

func TestSingleCallOpenAIThinkingMatchesTheCallerContract(t *testing.T) {
	fixture := readThinkingFixture(t)
	for _, tc := range fixture.Thinking.OpenAICompatible {
		got := SingleCallOpenAIThinking(tc.Control, tc.Step)
		wantEffort := ""
		if tc.ReasoningEffort != nil {
			wantEffort = *tc.ReasoningEffort
		}
		if got.ReasoningEffort != wantEffort {
			t.Errorf("%s/%s: reasoning_effort %q, want %q", tc.Control, tc.Step, got.ReasoningEffort, wantEffort)
		}
		switch {
		case tc.EnableThinking == nil && got.EnableThinking != nil:
			t.Errorf("%s/%s: enable_thinking %v, want absent", tc.Control, tc.Step, *got.EnableThinking)
		case tc.EnableThinking != nil && (got.EnableThinking == nil || *got.EnableThinking != *tc.EnableThinking):
			t.Errorf("%s/%s: enable_thinking %v, want %v", tc.Control, tc.Step, got.EnableThinking, *tc.EnableThinking)
		}
	}
	for input, want := range fixture.Thinking.Settings {
		if got, ok := CanonicalReasoningEffort(input); !ok || got != want {
			t.Errorf("setting %q: got (%q, %v), want %q", input, got, ok, want)
		}
	}
	for _, input := range fixture.Thinking.RefusedSettings {
		if _, ok := CanonicalReasoningEffort(input); ok {
			t.Errorf("setting %q: accepted, want refused", input)
		}
	}
	for _, tc := range fixture.Thinking.Conflicts {
		if ReasoningEffortConflict(tc.Control, tc.Setting) == nil {
			t.Errorf("%s with %s: no conflict, want one", tc.Control, tc.Setting)
		}
	}
}

func TestSingleCallOpenAIThinkingAutoAndUndeclared(t *testing.T) {
	cases := []struct {
		control, setting, effort string
		enable                   *bool
		refusesTemperature       bool
	}{
		{"", "", "", nil, false},
		{"", ReasoningEffortAuto, "medium", nil, true},
		{"", ReasoningEffortOff, "none", nil, false},
		{ReasoningControlEffort, ReasoningEffortAuto, "medium", nil, true},
		{ReasoningControlBudget, ReasoningEffortHigh, "high", nil, true},
		{ReasoningControlToggle, ReasoningEffortAuto, "", ptr(true), false},
		{ReasoningControlToggle, ReasoningEffortOff, "", ptr(false), false},
		{ReasoningControlFixedOn, ReasoningEffortAuto, "", nil, false},
		{ReasoningControlFixedOff, ReasoningEffortOff, "", nil, false},
	}
	for _, tc := range cases {
		got := SingleCallOpenAIThinking(tc.control, tc.setting)
		if got.ReasoningEffort != tc.effort || got.RefusesTemperature != tc.refusesTemperature ||
			(got.EnableThinking == nil) != (tc.enable == nil) ||
			(got.EnableThinking != nil && *got.EnableThinking != *tc.enable) {
			t.Errorf("%q/%q: got %+v", tc.control, tc.setting, got)
		}
	}
}

func ptr[T any](value T) *T { return &value }
