package material

// The model's thinking control, for the engines' model client (ADR-0038
// decisions 3 and 4, #1452).
//
// The engines (DeepWiki, Inventory) read llm_settings through
// libs/rust/model-client: with reasoning_control (and reasoning_tool_steps,
// reasoning_off_with_tools) there, `auto` is the per-step policy and every setting takes the model's
// own form; without it, the client keeps the reasoning_effort form and
// thinks at the default level on every call. The block carries the model's
// catalogue resolution, the same one admission freezes for an agent run.

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
)

// Reasoning is one model's catalogue reasoning data. Control "" means the
// catalogue declares none; a nil rule is not declared.
type Reasoning struct {
	Control      string
	ToolSteps    *bool
	OffWithTools *bool
}

// ModelReasoning reads one model's control form, tool-step rule and
// off-with-tools rule from the project's catalogue, in one read.
type ModelReasoning interface {
	ModelReasoning(ctx context.Context, projectID int64, model string) (Reasoning, error)
}

// ModelCatalog is the project model catalogue (the configurations runtime's).
type ModelCatalog interface {
	Get(context.Context, configurationapp.CurrentModelCatalogQuery) (configurationapp.CurrentModelCatalogResponse, error)
}

// WithModelReasoning returns a Minter that also reads the model's reasoning
// for every rewrite it mints for: Settle and SourceRewriter settle the
// callback block with it when they name no reader of their own. The
// composition root wraps each facade's minter once, so no facade carries
// the plumbing (ADR-0012 budget). A nil reasoning returns minter unchanged.
func WithModelReasoning(minter Minter, reasoning ModelReasoning) Minter {
	if minter == nil || reasoning == nil {
		return minter
	}
	return reasoningMinter{Minter: minter, reasoning: reasoning}
}

// reasoningMinter names its reader: an embedded ModelReasoning field would
// shadow the method of the same name, and the type would not satisfy it.
type reasoningMinter struct {
	Minter
	reasoning ModelReasoning
}

func (m reasoningMinter) ModelReasoning(ctx context.Context, projectID int64, model string) (Reasoning, error) {
	return m.reasoning.ModelReasoning(ctx, projectID, model)
}

// reasoningFor is the reader a rewrite settles with: its own, else the one
// its minter carries.
func reasoningFor(own ModelReasoning, minter Minter) ModelReasoning {
	if own != nil {
		return own
	}
	carried, _ := minter.(ModelReasoning)
	return carried
}

// CatalogueReasoning resolves a model through the project catalogue, shared
// models included: the project's own row of the name first.
type CatalogueReasoning struct {
	Models          ModelCatalog
	PublicProjectID int32
}

// ModelReasoning implements ModelReasoning.
func (c CatalogueReasoning) ModelReasoning(ctx context.Context, projectID int64, model string) (Reasoning, error) {
	project, ok := NarrowRowID(projectID)
	if !ok {
		return Reasoning{}, fmt.Errorf("project %s is out of range", strconv.FormatInt(projectID, 10))
	}
	catalog, err := c.Models.Get(ctx, configurationapp.CurrentModelCatalogQuery{
		Section: configurationapp.CurrentModelSectionLLM, ProjectID: project,
		PublicProjectID: c.PublicProjectID, IncludeShared: true,
	})
	if err != nil {
		return Reasoning{}, err
	}
	item := configurationapp.FindModelCatalogItem(catalog.Items, model, project)
	if item == nil {
		return Reasoning{}, nil
	}
	return Reasoning{
		Control:      configurationapp.ModelItemReasoningControl(item),
		ToolSteps:    item.ReasoningToolSteps,
		OffWithTools: item.ReasoningOffWithTools,
	}, nil
}

// SettleReasoning writes the model's control form, tool-step rule and
// off-with-tools rule into the callback block. A stored setting the model cannot honour (the toolkit's
// saved default, or a value a client lifted into the block) runs as auto and
// is logged at warn: the one run-time rule of every surface
// (configurations.RunReasoningEffort). It never refuses the call. A block
// with no model, a nil reader, or a catalogue that cannot be read keeps the
// block as it is: the engine then sends the reasoning_effort form, as before
// ADR-0038.
func SettleReasoning(ctx context.Context, reasoning ModelReasoning, block map[string]any, projectID int64) error {
	model, _ := block["model_name"].(string)
	if reasoning == nil || model == "" {
		return nil
	}
	declared, err := reasoning.ModelReasoning(ctx, projectID, model)
	if err != nil {
		slog.WarnContext(ctx, "provider callback: model catalogue unavailable; the engine keeps the reasoning_effort form",
			"project_id", projectID, "model", model, "err", err)
		return nil
	}
	control := declared.Control
	if control == "" {
		return nil
	}
	if effort, ok := block["reasoning_effort"].(string); ok && effort != "" {
		block["reasoning_effort"] = configurationapp.RunReasoningEffort(ctx, control, effort, model, "engine")
	}
	block["reasoning_control"] = control
	if declared.ToolSteps != nil {
		block["reasoning_tool_steps"] = *declared.ToolSteps
	}
	if declared.OffWithTools != nil {
		block["reasoning_off_with_tools"] = *declared.OffWithTools
	}
	return nil
}
