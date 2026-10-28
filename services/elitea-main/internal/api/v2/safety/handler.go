// Package safety serves the kill switch (ADR-0041 decision 1, issue #1493)
// on two browser-facing surfaces that share one handler:
//
//	GET|POST /api/v2/admin/execution_suspensions
//	POST     /api/v2/admin/execution_suspensions/{suspension_id}:resume
//	         the admin console, every scope; central safety.suspend
//	         (administration mode)
//	GET|POST /api/v2/elitea_core/execution_suspensions/prompt_lib/{projectID}
//	POST     /api/v2/elitea_core/execution_suspensions/prompt_lib/{projectID}/{suspension_id}:resume
//	         the agent page, agent and project scope of that project only;
//	         safety.suspend resolved in the project (the project admin)
//
// The router applies those gates. Every suspend and resume annotates the
// audit record the middleware writes to centry.audit_events
// (safety.suspension.created / safety.suspension.resumed).
package safety

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	safetyapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/safety"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/audit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// The route patterns, relative to their /api/v2/admin and
// /api/v2/elitea_core groups.
const (
	AdminPath         = "/execution_suspensions"
	AdminResumePath   = "/execution_suspensions/{suspension_id}:resume"
	ProjectPath       = "/execution_suspensions/prompt_lib/{projectID}"
	ProjectResumePath = "/execution_suspensions/prompt_lib/{projectID}/{suspension_id}:resume"
)

// Audit actions.
const (
	AuditActionCreated = "safety.suspension.created"
	AuditActionResumed = "safety.suspension.resumed"
)

// Service is *safetyapp.Service.
type Service interface {
	Create(ctx context.Context, actor safetyapp.Actor, request safetyapp.CreateRequest) (safetyapp.Suspension, error)
	Resume(ctx context.Context, actor safetyapp.Actor, request safetyapp.ResumeRequest) (safetyapp.Suspension, error)
	List(ctx context.Context, actor safetyapp.Actor, filter safetyapp.ListFilter) ([]safetyapp.Suspension, error)
}

var _ Service = (*safetyapp.Service)(nil)

// Handler serves both surfaces.
type Handler struct{ service Service }

// NewHandler builds the handler.
func NewHandler(service Service) (*Handler, error) {
	if service == nil {
		return nil, errors.New("the suspension service is required")
	}
	return &Handler{service: service}, nil
}

// Suspension is the wire shape.
type Suspension struct {
	ID             int64      `json:"id"`
	Scope          string     `json:"scope"`
	ProjectID      *int64     `json:"project_id"`
	ApplicationID  *int64     `json:"application_id"`
	ToolkitType    *string    `json:"toolkit_type"`
	Reason         string     `json:"reason"`
	ActorID        int64      `json:"actor_id"`
	Source         string     `json:"source"`
	ResumeRequires string     `json:"resume_requires"`
	CreatedAt      time.Time  `json:"created_at"`
	CancelledRuns  int        `json:"cancelled_runs"`
	Active         bool       `json:"active"`
	ResumedAt      *time.Time `json:"resumed_at"`
	ResumedBy      *int64     `json:"resumed_by"`
	ResumeReason   *string    `json:"resume_reason"`
}

func wire(s safetyapp.Suspension) Suspension {
	out := Suspension{
		ID: s.ID, Scope: string(s.Target.Scope), Reason: s.Reason, ActorID: s.ActorID,
		Source: string(s.Source), ResumeRequires: string(s.ResumeRequires), CreatedAt: s.CreatedAt,
		CancelledRuns: s.CancelledRuns, Active: s.Active(), ResumedAt: s.ResumedAt, ResumedBy: s.ResumedBy,
	}
	switch s.Target.Scope {
	case safetyapp.ScopeProject:
		id := s.Target.ProjectID
		out.ProjectID = &id
	case safetyapp.ScopeAgent:
		project, app := s.Target.ProjectID, s.Target.ApplicationID
		out.ProjectID, out.ApplicationID = &project, &app
	case safetyapp.ScopeToolkitType:
		toolkit := s.Target.ToolkitType
		out.ToolkitType = &toolkit
	}
	if s.ResumeReason != "" {
		reason := s.ResumeReason
		out.ResumeReason = &reason
	}
	return out
}

type createBody struct {
	Scope          string `json:"scope"`
	ProjectID      *int64 `json:"project_id"`
	ApplicationID  *int64 `json:"application_id"`
	ToolkitType    string `json:"toolkit_type"`
	Reason         string `json:"reason"`
	ResumeRequires string `json:"resume_requires"`
	Confirm        string `json:"confirm"`
}

type resumeBody struct {
	Reason string `json:"reason"`
}

// AdminList lists suspensions: ?active=true for the active ones only,
// ?project_id= for one project's.
func (h *Handler) AdminList(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.actor(w, r, 0)
	if !ok {
		return
	}
	filter, ok := listFilter(w, r)
	if !ok {
		return
	}
	if raw := r.URL.Query().Get("project_id"); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 32)
		if err != nil || id <= 0 {
			apierr.WriteStatus(w, http.StatusBadRequest, "invalid project_id")
			return
		}
		filter.ProjectID = id
	}
	h.list(w, r, actor, filter)
}

// ProjectList lists one project's agent and project suspensions.
func (h *Handler) ProjectList(w http.ResponseWriter, r *http.Request) {
	projectID, ok := pathProject(w, r)
	if !ok {
		return
	}
	actor, ok := h.actor(w, r, projectID)
	if !ok {
		return
	}
	filter, ok := listFilter(w, r)
	if !ok {
		return
	}
	h.list(w, r, actor, filter)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request, actor safetyapp.Actor, filter safetyapp.ListFilter) {
	rows, err := h.service.List(r.Context(), actor, filter)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	out := make([]Suspension, 0, len(rows))
	for _, row := range rows {
		out = append(out, wire(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{"suspensions": out})
}

// AdminCreate suspends any scope.
func (h *Handler) AdminCreate(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.actor(w, r, 0)
	if !ok {
		return
	}
	h.create(w, r, actor)
}

// ProjectCreate suspends one agent of the project, or the whole project.
func (h *Handler) ProjectCreate(w http.ResponseWriter, r *http.Request) {
	projectID, ok := pathProject(w, r)
	if !ok {
		return
	}
	actor, ok := h.actor(w, r, projectID)
	if !ok {
		return
	}
	h.create(w, r, actor)
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request, actor safetyapp.Actor) {
	var body createBody
	decoder := json.NewDecoder(io.LimitReader(r.Body, 8<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		apierr.WriteStatus(w, http.StatusBadRequest, "invalid request body")
		return
	}
	target := safetyapp.Target{Scope: safetyapp.Scope(body.Scope), ToolkitType: body.ToolkitType}
	if body.ProjectID != nil {
		target.ProjectID = *body.ProjectID
	}
	if body.ApplicationID != nil {
		target.ApplicationID = *body.ApplicationID
	}
	if !actor.PlatformAdmin {
		// The project surface: the project is the path's.
		if body.ProjectID != nil && *body.ProjectID != actor.ProjectID {
			apierr.WriteStatus(w, http.StatusBadRequest, "project_id must be the project of the path")
			return
		}
		target.ProjectID = actor.ProjectID
	}
	if target.Scope == safetyapp.ScopeAgent && target.ApplicationID <= 0 {
		// The synthetic agent (0, 0) is the self-test's; no caller sets it.
		apierr.WriteStatus(w, http.StatusBadRequest, "an agent suspension needs application_id")
		return
	}
	created, err := h.service.Create(r.Context(), actor, safetyapp.CreateRequest{
		Target: target, Reason: body.Reason,
		ResumeRequires: safetyapp.ResumeRequirement(body.ResumeRequires), Confirmation: body.Confirm,
	})
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	annotate(r.Context(), AuditActionCreated, created)
	writeJSON(w, http.StatusCreated, wire(created))
}

// AdminResume resumes any suspension the resume rule lets the caller.
func (h *Handler) AdminResume(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.actor(w, r, 0)
	if !ok {
		return
	}
	h.resume(w, r, actor)
}

// ProjectResume resumes one of the project's agent or project suspensions.
func (h *Handler) ProjectResume(w http.ResponseWriter, r *http.Request) {
	projectID, ok := pathProject(w, r)
	if !ok {
		return
	}
	actor, ok := h.actor(w, r, projectID)
	if !ok {
		return
	}
	h.resume(w, r, actor)
}

func (h *Handler) resume(w http.ResponseWriter, r *http.Request, actor safetyapp.Actor) {
	id, err := strconv.ParseInt(chi.URLParam(r, "suspension_id"), 10, 64)
	if err != nil || id <= 0 {
		apierr.WriteStatus(w, http.StatusBadRequest, "invalid suspension id")
		return
	}
	var body resumeBody
	if r.ContentLength != 0 {
		decoder := json.NewDecoder(io.LimitReader(r.Body, 4<<10))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			apierr.WriteStatus(w, http.StatusBadRequest, "invalid request body")
			return
		}
	}
	resumed, err := h.service.Resume(r.Context(), actor, safetyapp.ResumeRequest{ID: id, Reason: body.Reason})
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	annotate(r.Context(), AuditActionResumed, resumed)
	writeJSON(w, http.StatusOK, wire(resumed))
}

// actor is the signed-in caller on the surface: projectID 0 is the admin
// surface, a project id the project surface.
func (h *Handler) actor(w http.ResponseWriter, r *http.Request, projectID int64) (safetyapp.Actor, bool) {
	if h == nil || h.service == nil {
		apierr.WriteStatus(w, http.StatusServiceUnavailable, "database unavailable")
		return safetyapp.Actor{}, false
	}
	principal, _ := auth.UserFromContext(r.Context())
	userID, ok := principal.OwningUserID()
	if !ok || userID > math.MaxInt32 {
		apierr.WriteStatus(w, http.StatusUnauthorized, "the change needs a signed-in user")
		return safetyapp.Actor{}, false
	}
	return safetyapp.Actor{UserID: userID, PlatformAdmin: projectID == 0, ProjectID: projectID}, true
}

func pathProject(w http.ResponseWriter, r *http.Request) (int64, bool) {
	projectID, err := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 32)
	if err != nil || projectID <= 0 {
		apierr.WriteStatus(w, http.StatusBadRequest, "invalid project id")
		return 0, false
	}
	return projectID, true
}

func listFilter(w http.ResponseWriter, r *http.Request) (safetyapp.ListFilter, bool) {
	filter := safetyapp.ListFilter{}
	switch r.URL.Query().Get("active") {
	case "", "false":
	case "true":
		filter.ActiveOnly = true
	default:
		apierr.WriteStatus(w, http.StatusBadRequest, "active must be true or false")
		return filter, false
	}
	return filter, true
}

// annotate names the suspension in the audit record of the request.
func annotate(ctx context.Context, action string, s safetyapp.Suspension) {
	id := s.ID
	annotation := audit.Annotation{
		Action:     action,
		EntityType: "execution_suspension",
		EntityID:   &id,
		EntityName: string(s.Target.Scope),
	}
	if key := s.Target.Key(); key != "" {
		annotation.EntityName += ":" + key
	}
	if s.Target.Scope.ProjectScoped() && s.Target.ProjectID > 0 {
		project := s.Target.ProjectID
		annotation.ProjectID = &project
	}
	audit.Annotate(ctx, annotation)
}

func writeError(ctx context.Context, w http.ResponseWriter, err error) {
	var already *safetyapp.AlreadySuspendedError
	switch {
	case errors.As(err, &already):
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":      "an active suspension already covers this target",
			"code":       "already_suspended",
			"suspension": wire(already.Existing),
		})
	case errors.Is(err, safetyapp.ErrInvalidTarget):
		apierr.WriteStatus(w, http.StatusBadRequest, "invalid suspension: check the scope, its key and the reason")
	case errors.Is(err, safetyapp.ErrConfirmationRequired):
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "a platform stop needs \"confirm\": \"STOP\"",
			"code":  "confirmation_required",
		})
	case errors.Is(err, safetyapp.ErrScopeForbidden):
		apierr.WriteStatus(w, http.StatusForbidden, "this surface may not suspend that scope")
	case errors.Is(err, safetyapp.ErrResumeForbidden):
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error": "the suspension's resume rule does not let this caller resume it",
			"code":  "resume_forbidden",
		})
	case errors.Is(err, safetyapp.ErrNotFound):
		apierr.WriteStatus(w, http.StatusNotFound, "suspension not found")
	case errors.Is(err, safetyapp.ErrNotActive):
		apierr.WriteStatus(w, http.StatusConflict, "the suspension was already resumed")
	default:
		slog.ErrorContext(ctx, "execution suspension", "error", err)
		apierr.WriteStatus(w, http.StatusInternalServerError, "could not read or save the suspension")
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
