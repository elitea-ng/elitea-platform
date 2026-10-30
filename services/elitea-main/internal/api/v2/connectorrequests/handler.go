// Package connectorrequests serves the connector approval queue (ADR-0044
// decision 1, #1501) under /api/v2/elitea_core and /api/v2/admin:
//
//	GET  /connector_catalogue/prompt_lib/{projectID}
//	     the catalogue mode and what the project may use (tools.list)
//	GET  /connector_requests/prompt_lib/{projectID}[/{requestID}]
//	     the project's requests (models.applications.tools.list)
//	POST /connector_requests/prompt_lib/{projectID}
//	     a new request, draft or submitted (models.applications.tools.create)
//	POST /connector_requests/prompt_lib/{projectID}/{requestID}/{submit|withdraw}
//	     the requester's moves (models.applications.tools.create)
//	GET  /admin/connector_requests[/{requestID}]   every request (connector.approve)
//	POST /admin/connector_requests/{requestID}/{approve|reject}            (connector.approve)
//
// The router applies the route permissions. A handler with no store answers
// 503.
package connectorrequests

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/connectorapproval"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// The route patterns: the first five relative to /api/v2/elitea_core, the
// admin ones to /api/v2/admin.
const (
	CataloguePath     = "/connector_catalogue/prompt_lib/{projectID}"
	RequestsPath      = "/connector_requests/prompt_lib/{projectID}"
	RequestPath       = "/connector_requests/prompt_lib/{projectID}/{requestID}"
	SubmitPath        = RequestPath + "/submit"
	WithdrawPath      = RequestPath + "/withdraw"
	AdminRequestsPath = "/connector_requests"
	AdminRequestPath  = "/connector_requests/{requestID}"
	AdminApprovePath  = AdminRequestPath + "/approve"
	AdminRejectPath   = AdminRequestPath + "/reject"
	maxBody           = 512 << 10
)

// Store is *repos.ConnectorRequests.
type Store interface {
	Create(ctx context.Context, projectID, requesterID int64, draft connectorapproval.Draft, submit bool) (connectorapproval.Request, error)
	Get(ctx context.Context, projectID, id int64) (connectorapproval.Request, error)
	List(ctx context.Context, filter repos.ConnectorRequestFilter) ([]connectorapproval.Request, error)
	Submit(ctx context.Context, projectID, id, actorID int64) (connectorapproval.Request, error)
	Withdraw(ctx context.Context, projectID, id, actorID int64, reason string) (connectorapproval.Request, error)
	Approve(ctx context.Context, id, approverID int64, reason string) (repos.ConnectorApprovalOutcome, error)
	Reject(ctx context.Context, id, approverID int64, reason string) (connectorapproval.Request, error)
	UserNames(ctx context.Context, ids []int64) (map[int64]repos.ConnectorRequestUser, error)
}

var _ Store = (*repos.ConnectorRequests)(nil)

// Handler serves the routes.
type Handler struct {
	store Store
	gate  *connectorapproval.Gate
}

// NewHandler builds the handler. A nil store (or a typed nil) answers 503;
// a nil gate answers the open mode.
func NewHandler(store Store, gate *connectorapproval.Gate) *Handler {
	if typed, ok := store.(*repos.ConnectorRequests); ok && typed == nil {
		store = nil
	}
	return &Handler{store: store, gate: gate}
}

// DeclaredTool is one declared tool in a request or a response.
type DeclaredTool struct {
	Name        string `json:"name"`
	Category    string `json:"category"`
	Description string `json:"description,omitempty"`
	InputSchema any    `json:"input_schema,omitempty"`
	Digest      string `json:"digest,omitempty"`
}

// ConnectorRequest is one request in a response.
type ConnectorRequest struct {
	ID             int64          `json:"id"`
	ProjectID      int64          `json:"project_id"`
	TargetKind     string         `json:"target_kind"`
	Target         string         `json:"target"`
	ToolkitType    string         `json:"toolkit_type"`
	PlatformWide   bool           `json:"platform_wide"`
	ProjectIDs     []int64        `json:"project_ids"`
	Tools          []DeclaredTool `json:"tools"`
	EgressHosts    []string       `json:"egress_hosts"`
	DataClasses    []string       `json:"data_classes"`
	Justification  string         `json:"justification"`
	State          string         `json:"state"`
	RequestedBy    int64          `json:"requested_by"`
	RequesterName  *string        `json:"requester_name"`
	RequesterEmail *string        `json:"requester_email"`
	DecidedBy      *int64         `json:"decided_by"`
	DecisionReason string         `json:"decision_reason"`
	AttestationID  *int64         `json:"attestation_id"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
	SubmittedAt    *time.Time     `json:"submitted_at"`
	DecidedAt      *time.Time     `json:"decided_at"`
}

func positive(value int64) *int64 {
	if value <= 0 {
		return nil
	}
	return &value
}

func response(request connectorapproval.Request, users map[int64]repos.ConnectorRequestUser) ConnectorRequest {
	tools := make([]DeclaredTool, 0, len(request.Tools))
	for _, tool := range request.Tools {
		tools = append(tools, DeclaredTool{Name: tool.Name, Category: string(tool.Category),
			Description: tool.Description, InputSchema: tool.InputSchema, Digest: tool.Digest})
	}
	out := ConnectorRequest{
		ID: request.ID, ProjectID: request.ProjectID, TargetKind: string(request.TargetKind), Target: request.Target,
		ToolkitType: request.ToolkitType, PlatformWide: request.PlatformWide, ProjectIDs: request.ProjectIDs,
		Tools: tools, EgressHosts: request.EgressHosts, DataClasses: request.DataClasses,
		Justification: request.Justification, State: string(request.State), RequestedBy: request.RequestedBy,
		DecidedBy: positive(request.DecidedBy), DecisionReason: request.DecisionReason,
		AttestationID: positive(request.AttestationID), CreatedAt: request.CreatedAt, UpdatedAt: request.UpdatedAt,
		SubmittedAt: request.SubmittedAt, DecidedAt: request.DecidedAt,
	}
	if user, ok := users[request.RequestedBy]; ok {
		if user.Name != "" {
			out.RequesterName = &user.Name
		}
		if user.Email != "" {
			out.RequesterEmail = &user.Email
		}
	}
	return out
}

func (h *Handler) responses(ctx context.Context, requests []connectorapproval.Request) []ConnectorRequest {
	ids := make([]int64, 0, len(requests))
	for _, request := range requests {
		ids = append(ids, request.RequestedBy)
	}
	users, err := h.store.UserNames(ctx, ids)
	if err != nil {
		// The names are a convenience; the ids are always there.
		slog.WarnContext(ctx, "connector requests: requester names unavailable", "err", err)
	}
	out := make([]ConnectorRequest, 0, len(requests))
	for _, request := range requests {
		out = append(out, response(request, users))
	}
	return out
}

// createBody is the create request.
type createBody struct {
	TargetKind    string         `json:"target_kind"`
	Target        string         `json:"target"`
	PlatformWide  bool           `json:"platform_wide"`
	ProjectIDs    []int64        `json:"project_ids"`
	Tools         []DeclaredTool `json:"tools"`
	EgressHosts   []string       `json:"egress_hosts"`
	DataClasses   []string       `json:"data_classes"`
	Justification string         `json:"justification"`
	Submit        bool           `json:"submit"`
}

type reasonBody struct {
	Reason string `json:"reason"`
}

// Catalogue answers the project's catalogue mode and what it may use.
func (h *Handler) Catalogue(w http.ResponseWriter, r *http.Request) {
	projectID, ok := pathID(w, r, "projectID")
	if !ok {
		return
	}
	settings, err := h.gate.Settings(r.Context())
	if err != nil {
		writeError(r.Context(), w, connectorapproval.Refuse(connectorapproval.CatalogueUnavailableCode,
			"The connector catalogue mode could not be read."))
		return
	}
	view, err := h.gate.ForProject(r.Context(), projectID, false)
	if err != nil {
		writeError(r.Context(), w, connectorapproval.Refuse(connectorapproval.CatalogueUnavailableCode,
			"The approved connectors could not be read."))
		return
	}
	approved := []map[string]any{}
	add := func(kind connectorapproval.TargetKind, targets map[string]bool) {
		for target := range targets {
			approved = append(approved, map[string]any{"target_kind": string(kind), "target": target})
		}
	}
	add(connectorapproval.TargetToolkitType, view.Grants.ToolkitTypes)
	add(connectorapproval.TargetPrebuiltMCP, view.Grants.Prebuilt)
	for target, pins := range view.Grants.CustomMCP {
		names := make([]string, 0, len(pins))
		for _, pin := range pins {
			names = append(names, pin.Name)
		}
		approved = append(approved, map[string]any{"target_kind": string(connectorapproval.TargetCustomMCP),
			"target": target, "tools": names})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"mode":                             string(view.Mode),
		"platform_mode":                    string(settings.Mode),
		"governed":                         h.gate.Governed(),
		"require_architecture_attestation": settings.RequireArchitectureAttestation,
		"approved":                         approved,
	})
}

// ListProject lists the project's requests.
func (h *Handler) ListProject(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	projectID, ok := pathID(w, r, "projectID")
	if !ok {
		return
	}
	state, ok := stateFilter(w, r)
	if !ok {
		return
	}
	requests, err := h.store.List(r.Context(), repos.ConnectorRequestFilter{ProjectID: projectID, State: state})
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rows": h.responses(r.Context(), requests), "total": len(requests)})
}

// Create stores a request in the project.
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	projectID, ok := pathID(w, r, "projectID")
	if !ok {
		return
	}
	actor, ok := signedIn(w, r)
	if !ok {
		return
	}
	var body createBody
	if !decode(w, r, &body, false) {
		return
	}
	tools := make([]connectorapproval.DeclaredTool, 0, len(body.Tools))
	for _, tool := range body.Tools {
		tools = append(tools, connectorapproval.DeclaredTool{Name: tool.Name,
			Category: connectorapproval.ToolCategory(tool.Category), Description: tool.Description,
			InputSchema: tool.InputSchema})
	}
	created, err := h.store.Create(r.Context(), projectID, actor, connectorapproval.Draft{
		TargetKind: connectorapproval.TargetKind(body.TargetKind), Target: body.Target,
		PlatformWide: body.PlatformWide, ProjectIDs: body.ProjectIDs, Tools: tools,
		EgressHosts: body.EgressHosts, DataClasses: body.DataClasses, Justification: body.Justification,
	}, body.Submit)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	writeJSON(w, http.StatusCreated, h.responses(r.Context(), []connectorapproval.Request{created})[0])
}

// GetProject reads one of the project's requests.
func (h *Handler) GetProject(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	projectID, ok := pathID(w, r, "projectID")
	if !ok {
		return
	}
	id, ok := pathID(w, r, "requestID")
	if !ok {
		return
	}
	request, err := h.store.Get(r.Context(), projectID, id)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	writeJSON(w, http.StatusOK, h.responses(r.Context(), []connectorapproval.Request{request})[0])
}

// Submit submits the requester's draft.
func (h *Handler) Submit(w http.ResponseWriter, r *http.Request) {
	h.requesterMove(w, r, func(ctx context.Context, projectID, id, actor int64, _ string) (connectorapproval.Request, error) {
		return h.store.Submit(ctx, projectID, id, actor)
	}, true)
}

// Withdraw withdraws the requester's draft or submitted request.
func (h *Handler) Withdraw(w http.ResponseWriter, r *http.Request) {
	h.requesterMove(w, r, func(ctx context.Context, projectID, id, actor int64, reason string) (connectorapproval.Request, error) {
		return h.store.Withdraw(ctx, projectID, id, actor, reason)
	}, true)
}

func (h *Handler) requesterMove(w http.ResponseWriter, r *http.Request,
	move func(ctx context.Context, projectID, id, actor int64, reason string) (connectorapproval.Request, error), optional bool,
) {
	if !h.ready(w) {
		return
	}
	projectID, ok := pathID(w, r, "projectID")
	if !ok {
		return
	}
	id, ok := pathID(w, r, "requestID")
	if !ok {
		return
	}
	actor, ok := signedIn(w, r)
	if !ok {
		return
	}
	var body reasonBody
	if !decode(w, r, &body, optional) {
		return
	}
	request, err := move(r.Context(), projectID, id, actor, body.Reason)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	writeJSON(w, http.StatusOK, h.responses(r.Context(), []connectorapproval.Request{request})[0])
}

// AdminList lists every request, optionally of one state or project.
func (h *Handler) AdminList(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	state, ok := stateFilter(w, r)
	if !ok {
		return
	}
	var projectID int64
	if raw := strings.TrimSpace(r.URL.Query().Get("project_id")); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed <= 0 {
			writeError(r.Context(), w, connectorapproval.Refuse(connectorapproval.InvalidCode, "project_id is a positive integer."))
			return
		}
		projectID = parsed
	}
	requests, err := h.store.List(r.Context(), repos.ConnectorRequestFilter{ProjectID: projectID, State: state})
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rows": h.responses(r.Context(), requests), "total": len(requests)})
}

// AdminGet reads any request.
func (h *Handler) AdminGet(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	id, ok := pathID(w, r, "requestID")
	if !ok {
		return
	}
	request, err := h.store.Get(r.Context(), 0, id)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	writeJSON(w, http.StatusOK, h.responses(r.Context(), []connectorapproval.Request{request})[0])
}

// Approve approves a submitted request: the grants and the egress entries
// are written with it.
func (h *Handler) Approve(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	id, ok := pathID(w, r, "requestID")
	if !ok {
		return
	}
	actor, ok := signedIn(w, r)
	if !ok {
		return
	}
	var body reasonBody
	if !decode(w, r, &body, true) {
		return
	}
	outcome, err := h.store.Approve(r.Context(), id, actor, body.Reason)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	granted := outcome.GrantedProjects
	if granted == nil {
		granted = []int64{}
	}
	hosts := outcome.EgressHosts
	if hosts == nil {
		hosts = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"request":          h.responses(r.Context(), []connectorapproval.Request{outcome.Request})[0],
		"granted_projects": granted,
		"egress_hosts":     hosts,
	})
}

// Reject rejects a submitted request with a reason.
func (h *Handler) Reject(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	id, ok := pathID(w, r, "requestID")
	if !ok {
		return
	}
	actor, ok := signedIn(w, r)
	if !ok {
		return
	}
	var body reasonBody
	if !decode(w, r, &body, false) {
		return
	}
	request, err := h.store.Reject(r.Context(), id, actor, body.Reason)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	writeJSON(w, http.StatusOK, h.responses(r.Context(), []connectorapproval.Request{request})[0])
}

func (h *Handler) ready(w http.ResponseWriter) bool {
	if h == nil || h.store == nil {
		apierr.WriteStatus(w, http.StatusServiceUnavailable, "connector requests are not available on this deployment")
		return false
	}
	return true
}

func stateFilter(w http.ResponseWriter, r *http.Request) (connectorapproval.State, bool) {
	raw := strings.TrimSpace(r.URL.Query().Get("state"))
	if raw == "" {
		return "", true
	}
	state := connectorapproval.State(raw)
	if !state.Valid() {
		writeRefusal(w, connectorapproval.Refuse(connectorapproval.InvalidCode,
			"state is draft, submitted, approved, rejected or withdrawn."))
		return "", false
	}
	return state, true
}

func pathID(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	value, err := strconv.ParseInt(chi.URLParam(r, name), 10, 64)
	if err != nil || value <= 0 {
		writeRefusal(w, connectorapproval.Refuse(connectorapproval.InvalidCode, "The path's "+name+" is not a valid id."))
		return 0, false
	}
	return value, true
}

func signedIn(w http.ResponseWriter, r *http.Request) (int64, bool) {
	principal, _ := auth.UserFromContext(r.Context())
	id, ok := principal.OwningUserID()
	if !ok || id <= 0 {
		apierr.WriteStatus(w, http.StatusUnauthorized, "this action needs a signed-in user")
		return 0, false
	}
	return id, true
}

// decode reads a JSON body with no unknown fields, numbers kept as written
// (a declared tool's input schema is digested as written); optional accepts
// an empty body.
func decode(w http.ResponseWriter, r *http.Request, target any, optional bool) bool {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil || len(raw) > maxBody {
		writeRefusal(w, connectorapproval.Refuse(connectorapproval.InvalidCode, "The request body is not readable or too large."))
		return false
	}
	if strings.TrimSpace(string(raw)) == "" {
		if optional {
			return true
		}
		writeRefusal(w, connectorapproval.Refuse(connectorapproval.InvalidCode, "A JSON body is required."))
		return false
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if decoder.Decode(target) != nil {
		writeRefusal(w, connectorapproval.Refuse(connectorapproval.InvalidCode, "The request body is not valid JSON for this route."))
		return false
	}
	return true
}

func writeRefusal(w http.ResponseWriter, refusal *connectorapproval.Refusal) {
	writeJSON(w, refusal.Status(), refusal.Body())
}

func writeError(ctx context.Context, w http.ResponseWriter, err error) {
	if refusal, ok := connectorapproval.AsRefusal(err); ok {
		writeRefusal(w, refusal)
		return
	}
	slog.ErrorContext(ctx, "connector requests", "error", err)
	apierr.WriteStatus(w, http.StatusInternalServerError, "could not read or save the connector request")
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
