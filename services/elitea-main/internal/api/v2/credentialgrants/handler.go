// Package credentialgrants serves the admin API for credential use grants
// (ADR-0039, issue #1116):
//
//	GET  /api/v2/projects/{projectID}/credential_grants
//	POST /api/v2/projects/{projectID}/credential_grants
//	POST /api/v2/projects/{projectID}/credential_grants/{id}:approve|:suspend|:revoke
//
// router.go mounts these only when RouterConfig.CredentialGrantsAdmin is
// set, which the composition root does only with the governance flag on.
// The permission gates live in router.go: they resolve the caller's
// permissions in the {projectID} project, so a non-member is refused before
// this package runs. The project in the path is the grant's tenant project;
// a grant of another project answers 404.
package credentialgrants

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
)

const maxBodyBytes = 16 << 10

// Service is the decision surface the handler calls. *credentialgrant.Service
// implements it.
type Service interface {
	ProposeGrant(ctx context.Context, proposal credentialgrant.Proposal, proposedBy string) (credentialgrant.CredentialUseGrant, error)
	ListGrants(ctx context.Context, filter credentialgrant.ListFilter) ([]credentialgrant.CredentialUseGrant, error)
	TransitionGrant(ctx context.Context, tenantProjectID int64, id string, action credentialgrant.Action, actor string) (credentialgrant.CredentialUseGrant, error)
}

var _ Service = (*credentialgrant.Service)(nil)

// Handler serves the routes.
type Handler struct{ service Service }

// New builds the handler. A nil service is refused.
func New(service Service) (*Handler, error) {
	if service == nil {
		return nil, errors.New("credential grants admin: a service is required")
	}
	return &Handler{service: service}, nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

// writeDomainError maps a typed error to a status. The typed errors'
// messages are safe; anything else is a generic 500.
func writeDomainError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, credentialgrant.ErrInvalidGrant):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, credentialgrant.ErrSelfApproval):
		writeError(w, http.StatusForbidden, err.Error())
	case errors.Is(err, credentialgrant.ErrGrantNotFound):
		writeError(w, http.StatusNotFound, credentialgrant.ErrGrantNotFound.Error())
	case errors.Is(err, credentialgrant.ErrInvalidTransition), errors.Is(err, credentialgrant.ErrDuplicateGrant),
		errors.Is(err, credentialgrant.ErrReferenceNotUsable):
		writeError(w, http.StatusConflict, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "credential grants admin: internal error")
	}
}

// actor is the resolved auth_core user id the permission gate stamped.
func actor(w http.ResponseWriter, r *http.Request) (string, bool) {
	user, ok := auth.UserFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return "", false
	}
	if strings.TrimSpace(user.UserID) == "" {
		writeError(w, http.StatusForbidden, "the caller has no resolved user id")
		return "", false
	}
	return user.UserID, true
}

// projectID reads {projectID}. The gate already resolved it, so a bad value
// cannot reach here through the router; the check keeps the handler safe
// on its own.
func projectID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw := chi.URLParam(r, "projectID")
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 || strconv.FormatInt(id, 10) != raw {
		writeError(w, http.StatusBadRequest, "invalid project id")
		return 0, false
	}
	return id, true
}

// pathID reads {id} without its `:verb` suffix.
func pathID(r *http.Request, verb string) string {
	return strings.TrimSuffix(chi.URLParam(r, "id"), ":"+verb)
}

type grantView struct {
	ID                    string   `json:"id"`
	TenantProjectID       int64    `json:"tenant_project_id"`
	AgentBindingID        string   `json:"agent_binding_id"`
	ToolkitID             int64    `json:"toolkit_id"`
	CredentialReferenceID string   `json:"credential_reference_id"`
	TargetBindingID       string   `json:"target_binding_id"`
	Capabilities          []string `json:"capabilities"`
	OperationScopes       []string `json:"operation_scopes"`
	ResourceScopes        []string `json:"resource_scopes"`
	Obligations           []string `json:"obligations"`
	State                 string   `json:"state"`
	Epoch                 int64    `json:"epoch"`
	ExpiresAt             *string  `json:"expires_at"`
	ProposedBy            string   `json:"proposed_by"`
	ApprovedBy            *string  `json:"approved_by"`
	CreatedAt             string   `json:"created_at"`
	UpdatedAt             string   `json:"updated_at"`
}

func orEmpty(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

func viewGrant(g credentialgrant.CredentialUseGrant) grantView {
	capabilities := make([]string, len(g.Capabilities))
	for i, c := range g.Capabilities {
		capabilities[i] = string(c)
	}
	view := grantView{
		ID: g.ID, TenantProjectID: g.TenantProjectID, AgentBindingID: g.AgentBindingID, ToolkitID: g.ToolkitID,
		CredentialReferenceID: g.CredentialReferenceID, TargetBindingID: g.TargetBindingID,
		Capabilities: capabilities, OperationScopes: orEmpty(g.OperationScopes),
		ResourceScopes: orEmpty(g.ResourceScopes), Obligations: orEmpty(g.Obligations),
		State: string(g.State), Epoch: g.Epoch, ProposedBy: g.ProposedBy,
		CreatedAt: g.CreatedAt.UTC().Format(time.RFC3339Nano), UpdatedAt: g.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
	if g.ExpiresAt != nil {
		expires := g.ExpiresAt.UTC().Format(time.RFC3339Nano)
		view.ExpiresAt = &expires
	}
	if g.ApprovedBy != "" {
		approved := g.ApprovedBy
		view.ApprovedBy = &approved
	}
	return view
}

// List serves GET /projects/{projectID}/credential_grants.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	tenant, ok := projectID(w, r)
	if !ok {
		return
	}
	query := r.URL.Query()
	filter := credentialgrant.ListFilter{
		TenantProjectID: tenant,
		AgentBindingID:  query.Get("agent_binding_id"),
		State:           credentialgrant.GrantState(query.Get("state")),
	}
	parse := func(name string, min, max int) (int, bool) {
		raw := query.Get(name)
		if raw == "" {
			return 0, true
		}
		value, err := strconv.Atoi(raw)
		return value, err == nil && value >= min && value <= max
	}
	var okLimit, okOffset bool
	filter.Limit, okLimit = parse("limit", 1, 200)
	filter.Offset, okOffset = parse("offset", 0, 1<<30)
	if !okLimit || !okOffset || (filter.State != "" && !filter.State.Valid()) {
		writeError(w, http.StatusBadRequest, "invalid state, limit or offset")
		return
	}
	rows, err := h.service.ListGrants(r.Context(), filter)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	views := make([]grantView, 0, len(rows))
	for _, row := range rows {
		views = append(views, viewGrant(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{"rows": views})
}

type grantCreate struct {
	AgentBindingID        string     `json:"agent_binding_id"`
	ToolkitID             int64      `json:"toolkit_id"`
	CredentialReferenceID string     `json:"credential_reference_id"`
	TargetBindingID       string     `json:"target_binding_id"`
	Capabilities          []string   `json:"capabilities"`
	OperationScopes       []string   `json:"operation_scopes"`
	ResourceScopes        []string   `json:"resource_scopes"`
	Obligations           []string   `json:"obligations"`
	ExpiresAt             *time.Time `json:"expires_at"`
}

// Propose serves POST /projects/{projectID}/credential_grants.
func (h *Handler) Propose(w http.ResponseWriter, r *http.Request) {
	tenant, ok := projectID(w, r)
	if !ok {
		return
	}
	who, ok := actor(w, r)
	if !ok {
		return
	}
	var body grantCreate
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil || decoder.More() {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	capabilities := make([]credentialgrant.Capability, len(body.Capabilities))
	for i, c := range body.Capabilities {
		capabilities[i] = credentialgrant.Capability(c)
	}
	created, err := h.service.ProposeGrant(r.Context(), credentialgrant.Proposal{
		TenantProjectID: tenant, AgentBindingID: body.AgentBindingID, ToolkitID: body.ToolkitID,
		CredentialReferenceID: body.CredentialReferenceID, TargetBindingID: body.TargetBindingID,
		Capabilities: capabilities, OperationScopes: body.OperationScopes,
		ResourceScopes: body.ResourceScopes, Obligations: body.Obligations, ExpiresAt: body.ExpiresAt,
	}, who)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, viewGrant(created))
}

func (h *Handler) transition(action credentialgrant.Action) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenant, ok := projectID(w, r)
		if !ok {
			return
		}
		who, ok := actor(w, r)
		if !ok {
			return
		}
		next, err := h.service.TransitionGrant(r.Context(), tenant, pathID(r, string(action)), action, who)
		if err != nil {
			writeDomainError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, viewGrant(next))
	}
}

// Approve serves POST …/credential_grants/{id}:approve.
func (h *Handler) Approve(w http.ResponseWriter, r *http.Request) {
	h.transition(credentialgrant.ActionApprove)(w, r)
}

// Suspend serves POST …/credential_grants/{id}:suspend.
func (h *Handler) Suspend(w http.ResponseWriter, r *http.Request) {
	h.transition(credentialgrant.ActionSuspend)(w, r)
}

// Revoke serves POST …/credential_grants/{id}:revoke.
func (h *Handler) Revoke(w http.ResponseWriter, r *http.Request) {
	h.transition(credentialgrant.ActionRevoke)(w, r)
}
