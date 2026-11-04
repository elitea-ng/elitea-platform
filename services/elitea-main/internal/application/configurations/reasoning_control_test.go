package configurations

import (
	"strings"
	"testing"
)

func TestCanonicalReasoningEffortReadsNoneAsOff(t *testing.T) {
	for input, want := range map[string]string{
		"auto": "auto", "off": "off", "none": "off", "low": "low", "medium": "medium", "high": "high",
	} {
		got, ok := CanonicalReasoningEffort(input)
		if !ok || got != want {
			t.Errorf("%q: got (%q, %v), want %q", input, got, ok, want)
		}
	}
	for _, input := range []string{"", "minimal", "AUTO", "xhigh"} {
		if _, ok := CanonicalReasoningEffort(input); ok {
			t.Errorf("%q must not be canonical", input)
		}
	}
}

// The texts are the llm-wire table's (conformance/llm-caller/contract.json).
func TestReasoningEffortConflictMatchesTheTable(t *testing.T) {
	cases := []struct {
		control, effort string
		want            string
	}{
		{ReasoningControlFixedOn, "off", "this model always thinks: set reasoning_effort to auto, low, medium or high, not off"},
		{ReasoningControlFixedOn, "auto", ""},
		{ReasoningControlFixedOn, "high", ""},
		{ReasoningControlFixedOff, "low", "this model cannot think: set reasoning_effort to auto or off, not a level"},
		{ReasoningControlFixedOff, "auto", ""},
		{ReasoningControlFixedOff, "off", ""},
		{ReasoningControlToggle, "off", ""},
		{ReasoningControlEffort, "high", ""},
		{"", "off", ""},
	}
	for _, tc := range cases {
		err := ReasoningEffortConflict(tc.control, tc.effort)
		got := ""
		if err != nil {
			got = err.Error()
		}
		if got != tc.want {
			t.Errorf("(%s, %s): got %q, want %q", tc.control, tc.effort, got, tc.want)
		}
	}
}

func TestValidateLLMModelReasoning(t *testing.T) {
	valid := []map[string]any{
		{},
		{"reasoning_control": nil, "reasoning_tool_steps": nil},
		{"reasoning_control": "toggle", "supports_reasoning": true},
		{"reasoning_control": "budget", "reasoning_tool_steps": true},
		{"reasoning_control": "fixed_off", "supports_reasoning": false},
	}
	for _, data := range valid {
		if err := ValidateLLMModelReasoning(data); err != nil {
			t.Errorf("%v: %v", data, err)
		}
	}
	invalid := map[string]map[string]any{
		"one of":                  {"reasoning_control": "adaptive"},
		"one of ":                 {"reasoning_control": 3},
		"boolean":                 {"reasoning_tool_steps": "yes"},
		"supports_reasoning true": {"reasoning_control": "toggle", "supports_reasoning": false},
	}
	for want, data := range invalid {
		err := ValidateLLMModelReasoning(data)
		if err == nil || !strings.Contains(err.Error(), strings.TrimSpace(want)) {
			t.Errorf("%v: got %v, want an error naming %q", data, err, want)
		}
	}
}

func mustDefaults(t *testing.T) []ModelReasoningDefault {
	t.Helper()
	var defaults []ModelReasoningDefault
	for _, row := range []struct {
		pattern, control string
		toolSteps        bool
	}{
		{"qwen[^/]*-thinking", "fixed_on", false},
		{"(^|/)qwen", "toggle", false},
		{"claude", "budget", true},
	} {
		rule, err := CompileModelReasoningDefault(row.pattern, row.control, row.toolSteps)
		if err != nil {
			t.Fatal(err)
		}
		defaults = append(defaults, rule)
	}
	return defaults
}

func TestApplyModelReasoningDefaults(t *testing.T) {
	yes, no := true, false
	toggle, effort := "toggle", "effort"
	defaults := mustDefaults(t)

	item := CurrentModelCatalogItem{Name: "Qwen/Qwen3-8B", SupportsReasoning: &yes}
	ApplyModelReasoningDefaults(&item, defaults)
	if item.ReasoningControl == nil || *item.ReasoningControl != toggle || item.ReasoningToolSteps == nil || *item.ReasoningToolSteps {
		t.Fatalf("a Qwen reasoning model takes toggle without tool steps: %+v", item)
	}

	item = CurrentModelCatalogItem{Name: "qwen3-235b-a22b-thinking-2507", SupportsReasoning: &yes}
	ApplyModelReasoningDefaults(&item, defaults)
	if *item.ReasoningControl != "fixed_on" {
		t.Fatalf("the first match in priority order wins: %q", *item.ReasoningControl)
	}

	// The row's own key wins; only the missing key takes the default.
	item = CurrentModelCatalogItem{Name: "claude-sonnet-4-5", SupportsReasoning: &yes, ReasoningControl: &effort}
	ApplyModelReasoningDefaults(&item, defaults)
	if *item.ReasoningControl != effort || item.ReasoningToolSteps == nil || !*item.ReasoningToolSteps {
		t.Fatalf("own control kept, tool steps from the family: %+v", item)
	}

	// A model that does not say it reasons takes no default.
	for _, supports := range []*bool{nil, &no} {
		item = CurrentModelCatalogItem{Name: "Qwen/Qwen2.5-7B", SupportsReasoning: supports}
		ApplyModelReasoningDefaults(&item, defaults)
		if item.ReasoningControl != nil || item.ReasoningToolSteps != nil {
			t.Fatalf("a non-reasoning model took a default: %+v", item)
		}
	}

	// No match: nothing, so the caller keeps its previous form.
	item = CurrentModelCatalogItem{Name: "mistral-large", SupportsReasoning: &yes}
	ApplyModelReasoningDefaults(&item, defaults)
	if item.ReasoningControl != nil {
		t.Fatalf("an unmatched model took a default: %+v", item)
	}
}

func TestCompileModelReasoningDefaultRefusesBadRows(t *testing.T) {
	if _, err := CompileModelReasoningDefault("(?<=x)y", "toggle", false); err == nil {
		t.Fatal("a pattern RE2 does not take must be refused")
	}
	if _, err := CompileModelReasoningDefault("x", "adaptive", false); err == nil {
		t.Fatal("an unknown control must be refused")
	}
}
