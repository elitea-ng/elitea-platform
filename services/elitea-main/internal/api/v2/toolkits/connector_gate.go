package toolkits

// The connector catalogue gate on the toolkit surfaces (ADR-0044 decision 1,
// #1501). In `approved_only` a toolkit type, a pre-built MCP server or a
// custom MCP server URL is usable only by the projects an approved connector
// request names (internal/application/connectorapproval):
//
//   - the type catalogue and the type list offer only approved types (a
//     custom MCP type is offered when any custom server is approved for the
//     project; its URL is judged when the toolkit is saved);
//   - create, fork and a settings or type update of a toolkit are refused
//     with 403 connector_not_approved, naming the target a request would
//     approve.
//
// Unlike the type policy, a gate that cannot read does not serve
// everything: in approved_only an unreadable approval list offers only the
// platform-internal types, and a write is refused with 503.

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/connectorapproval"
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

// connectorFilter is the gate as a PolicyFilter.
type connectorFilter struct {
	view   connectorapproval.View
	failed bool
}

func (f connectorFilter) Allows(toolkitType string) bool {
	if f.failed {
		return connectorapproval.InternalToolkitType(toolkitType)
	}
	return f.view.AllowsType(toolkitType)
}

// bothFilters admits a type both filters admit.
type bothFilters struct{ first, second PolicyFilter }

func (f bothFilters) Allows(toolkitType string) bool {
	return f.first.Allows(toolkitType) && f.second.Allows(toolkitType)
}

func numericProject(projectID string) int64 {
	value, err := strconv.ParseInt(strings.TrimSpace(projectID), 10, 64)
	if err != nil || value <= 0 {
		return 0
	}
	return value
}

// catalogueFilter is the type policy filter AND the connector gate: what a
// project may be offered.
func (h *Handler) catalogueFilter(ctx context.Context, surface, projectID string) PolicyFilter {
	policy := h.typePolicyFilter(ctx, surface, projectID)
	if h == nil || h.connectors == nil {
		return policy
	}
	view, err := h.connectors.ForProject(ctx, numericProject(projectID), false)
	if err != nil {
		slog.ErrorContext(ctx, "connector catalogue: read failed; offering platform-internal types only",
			"surface", surface, "project_id", projectID, "err", err)
		return bothFilters{first: policy, second: connectorFilter{failed: true}}
	}
	if !view.ApprovedOnly() {
		return policy
	}
	return bothFilters{first: policy, second: connectorFilter{view: view}}
}

// refuseUnapprovedConnector answers a toolkit write naming a connector the
// project may not use. It returns true when it has written a refusal.
func (h *Handler) refuseUnapprovedConnector(
	w http.ResponseWriter, r *http.Request, surface, projectID, toolkitType string, settings map[string]any,
) bool {
	if h == nil || h.connectors == nil || strings.TrimSpace(toolkitType) == "" {
		return false
	}
	view, err := h.connectors.ForProject(r.Context(), numericProject(projectID), false)
	if err != nil {
		slog.ErrorContext(r.Context(), "connector catalogue: read failed; refusing the write",
			"surface", surface, "project_id", projectID, "err", err)
		refusal := connectorapproval.Refuse(connectorapproval.CatalogueUnavailableCode,
			"The connector catalogue could not be read, so the toolkit cannot be saved now. Try again.")
		writeJSON(w, refusal.Status(), refusal.Body())
		return true
	}
	decision := view.Decide(toolkitType, settings)
	if decision.Allowed {
		return false
	}
	slog.WarnContext(r.Context(), "connector catalogue: refused an unapproved connector",
		"surface", surface, "project_id", projectID, "toolkit_type", toolkitType,
		"target_kind", string(decision.Kind))
	refusal := connectorapproval.NotApproved(decision)
	writeJSON(w, refusal.Status(), refusal.Body())
	return true
}

// refuseUnapprovedUpdate judges an update on the toolkit it produces: the
// body's type or the stored one, the body's settings or the stored ones. A
// stored row that cannot be read is judged on what the body says; with no
// type at all the update fails on its own (the row is missing).
func (h *Handler) refuseUnapprovedUpdate(
	w http.ResponseWriter, r *http.Request, projectID, toolkitID, bodyType string,
	bodySettings map[string]any, hasSettings bool,
) bool {
	if h == nil || h.connectors == nil {
		return false
	}
	toolkitType, settings := bodyType, bodySettings
	if toolkitType == "" || !hasSettings {
		stored, err := h.repo.GetToolkit(r.Context(), projectID, toolkitID)
		if err == nil {
			if toolkitType == "" {
				toolkitType, _ = stored["type"].(string)
			}
			if !hasSettings {
				settings, _ = stored["settings"].(map[string]any)
			}
		}
	}
	if toolkitType == "" {
		// Nothing to judge: the update itself fails on the missing row.
		return false
	}
	return h.refuseUnapprovedConnector(w, r, "update_toolkit", projectID, toolkitType, settings)
}
