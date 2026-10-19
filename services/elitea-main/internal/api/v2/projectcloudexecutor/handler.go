// Package projectcloudexecutor serves a project's own Send to cloud switch
// (ADR-0034 decision 11, owner decision 2026-10-19) on two browser-facing
// routes that share one handler:
//
//	GET|PUT /api/v2/elitea_core/project_cloud_executor/prompt_lib/{projectID}
//	        the project settings page; read with models.project_context.view,
//	        write with models.project_settings.edit (the project admin's)
//	GET|PUT /api/v2/admin/project_cloud_executor/administration/{projectID}
//	        the admin Projects page, for any project; read with
//	        projects.projects.projects.view, write with
//	        projects.projects.projects.edit (central, administration mode)
//
// The router applies those gates. The handler only decodes and maps errors.
// The PUT body is {"enabled": true | false | null}; null removes the
// project's explicit value so it follows the platform's
// cloud_executor.project_default_enabled.
package projectcloudexecutor

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	handoffapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/workspacehandoff"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

// The route patterns, relative to their enclosing /api/v2/elitea_core and
// /api/v2/admin groups.
const (
	ProjectPath = "/project_cloud_executor/prompt_lib/{projectID}"
	AdminPath   = "/project_cloud_executor/administration/{projectID}"
)

// Switch is *handoffapp.ProjectSwitchService.
type Switch interface {
	State(ctx context.Context, projectID int64) (handoffapp.ProjectSwitchState, error)
	Set(ctx context.Context, projectID int64, enabled *bool) (handoffapp.ProjectSwitchState, error)
}

var _ Switch = (*handoffapp.ProjectSwitchService)(nil)

// Handler serves both routes. A nil Switch answers 503.
type Handler struct{ projectSwitch Switch }

func NewHandler(projectSwitch Switch) *Handler { return &Handler{projectSwitch: projectSwitch} }

// Get answers the project's switch.
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	projectID, ok := h.project(w, r)
	if !ok {
		return
	}
	state, err := h.projectSwitch.State(r.Context(), projectID)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	writeJSON(w, http.StatusOK, state)
}

// Put stores the project's explicit value, or removes it for null.
func (h *Handler) Put(w http.ResponseWriter, r *http.Request) {
	projectID, ok := h.project(w, r)
	if !ok {
		return
	}
	var body map[string]json.RawMessage
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil {
		apierr.WriteStatus(w, http.StatusBadRequest, "invalid request body")
		return
	}
	raw, present := body["enabled"]
	if !present || len(body) != 1 {
		apierr.WriteStatus(w, http.StatusBadRequest, "the body is {\"enabled\": true, false or null}")
		return
	}
	var enabled *bool
	if err := json.Unmarshal(raw, &enabled); err != nil {
		apierr.WriteStatus(w, http.StatusBadRequest, "enabled must be true, false or null")
		return
	}
	state, err := h.projectSwitch.Set(r.Context(), projectID, enabled)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	writeJSON(w, http.StatusOK, state)
}

func (h *Handler) project(w http.ResponseWriter, r *http.Request) (int64, bool) {
	if h.projectSwitch == nil {
		apierr.WriteStatus(w, http.StatusServiceUnavailable, "database unavailable")
		return 0, false
	}
	projectID, err := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 32)
	if err != nil || projectID <= 0 {
		apierr.WriteStatus(w, http.StatusBadRequest, "invalid project id")
		return 0, false
	}
	return projectID, true
}

func writeError(ctx context.Context, w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, handoffapp.ErrProjectNotFound):
		apierr.WriteStatus(w, http.StatusNotFound, "project not found")
	case errors.Is(err, handoffapp.ErrProjectSwitchPlatformOff):
		apierr.WriteStatus(w, http.StatusConflict,
			"the cloud executor is off for this deployment, so a project cannot turn it on")
	default:
		slog.ErrorContext(ctx, "project cloud executor switch", "error", err)
		apierr.WriteStatus(w, http.StatusInternalServerError, "could not read or save the cloud executor setting")
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
