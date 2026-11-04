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
	control   string
	toolSteps *bool
	err       error
	model     string
}

func (s *reasoningStub) ModelReasoning(_ context.Context, _ int64, model string) (string, *bool, error) {
	s.model = model
	return s.control, s.toolSteps, s.err
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
}

func TestSettleReasoningRefusesWhatTheModelCannotHonour(t *testing.T) {
	for effort, want := range map[string]string{
		"off":     "this model always thinks",
		"minimal": "reasoning_effort must be one of",
	} {
		block := map[string]any{"model_name": "qwq", "reasoning_effort": effort}
		err := SettleReasoning(context.Background(), &reasoningStub{control: "fixed_on"}, block, 7)
		if !errors.Is(err, ErrRejected) || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want ErrRejected naming %q", effort, err, want)
		}
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
	control, steps, err := reader.ModelReasoning(context.Background(), 7, "qwen3")
	if err != nil || control != "fixed_on" || steps == nil || *steps {
		t.Fatalf("own row: (%q, %v, %v)", control, steps, err)
	}
	if control, _, _ := reader.ModelReasoning(context.Background(), 9, "qwen3"); control != "toggle" {
		t.Errorf("shared row: %q", control)
	}
	if control, _, _ := reader.ModelReasoning(context.Background(), 7, "gpt-4o"); control != "fixed_off" {
		t.Errorf("supports_reasoning false: %q, want fixed_off", control)
	}
	if control, _, _ := reader.ModelReasoning(context.Background(), 7, "unknown"); control != "" {
		t.Errorf("unknown model: %q", control)
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
