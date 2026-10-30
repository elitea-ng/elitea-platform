package eliteacore

// The connector catalogue gate on MCP discovery (ADR-0044 decision 1,
// #1501). Under approved_only, Load Tools (mcp_sync_tools):
//
//   - for an unapproved server (a custom URL, or a pre-built entry) still
//     answers the tools, as a preview the "Request connector" form declares
//     from, flagged `connector_approval_required` with the target to
//     request; it stores no registration, so no worker reads the server;
//   - for an approved custom server answers and stores only the pinned
//     tools (name and digest of an approved request): a tool the server added
//     or changed since is hidden and named in `hidden_tools`.

import (
	"context"
	"strconv"
	"strings"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/connectorapproval"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/mcpregistry"
)

// WithConnectorGate supplies the connector catalogue gate. Nil is the open
// mode.
func WithConnectorGate(gate *connectorapproval.Gate) Option {
	return func(h *Handler) {
		if gate != nil {
			h.connectors = gate
		}
	}
}

// mcpDiscoveryGate is the gate's answer for one discovery.
type mcpDiscoveryGate struct {
	// approvalRequired: the server is not approved for the project; the
	// discovery is a preview and is not stored.
	approvalRequired bool
	kind             connectorapproval.TargetKind
	target           string
	// pins restrict an approved custom server's tools (nil: no restriction).
	pins     []connectorapproval.PinnedTool
	pinsOnly bool
}

// mcpDiscoveryDecision judges a discovery of endpoint for toolkitType in
// rawProjectID. An error is a gate that could not read: the caller refuses.
func (h *Handler) mcpDiscoveryDecision(ctx context.Context, rawProjectID, toolkitType, endpoint string) (mcpDiscoveryGate, error) {
	if h.connectors == nil {
		return mcpDiscoveryGate{}, nil
	}
	projectID, _ := strconv.ParseInt(strings.TrimSpace(rawProjectID), 10, 64)
	view, err := h.connectors.ForProject(ctx, projectID, false)
	if err != nil {
		return mcpDiscoveryGate{}, err
	}
	if !view.ApprovedOnly() {
		return mcpDiscoveryGate{}, nil
	}
	toolkitType = strings.TrimSpace(toolkitType)
	if toolkitType == "" || !mcpregistry.IsPrebuiltToolkitType(toolkitType) {
		// A preview with no type, or a custom server: judged by its URL.
		toolkitType = "mcp"
	}
	decision := view.Decide(toolkitType, map[string]any{"url": endpoint})
	gate := mcpDiscoveryGate{kind: decision.Kind, target: decision.Target}
	if !decision.Allowed {
		gate.approvalRequired = true
		return gate, nil
	}
	if decision.Kind == connectorapproval.TargetCustomMCP {
		gate.pins, gate.pinsOnly = decision.Pins, true
	}
	return gate, nil
}
