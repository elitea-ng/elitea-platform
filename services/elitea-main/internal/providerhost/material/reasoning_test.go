package material

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
)

type reasoningStub struct {
	control      string
	toolSteps    *bool
	offWithTools *bool
	err          error
	model        string
}

func (s *reasoningStub) ModelReasoning(_ context.Context, _ int64, model string) (Reasoning, error) {
	s.model = model
	return Reasoning{Control: s.control, ToolSteps: s.toolSteps, OffWithTools: s.offWithTools}, s.err
}

func TestSettleReasoningCarriesTheCatalogueControl(t *testing.T) {
	yes := true
	block := map[string]any{"model_name": "qwen3", "reasoning_effort": "auto"}
	stub := &reasoningStub{control: "toggle", toolSteps: &yes}
	if err := SettleReasoning(context.Background(), stub, block, 7); err != nil {
		t.Fatal(err)
	}
	if stub.model != "qwen3" || block["reasoning_control"] != "toggle" || block["reasoning_tool_steps"] != true {
		t.Fatalf("block = %v", block)
	}
	if _, present := block["reasoning_off_with_tools"]; present {
		t.Fatalf("an undeclared off_with_tools must stay absent: %v", block)
	}

	// The GPT-5.6 family: the engine's client turns auto off beside tools.
	block = map[string]any{"model_name": "gpt-5.6", "reasoning_effort": "auto"}
	if err := SettleReasoning(context.Background(), &reasoningStub{control: "effort", offWithTools: &yes}, block, 7); err != nil {
		t.Fatal(err)
	}
	if block["reasoning_off_with_tools"] != true || block["reasoning_control"] != "effort" {
		t.Fatalf("block = %v", block)
	}
}

// ADR-0038, one rule on every surface: a stored setting the engine's model
// cannot honour runs as auto; the call is never refused for it.
func TestSettleReasoningRunsWhatTheModelCannotHonourAsAuto(t *testing.T) {
	for control, effort := range map[string]string{
		"fixed_on":  "off",
		"fixed_off": "high",
		"toggle":    "minimal",
	} {
		block := map[string]any{"model_name": "m", "reasoning_effort": effort}
		if err := SettleReasoning(context.Background(), &reasoningStub{control: control}, block, 7); err != nil {
			t.Fatalf("%s/%s: err = %v, want no refusal", control, effort, err)
		}
		if block["reasoning_effort"] != "auto" || block["reasoning_control"] != control {
			t.Errorf("%s/%s: block = %v, want auto with the control", control, effort, block)
		}
	}
	block := map[string]any{"model_name": "m", "reasoning_effort": "none"}
	_ = SettleReasoning(context.Background(), &reasoningStub{control: "effort"}, block, 7)
	if block["reasoning_effort"] != "off" {
		t.Errorf("none is canonical off: %v", block)
	}
}

func TestSettleReasoningKeepsTheBlockWithoutAControl(t *testing.T) {
	for name, tc := range map[string]struct {
		reasoning ModelReasoning
		block     map[string]any
	}{
		"no reader":        {nil, map[string]any{"model_name": "m", "reasoning_effort": "auto"}},
		"no model":         {&reasoningStub{control: "toggle"}, map[string]any{"reasoning_effort": "auto"}},
		"undeclared":       {&reasoningStub{}, map[string]any{"model_name": "m", "reasoning_effort": "auto"}},
		"catalogue outage": {&reasoningStub{control: "toggle", err: errors.New("down")}, map[string]any{"model_name": "m"}},
	} {
		if err := SettleReasoning(context.Background(), tc.reasoning, tc.block, 7); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if _, present := tc.block["reasoning_control"]; present {
			t.Errorf("%s: control added: %v", name, tc.block)
		}
	}
}

type catalogueStub struct {
	items []configurationapp.CurrentModelCatalogItem
}

func (c catalogueStub) Get(context.Context, configurationapp.CurrentModelCatalogQuery) (configurationapp.CurrentModelCatalogResponse, error) {
	return configurationapp.CurrentModelCatalogResponse{Items: c.items}, nil
}

func TestCatalogueReasoningPrefersTheProjectsOwnRow(t *testing.T) {
	toggle, fixed, no := "toggle", "fixed_on", false
	reader := CatalogueReasoning{PublicProjectID: 1, Models: catalogueStub{items: []configurationapp.CurrentModelCatalogItem{
		{Name: "qwen3", ProjectID: 1, ReasoningControl: &toggle},
		{Name: "qwen3", ProjectID: 7, ReasoningControl: &fixed, ReasoningToolSteps: &no},
		{Name: "gpt-4o", ProjectID: 7, SupportsReasoning: &no},
	}}}
	own, err := reader.ModelReasoning(context.Background(), 7, "qwen3")
	if err != nil || own.Control != "fixed_on" || own.ToolSteps == nil || *own.ToolSteps {
		t.Fatalf("own row: (%+v, %v)", own, err)
	}
	if got, _ := reader.ModelReasoning(context.Background(), 9, "qwen3"); got.Control != "toggle" {
		t.Errorf("shared row: %q", got.Control)
	}
	if got, _ := reader.ModelReasoning(context.Background(), 7, "gpt-4o"); got.Control != "fixed_off" {
		t.Errorf("supports_reasoning false: %q, want fixed_off", got.Control)
	}
	if got, _ := reader.ModelReasoning(context.Background(), 7, "unknown"); got.Control != "" {
		t.Errorf("unknown model: %q", got.Control)
	}
}

type oneMinter struct{}

func (oneMinter) Mint(context.Context, int64, int64, string, time.Duration) (Grant, error) {
	return Grant{Bearer: "b", UUID: "u"}, nil
}
func (oneMinter) Revoke(context.Context, int64, string) error { return nil }

// The whole rewrite: the engine's llm_settings carries the control.
func TestCallbackOnlyRewriteCarriesTheReasoningControl(t *testing.T) {
	rewrite := CallbackOnly("inventory", oneMinter{}, &reasoningStub{control: "budget"}, PlatformRouteBase, time.Minute)
	body := `{"configuration":{"parameters":{"llm_model":"claude-sonnet"}},"parameters":{"llm_settings":{"reasoning_effort":"auto"}}}`
	rewritten, _, err := rewrite(context.Background(), strings.NewReader(body), 7, 3)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Configuration struct {
			Parameters struct {
				LLMSettings map[string]any `json:"llm_settings"`
			} `json:"parameters"`
		} `json:"configuration"`
	}
	if err := json.Unmarshal(rewritten, &out); err != nil {
		t.Fatal(err)
	}
	settings := out.Configuration.Parameters.LLMSettings
	if settings["reasoning_control"] != "budget" || settings["reasoning_effort"] != "auto" || settings["model_name"] != "claude-sonnet" {
		t.Fatalf("llm_settings = %v", settings)
	}
}
