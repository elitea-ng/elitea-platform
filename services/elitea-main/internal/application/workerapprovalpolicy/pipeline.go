package workerapprovalpolicy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/approvalpolicy"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/toolcategory"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// PipelineAdmission checks the pipeline hitl rule when a run is admitted
// (ADR-0041 decision 5): under a policy that requires a `hitl` node for the
// agent's tier, a pipeline with a path to an external_send or payment tool
// node that passes no `hitl` node does not start.
//
// Pipelines are not published (publish refuses them,
// `pipeline_not_publishable`), so run admission is where the rule holds: the
// start service checks every freeze of a run's root version (start, ad hoc,
// regenerate, continue; schedules and triggers start through it).
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

// ChildVersions answers the frozen version of one saved child the run
// reaches (an application tool), in the run's project, frozen as the run's
// own version is. A child that does not exist answers an error.
type ChildVersions = func(ctx context.Context, applicationID, versionID int64) (json.RawMessage, error)

// MaxCompositionDepth is the nesting cap of saved pipelines below the root:
// the worker's MAX_PIPELINE_COMPOSITION_DEPTH.
const MaxCompositionDepth = 3

// maxCompositionReads bounds the child versions one check reads.
const maxCompositionReads = 64

// frozenVersion is the part of a frozen version the check reads.
type frozenVersion struct {
	AgentType    string `json:"agent_type"`
	Instructions string `json:"instructions"`
	Tools        []struct {
		Type        string `json:"type"`
		ToolkitName string `json:"toolkit_name"`
		Name        any    `json:"name"`
		AgentType   any    `json:"agent_type"`
		Settings    struct {
			ApplicationID        any `json:"application_id"`
			ApplicationVersionID any `json:"application_version_id"`
			PinnedTools          any `json:"pinned_tools"`
		} `json:"settings"`
	} `json:"tools"`
}

type pinnedTool struct {
	Name     string `json:"name"`
	Category string `json:"category"`
}

// AdmitPipeline refuses the start of a run that breaks the hitl rule
// (approvalpolicy.CodeHITLRequired). versionDetails is the frozen version of
// the run's root: its tools carry the toolkit names the nodes bind, an
// approved MCP server's pins and the saved child pipelines it attaches.
//
// The check covers the root when it is a pipeline, every saved pipeline an
// `agent` node of a checked pipeline runs, and every saved pipeline the root
// or a checked pipeline attaches as a tool, MaxCompositionDepth levels deep.
// children reads those versions. A version is checked under the first tier
// that requires the rule: its parent's, else its own registry row's.
//
// No registry row or child is read while no tier requires the rule. A root
// that is not a pipeline and attaches no pipeline is admitted.
func (a *PipelineAdmission) AdmitPipeline(
	ctx context.Context,
	projectID, applicationID int64,
	versionDetails json.RawMessage,
	children ChildVersions,
) error {
	settings, err := a.settings.Get(ctx)
	if err != nil {
		return fmt.Errorf("pipeline approval policy: read the platform setting: %w", err)
	}
	if len(settings.Policy.RequireHITL) == 0 {
		return nil
	}
	var version frozenVersion
	if json.Unmarshal(versionDetails, &version) != nil {
		// A malformed version is refused by the start's own checks.
		return nil
	}
	if version.AgentType != "pipeline" && len(pipelineChildren(version)) == 0 {
		return nil
	}
	walk := compositionWalk{admission: a, settings: settings, projectID: projectID, children: children, open: map[[2]int64]bool{}}
	// The worker admits a pipeline an agent attaches as a tool as the root
	// of its own composition: it counts depth from there.
	depth := 0
	if version.AgentType != "pipeline" {
		depth = -1
	}
	graph, err := walk.graph(ctx, version, applicationID, "", depth)
	if err != nil {
		return err
	}
	return approvalpolicy.CheckPipelineGraph(graph)
}

type compositionWalk struct {
	admission *PipelineAdmission
	settings  platformconfig.ApprovalPolicySettings
	projectID int64
	children  ChildVersions
	// open holds the child versions on the current branch: a cycle.
	open  map[[2]int64]bool
	reads int
}

// graph builds the checked graph of one version. inherited is the tier of
// a parent that requires the rule, or empty.
func (w *compositionWalk) graph(
	ctx context.Context, version frozenVersion, applicationID int64, inherited agentlifecycle.Tier, depth int,
) (*approvalpolicy.PipelineGraph, error) {
	tier := inherited
	if tier == "" {
		own := agentlifecycle.TierLow
		if applicationID > 0 {
			read, err := ReadTier(ctx, w.admission.tiers, w.projectID, applicationID)
			if err != nil {
				return nil, err
			}
			own = read
		}
		if w.settings.Policy.RequiresHITL(own) {
			tier = own
		}
	}
	graph := &approvalpolicy.PipelineGraph{
		IsPipeline:   version.AgentType == "pipeline",
		Instructions: version.Instructions,
		Toolkits:     versionToolkits(version),
		Tier:         tier,
	}
	references := pipelineChildren(version)
	if len(references) == 0 {
		return graph, nil
	}
	if depth >= MaxCompositionDepth {
		return nil, approvalpolicy.CompositionRefusal(fmt.Sprintf("its saved pipelines nest more than %d levels deep", MaxCompositionDepth))
	}
	if w.children == nil {
		return nil, errors.New("pipeline approval policy: no reader of child versions is attached")
	}
	graph.Children = make(map[string]*approvalpolicy.PipelineGraph, len(references))
	for _, reference := range references {
		identity := [2]int64{reference.applicationID, reference.versionID}
		if w.open[identity] {
			return nil, approvalpolicy.CompositionRefusal("its saved pipelines form a cycle")
		}
		if w.reads == maxCompositionReads {
			return nil, approvalpolicy.CompositionRefusal(fmt.Sprintf("it reaches more than %d saved pipelines", maxCompositionReads))
		}
		w.reads++
		details, err := w.children(ctx, reference.applicationID, reference.versionID)
		if err != nil {
			return nil, fmt.Errorf("pipeline approval policy: read the saved pipeline %q: %w", reference.alias, err)
		}
		var child frozenVersion
		if json.Unmarshal(details, &child) != nil || child.AgentType != "pipeline" {
			// The worker refuses a pipeline reference that is not one.
			continue
		}
		w.open[identity] = true
		childGraph, err := w.graph(ctx, child, reference.applicationID, tier, depth+1)
		delete(w.open, identity)
		if err != nil {
			return nil, err
		}
		graph.Children[reference.alias] = childGraph
	}
	return graph, nil
}

type childReference struct {
	alias                    string
	applicationID, versionID int64
}

// pipelineChildren answers the saved pipelines a version attaches: its
// `application` tools whose agent type is pipeline. An agent node selects
// one by its `toolkit_name`. A second tool with the same alias keeps a
// distinct key, so no agent node selects it and it is checked on its own.
func pipelineChildren(version frozenVersion) []childReference {
	var out []childReference
	seen := map[string]bool{}
	for index, tool := range version.Tools {
		if tool.Type != "application" || tool.AgentType != "pipeline" {
			continue
		}
		applicationID, okApplication := positiveID(tool.Settings.ApplicationID)
		versionID, okVersion := positiveID(tool.Settings.ApplicationVersionID)
		if !okApplication || !okVersion {
			continue
		}
		alias := tool.ToolkitName
		if seen[alias] {
			alias = fmt.Sprintf("%s#%d", alias, index)
		}
		seen[alias] = true
		out = append(out, childReference{alias: alias, applicationID: applicationID, versionID: versionID})
	}
	return out
}

func positiveID(value any) (int64, bool) {
	number, ok := value.(float64)
	if !ok || number < 1 || number > math.MaxInt32 || number != math.Trunc(number) {
		return 0, false
	}
	return int64(number), true
}

// versionToolkits answers the toolkits a version's tool nodes bind.
func versionToolkits(version frozenVersion) []approvalpolicy.PipelineToolkit {
	toolkits := make([]approvalpolicy.PipelineToolkit, 0, len(version.Tools))
	for _, tool := range version.Tools {
		var pins map[string][]toolcategory.Category
		var pinned []pinnedTool
		if raw, err := json.Marshal(tool.Settings.PinnedTools); err == nil && json.Unmarshal(raw, &pinned) == nil && len(pinned) > 0 {
			pins = map[string][]toolcategory.Category{}
			for _, pin := range pinned {
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
	return toolkits
}
