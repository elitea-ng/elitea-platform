package eliteacore

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/marketplace"
)

// The bindings of an installed or forked agent stay editable while its
// definition is read-only (ADR-0051 decision 3): which project toolkit backs
// each requirement is a project setting, not part of the definition.

type bindingsState struct {
	Requirements     marketplace.Requirements `json:"requirements"`
	Bindings         []bindingJSON            `json:"bindings"`
	ToolsEnabled     bool                     `json:"tools_enabled"`
	AwaitingApproval bool                     `json:"awaiting_approval"`
}

type bindingJSON struct {
	RequirementKey string `json:"requirement_key"`
	ToolkitID      string `json:"toolkit_id,omitempty"`
	Disabled       bool   `json:"disabled,omitempty"`
}

func (h *Handler) readBindings(ctx context.Context, q relationQuerier, projectID, appID int64, lock bool) (marketplace.StoredVersion, []marketplace.Binding, bool, error) {
	var listingID, versionID int64
	var raw []byte
	var enabled bool
	sql := `SELECT listing_id, version_id, bindings, tools_enabled FROM elitea_marketplace.installs
WHERE project_id = $1 AND entity_kind = 'agent' AND entity_id = $2`
	if lock {
		sql += ` FOR UPDATE`
	}
	if err := q.QueryRow(ctx, sql, projectID, appID).Scan(&listingID, &versionID, &raw, &enabled); err != nil {
		return marketplace.StoredVersion{}, nil, false, err
	}
	version, err := marketplace.LoadVersion(ctx, q, listingID, versionID)
	if err != nil {
		return marketplace.StoredVersion{}, nil, false, err
	}
	var bindings []marketplace.Binding
	_ = json.Unmarshal(raw, &bindings) // a malformed column reads as no bindings
	return version, bindings, enabled, nil
}

func bindingsResponse(version marketplace.StoredVersion, bindings []marketplace.Binding, enabled bool) bindingsState {
	out := bindingsState{Requirements: version.Requirements, Bindings: []bindingJSON{}, ToolsEnabled: enabled, AwaitingApproval: !enabled}
	if out.Requirements.Items == nil {
		out.Requirements.Items = []marketplace.Requirement{}
	}
	for _, b := range bindings {
		item := bindingJSON{RequirementKey: b.RequirementKey, Disabled: b.Disabled}
		if b.ToolkitID > 0 {
			item.ToolkitID = strconv.FormatInt(b.ToolkitID, 10)
		}
		out.Bindings = append(out.Bindings, item)
	}
	return out
}

// MarketplaceBindings answers GET and PUT
// /elitea_core/marketplace_bindings/prompt_lib/{projectID}/{applicationID}.
func (h *Handler) MarketplaceBindings(w http.ResponseWriter, r *http.Request) {
	if h.pool == nil {
		hireError(w, http.StatusServiceUnavailable, "unavailable", "The marketplace needs a database.")
		return
	}
	ctx := r.Context()
	projectIDText := chi.URLParam(r, "projectID")
	schema, ok := tenantSchema(w, projectIDText)
	if !ok {
		return
	}
	projectID := parseProjectID(projectIDText)
	appID, err := strconv.ParseInt(chi.URLParam(r, "applicationID"), 10, 64)
	if err != nil || appID <= 0 || projectID <= 0 {
		hireError(w, http.StatusBadRequest, "invalid_request", "The project or application id is not valid.")
		return
	}
	if r.Method == http.MethodGet {
		version, bindings, enabled, err := h.readBindings(ctx, h.pool, projectID, appID, false)
		if errors.Is(err, pgx.ErrNoRows) || errors.Is(err, marketplace.ErrNotFound) {
			hireError(w, http.StatusNotFound, "not_found", "The agent was not hired from the marketplace.")
			return
		}
		if err != nil {
			slog.ErrorContext(ctx, "marketplace bindings: read failed", "application_id", appID, "error", err)
			hireError(w, http.StatusInternalServerError, "internal_error", "Failed to read the bindings.")
			return
		}
		writeJSON(w, http.StatusOK, bindingsResponse(version, bindings, enabled))
		return
	}
	var body struct {
		Bindings []hireBindingBody `json:"bindings"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		hireError(w, http.StatusBadRequest, "invalid_request", "The request body is not valid.")
		return
	}
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		hireError(w, http.StatusInternalServerError, "internal_error", "Failed to save the bindings.")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	version, old, enabled, err := h.readBindings(ctx, tx, projectID, appID, true)
	if errors.Is(err, pgx.ErrNoRows) || errors.Is(err, marketplace.ErrNotFound) {
		hireError(w, http.StatusNotFound, "not_found", "The agent was not hired from the marketplace.")
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "marketplace bindings: read failed", "application_id", appID, "error", err)
		hireError(w, http.StatusInternalServerError, "internal_error", "Failed to save the bindings.")
		return
	}
	next, code, msg := h.checkBindings(ctx, schema, version.Requirements, body.Bindings)
	if code != "" {
		status := http.StatusConflict
		switch code {
		case "binding_invalid":
			status = http.StatusBadRequest
		case "internal_error":
			status = http.StatusInternalServerError
		}
		hireError(w, status, code, msg)
		return
	}
	if enabled {
		node, perr := marketplace.ParseDefinition(version.Definition)
		var versionRow int64
		qerr := tx.QueryRow(ctx, `SELECT id FROM `+schema+`.application_versions
WHERE application_id = $1 AND meta ? 'origin_listing_id' ORDER BY id LIMIT 1`, appID).Scan(&versionRow)
		if perr != nil || qerr != nil {
			slog.ErrorContext(ctx, "marketplace bindings: read the definition failed", "parse", perr, "query", qerr)
			hireError(w, http.StatusInternalServerError, "internal_error", "Failed to save the bindings.")
			return
		}
		if err := marketplace.DetachTools(ctx, tx, schema, versionRow, node, marketplace.BindingMap(old)); err != nil {
			slog.ErrorContext(ctx, "marketplace bindings: detach failed", "error", err)
			hireError(w, http.StatusInternalServerError, "internal_error", "Failed to save the bindings.")
			return
		}
		if err := marketplace.AttachTools(ctx, tx, schema, appID, versionRow, node, marketplace.BindingMap(next)); err != nil {
			slog.ErrorContext(ctx, "marketplace bindings: attach failed", "error", err)
			hireError(w, http.StatusInternalServerError, "internal_error", "Failed to save the bindings.")
			return
		}
	}
	stored, _ := json.Marshal(next)
	if _, err := tx.Exec(ctx, `UPDATE elitea_marketplace.installs SET bindings = $4::jsonb, updated_at = clock_timestamp()
WHERE project_id = $1 AND entity_kind = 'agent' AND entity_id = $2 AND listing_id = $3`,
		projectID, appID, version.ListingID, string(stored)); err != nil {
		hireError(w, http.StatusInternalServerError, "internal_error", "Failed to save the bindings.")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		hireError(w, http.StatusInternalServerError, "internal_error", "Failed to save the bindings.")
		return
	}
	writeJSON(w, http.StatusOK, bindingsResponse(version, next, enabled))
}
