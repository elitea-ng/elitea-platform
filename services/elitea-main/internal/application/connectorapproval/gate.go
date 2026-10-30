package connectorapproval

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/mcpregistry"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// PinnedTool is one approved tool of a custom MCP server.
type PinnedTool struct {
	Name   string `json:"name"`
	Digest string `json:"digest"`
	// Category is the ADR-0041 category the approved request declared for
	// the tool. The worker's approval policy reads it (a tool with no
	// category is write). A worker from an earlier release reads only the
	// name and the digest of a pin.
	Category ToolCategory `json:"category,omitempty"`
}

// PinnedToolsSetting is the frozen settings key the agent freeze writes on a
// custom MCP toolkit in approved_only: the list of PinnedTool. The worker
// keeps only the listed tools whose live digest matches. A list, not a map
// keyed by tool name, so the worker egress host walk never reads a tool name
// as a URL field.
const PinnedToolsSetting = "pinned_tools"

// Grants is what the approved requests admit one project.
type Grants struct {
	ToolkitTypes map[string]bool
	Prebuilt     map[string]bool
	// CustomMCP maps a canonical URL to the union of its approved pins.
	CustomMCP map[string][]PinnedTool
}

// ApprovedConnector is one approved request, as the grant reader answers it.
type ApprovedConnector struct {
	TargetKind TargetKind
	Target     string
	Tools      []DeclaredTool
}

// BuildGrants folds approved requests into Grants.
func BuildGrants(approved []ApprovedConnector) Grants {
	grants := Grants{ToolkitTypes: map[string]bool{}, Prebuilt: map[string]bool{}, CustomMCP: map[string][]PinnedTool{}}
	pinned := map[string]map[PinnedTool]bool{}
	for _, connector := range approved {
		switch connector.TargetKind {
		case TargetToolkitType:
			grants.ToolkitTypes[connector.Target] = true
		case TargetPrebuiltMCP:
			grants.Prebuilt[connector.Target] = true
		case TargetCustomMCP:
			if pinned[connector.Target] == nil {
				pinned[connector.Target] = map[PinnedTool]bool{}
			}
			for _, tool := range connector.Tools {
				if tool.Digest != "" {
					pinned[connector.Target][PinnedTool{Name: tool.Name, Digest: tool.Digest, Category: tool.Category}] = true
				}
			}
		}
	}
	for target, set := range pinned {
		pins := make([]PinnedTool, 0, len(set))
		for pin := range set {
			pins = append(pins, pin)
		}
		sort.Slice(pins, func(i, j int) bool {
			if pins[i].Name != pins[j].Name {
				return pins[i].Name < pins[j].Name
			}
			if pins[i].Digest != pins[j].Digest {
				return pins[i].Digest < pins[j].Digest
			}
			return pins[i].Category < pins[j].Category
		})
		grants.CustomMCP[target] = pins
	}
	return grants
}

// GrantReader answers the approved requests that name a project or the
// whole platform.
type GrantReader interface {
	ApprovedConnectors(ctx context.Context, projectID int64) ([]ApprovedConnector, error)
}

// SettingsReader answers the platform section.
type SettingsReader func(ctx context.Context) (platformconfig.CatalogueSettings, error)

// EffectiveMode is the mode a decision is made under: approved_only when
// the platform says so, when the process runs the governed credential
// profile, or when the run itself is governed.
func EffectiveMode(settings platformconfig.CatalogueSettings, governed bool) platformconfig.CatalogueMode {
	if governed || settings.Mode != platformconfig.CatalogueOpen {
		return platformconfig.CatalogueApprovedOnly
	}
	return platformconfig.CatalogueOpen
}

// Gate builds Views. A nil *Gate answers open Views (a composition with no
// database).
type Gate struct {
	settings SettingsReader
	grants   GrantReader
	governed bool
}

// NewGate builds the gate. governed is the process's credential profile.
func NewGate(settings SettingsReader, grants GrantReader, governed bool) (*Gate, error) {
	if settings == nil || grants == nil {
		return nil, errors.New("connectorapproval: the settings and grant readers are required")
	}
	return &Gate{settings: settings, grants: grants, governed: governed}, nil
}

// Governed reports whether the process runs the governed profile.
func (g *Gate) Governed() bool { return g != nil && g.governed }

// Settings reads the platform section.
func (g *Gate) Settings(ctx context.Context) (platformconfig.CatalogueSettings, error) {
	if g == nil {
		return platformconfig.DefaultCatalogueSettings(), nil
	}
	return g.settings(ctx)
}

// Mode reads the effective mode of the process (a governed run adds its own
// flag in ForProject).
func (g *Gate) Mode(ctx context.Context) (platformconfig.CatalogueMode, error) {
	if g == nil {
		return platformconfig.CatalogueOpen, nil
	}
	settings, err := g.settings(ctx)
	if err != nil {
		return platformconfig.CatalogueApprovedOnly, fmt.Errorf("connectorapproval: read the catalogue mode: %w", err)
	}
	return EffectiveMode(settings, g.governed), nil
}

// ForProject answers the View of one project. An error is never "open": an
// enforcement point that cannot read refuses (or, for a listing, serves
// nothing gated). governedRun holds the view to approved_only.
func (g *Gate) ForProject(ctx context.Context, projectID int64, governedRun bool) (View, error) {
	if g == nil {
		return View{Mode: platformconfig.CatalogueOpen}, nil
	}
	settings, err := g.settings(ctx)
	if err != nil {
		return View{Mode: platformconfig.CatalogueApprovedOnly, ProjectID: projectID},
			fmt.Errorf("connectorapproval: read the catalogue mode: %w", err)
	}
	mode := EffectiveMode(settings, g.governed || governedRun)
	if mode == platformconfig.CatalogueOpen {
		return View{Mode: mode, ProjectID: projectID}, nil
	}
	approved, err := g.grants.ApprovedConnectors(ctx, projectID)
	if err != nil {
		return View{Mode: mode, ProjectID: projectID},
			fmt.Errorf("connectorapproval: read the approved connectors: %w", err)
	}
	return View{Mode: mode, ProjectID: projectID, Grants: BuildGrants(approved)}, nil
}

// View is one project's answer under one mode.
type View struct {
	Mode      platformconfig.CatalogueMode
	ProjectID int64
	Grants    Grants
}

// ApprovedOnly reports whether the view gates anything.
func (v View) ApprovedOnly() bool { return v.Mode == platformconfig.CatalogueApprovedOnly }

// Decision is the answer for one toolkit.
type Decision struct {
	Allowed bool
	// Gated is false for open mode and platform-internal types.
	Gated bool
	// Kind and Target name the connector a request would have to approve.
	Kind   TargetKind
	Target string
	// Pins is set for an approved custom MCP server: the tools it may
	// expose.
	Pins []PinnedTool
}

// Connector names the connector a toolkit of this type and these settings
// is: its kind and its normalised target. ok is false for a
// platform-internal type, and for a custom MCP toolkit whose URL is not a
// valid endpoint (Target is then empty and nothing approves it).
func Connector(toolkitType string, settings map[string]any) (TargetKind, string, bool) {
	toolkitType = strings.TrimSpace(toolkitType)
	if toolkitType == "" || InternalToolkitType(toolkitType) {
		return "", "", false
	}
	switch {
	case toolkitType == customMCPToolkitType:
		raw, _ := settings["url"].(string)
		canonical, err := CanonicalMCPURL(raw)
		if err != nil {
			return TargetCustomMCP, "", true
		}
		return TargetCustomMCP, canonical, true
	case mcpregistry.IsPrebuiltToolkitType(toolkitType):
		return TargetPrebuiltMCP, mcpregistry.NormalizeCatalogueKey(toolkitType), true
	default:
		return TargetToolkitType, toolkitType, true
	}
}

// Decide answers one toolkit.
func (v View) Decide(toolkitType string, settings map[string]any) Decision {
	kind, target, gated := Connector(toolkitType, settings)
	if !v.ApprovedOnly() || !gated {
		return Decision{Allowed: true, Kind: kind, Target: target}
	}
	decision := Decision{Gated: true, Kind: kind, Target: target}
	switch kind {
	case TargetToolkitType:
		decision.Allowed = v.Grants.ToolkitTypes[target]
	case TargetPrebuiltMCP:
		decision.Allowed = v.Grants.Prebuilt[target]
	case TargetCustomMCP:
		pins, approved := v.Grants.CustomMCP[target]
		decision.Allowed = target != "" && approved
		decision.Pins = pins
	}
	return decision
}

// AllowsType answers the type catalogue's question (no settings yet): may a
// toolkit of this type be offered at all? A custom MCP type is offered when
// any custom MCP server is approved for the project; the URL is judged when
// the toolkit is saved.
func (v View) AllowsType(toolkitType string) bool {
	if !v.ApprovedOnly() {
		return true
	}
	if toolkitType == customMCPToolkitType {
		return len(v.Grants.CustomMCP) > 0
	}
	return v.Decide(toolkitType, nil).Allowed
}

// FilterTools keeps the discovered tools of a custom MCP server whose name
// and digest are pinned. The hidden tools' names come back too, for the
// caller to report.
func FilterTools(pins []PinnedTool, tools []mcpregistry.Tool) ([]mcpregistry.Tool, []string) {
	allowed := map[PinnedTool]bool{}
	for _, pin := range pins {
		allowed[pin] = true
	}
	kept := make([]mcpregistry.Tool, 0, len(tools))
	var hidden []string
	for _, tool := range tools {
		var schema any
		if tool.InputSchema != nil {
			schema = tool.InputSchema
		}
		digest, err := ToolDigest(tool.Name, tool.Description, schema)
		if err == nil && allowed[PinnedTool{Name: tool.Name, Digest: digest}] {
			kept = append(kept, tool)
			continue
		}
		hidden = append(hidden, tool.Name)
	}
	return kept, hidden
}

// PinNames is the set of pinned tool names.
func PinNames(pins []PinnedTool) map[string]bool {
	names := make(map[string]bool, len(pins))
	for _, pin := range pins {
		names[pin.Name] = true
	}
	return names
}

// PinsSetting is the frozen settings value of pins.
func PinsSetting(pins []PinnedTool) []any {
	out := make([]any, 0, len(pins))
	for _, pin := range pins {
		entry := map[string]any{"name": pin.Name, "digest": pin.Digest}
		if pin.Category != "" {
			entry["category"] = string(pin.Category)
		}
		out = append(out, entry)
	}
	return out
}

// RefusalMessage is the caller-safe sentence for a refused connector.
func RefusalMessage(decision Decision) string {
	switch decision.Kind {
	case TargetCustomMCP:
		if decision.Target == "" {
			return "This MCP server URL is not valid, so it cannot be approved. Correct the toolkit's URL."
		}
		return fmt.Sprintf("The MCP server %s is not approved for this project. Request the connector, "+
			"and a connector approver must approve it.", truncate(decision.Target, 256))
	case TargetPrebuiltMCP:
		return fmt.Sprintf("The MCP server %q is not approved for this project. Request the connector, "+
			"and a connector approver must approve it.", decision.Target)
	default:
		return fmt.Sprintf("The toolkit type %q is not approved for this project. Request the connector, "+
			"and a connector approver must approve it.", decision.Target)
	}
}

// ProjectViewer answers one project's View (Gate.ForProject).
type ProjectViewer interface {
	ForProject(ctx context.Context, projectID int64, governedRun bool) (View, error)
}

// PinDirectSettings writes the approved pins into the settings of a direct
// call or read (toolkit.call_tool.v1, toolkit.execute.read.v1) of a custom
// MCP toolkit gated under approved_only, as the agent freeze does, so the
// worker checks each tool's digest on this path too and the command gate can
// require the pins. Any other toolkit, and an unapproved one (the command
// gate refuses it), comes back unchanged. A nil viewer is the open mode. A
// view that cannot be read is an error: an unknown catalogue is not open.
func PinDirectSettings(ctx context.Context, viewer ProjectViewer, projectID int64, toolkitType string, settings map[string]any) (map[string]any, error) {
	if viewer == nil || settings == nil {
		return settings, nil
	}
	view, err := viewer.ForProject(ctx, projectID, false)
	if err != nil {
		return nil, err
	}
	decision := view.Decide(toolkitType, settings)
	if !decision.Allowed || !decision.Gated || decision.Kind != TargetCustomMCP {
		return settings, nil
	}
	pinned := make(map[string]any, len(settings)+1)
	for key, value := range settings {
		pinned[key] = value
	}
	pinned[PinnedToolsSetting] = PinsSetting(decision.Pins)
	return pinned, nil
}
