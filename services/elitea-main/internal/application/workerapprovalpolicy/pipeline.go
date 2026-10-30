package workerapprovalpolicy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/approvalpolicy"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/toolcategory"
)

// PipelineAdmission checks the pipeline hitl rule when a pipeline run
// starts (ADR-0041 decision 5): under a policy that requires a `hitl` node
// for the agent's tier, a pipeline with a path to an external_send or
// payment tool node that passes no `hitl` node does not start.
//
// Pipelines are not published (publish refuses them,
// `pipeline_not_publishable`), so a run start is where the rule holds: every
// chat turn, schedule and trigger of a pipeline starts here.
type PipelineAdmission struct {
	settings SettingsReader
	tiers    TierReader
}

// NewPipelineAdmission builds the check.
func NewPipelineAdmission(settings SettingsReader, tiers TierReader) (*PipelineAdmission, error) {
	if settings == nil || tiers == nil {
		return nil, errors.New("worker approval policy: the settings and tier readers are required")
	}
	return &PipelineAdmission{settings: settings, tiers: tiers}, nil
}

// frozenVersion is the part of a frozen version the check reads.
type frozenVersion struct {
	AgentType    string `json:"agent_type"`
	Instructions string `json:"instructions"`
	Tools        []struct {
		Type        string `json:"type"`
		ToolkitName string `json:"toolkit_name"`
		Name        any    `json:"name"`
		Settings    struct {
			PinnedTools []struct {
				Name     string `json:"name"`
				Category string `json:"category"`
			} `json:"pinned_tools"`
		} `json:"settings"`
	} `json:"tools"`
}

// AdmitPipeline refuses the start of a pipeline that breaks the hitl rule
// of its tier (approvalpolicy.CodeHITLRequired). versionDetails is the
// frozen version: its tools carry the toolkit names the nodes bind and an
// approved MCP server's pins. Anything but a pipeline is admitted, and no
// registry row is read while no tier requires the rule.
func (a *PipelineAdmission) AdmitPipeline(ctx context.Context, projectID, applicationID int64, versionDetails json.RawMessage) error {
	settings, err := a.settings.Get(ctx)
	if err != nil {
		return fmt.Errorf("pipeline approval policy: read the platform setting: %w", err)
	}
	if len(settings.Policy.RequireHITL) == 0 {
		return nil
	}
	var version frozenVersion
	if json.Unmarshal(versionDetails, &version) != nil || version.AgentType != "pipeline" {
		// A malformed version is refused by the start's own checks.
		return nil
	}
	tier, err := ReadTier(ctx, a.tiers, projectID, applicationID)
	if err != nil {
		return err
	}
	if !settings.Policy.RequiresHITL(tier) {
		return nil
	}
	toolkits := make([]approvalpolicy.PipelineToolkit, 0, len(version.Tools))
	for _, tool := range version.Tools {
		var pins map[string][]toolcategory.Category
		if len(tool.Settings.PinnedTools) > 0 {
			pins = map[string][]toolcategory.Category{}
			for _, pin := range tool.Settings.PinnedTools {
				category, ok := toolcategory.Parse(pin.Category)
				if !ok {
					category = toolcategory.Default
				}
				pins[pin.Name] = append(pins[pin.Name], category)
			}
		}
		names := []string{tool.ToolkitName}
		if stored, ok := tool.Name.(string); ok && stored != tool.ToolkitName {
			names = append(names, stored)
		}
		for _, name := range names {
			if name != "" {
				toolkits = append(toolkits, approvalpolicy.PipelineToolkit{Name: name, Type: tool.Type, PinsCategories: pins})
			}
		}
	}
	return approvalpolicy.CheckPipelineHITL(version.Instructions, toolkits, tier)
}
