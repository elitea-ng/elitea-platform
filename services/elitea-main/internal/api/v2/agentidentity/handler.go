// Package agentidentity serves the admin API for agent identity bindings and
// mandates (ADR-0039, issue #1112):
//
//	GET  /api/v2/admin/agent_identities
//	POST /api/v2/admin/agent_identities
//	POST /api/v2/admin/agent_identities/{id}:approve|:suspend|:revoke
//	GET  /api/v2/admin/mandates
//	POST /api/v2/admin/mandates
//	POST /api/v2/admin/mandates/{id}:approve|:suspend|:revoke
//
// router.go mounts these only when RouterConfig.AgentIdentityAdmin is set,
// which the composition root does only with the governance flag on. With it
// unset the paths answer like any unknown route. The permission gates live in
// router.go (central administration-mode permissions); this package assumes
// they already passed.
package agentidentity

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentidentity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentidentity/governance"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/mandate"
)

// Permissions this surface is gated on (seeded by shared 0181).
const (
	PermissionBindingsManage  = "models.agent_identity.bindings.manage"
	PermissionBindingsApprove = "models.agent_identity.bindings.approve"
	PermissionMandatesManage  = "models.agent_identity.mandates.manage"
	PermissionMandatesApprove = "models.agent_identity.mandates.approve"
)

const maxBodyBytes = 16 << 10

// Service is the decision surface the handler calls. *governance.Service
// implements it.
type Service interface {
	ProposeBinding(ctx context.Context, proposal agentidentity.Proposal, proposedBy string) (agentidentity.AgentIdentityBinding, error)
	ListBindings(ctx context.Context, filter agentidentity.ListFilter) ([]agentidentity.AgentIdentityBinding, error)
	TransitionBinding(ctx context.Context, id string, action agentidentity.Action, actor string) (agentidentity.AgentIdentityBinding, error)
	IssueMandate(ctx context.Context, req mandate.Request, issuedBy string) (mandate.Mandate, error)
	ListMandates(ctx context.Context, filter mandate.ListFilter) ([]mandate.Mandate, error)
	TransitionMandate(ctx context.Context, id string, action mandate.Action, actor string) (mandate.Mandate, error)
}

var _ Service = (*governance.Service)(nil)

// Handler serves the routes.
type Handler struct {
	service Service
	now     func() time.Time
}

// New builds the handler. A nil service is refused.
func New(service Service) (*Handler, error) {
	if service == nil {
		return nil, errors.New("agent identity admin: a service is required")
	}
	return &Handler{service: service, now: func() time.Time { return time.Now().UTC() }}, nil
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

// writeDomainError maps a typed error to a status. Messages of the typed
// errors are safe by construction; anything else is a generic 500.
func writeDomainError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, agentidentity.ErrInvalidBinding), errors.Is(err, mandate.ErrInvalidMandate),
		errors.Is(err, mandate.ErrExpiresAtRequired), errors.Is(err, mandate.ErrTTLExceedsPolicy):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, agentidentity.ErrIdentityModeNotAllowed), errors.Is(err, agentidentity.ErrSelfApproval),
		errors.Is(err, mandate.ErrSelfApproval):
		writeError(w, http.StatusForbidden, err.Error())
	case errors.Is(err, agentidentity.ErrBindingNotFound), errors.Is(err, mandate.ErrMandateNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, agentidentity.ErrInvalidTransition), errors.Is(err, agentidentity.ErrDuplicateBinding),
		errors.Is(err, agentidentity.ErrExternalIdentityNotActive), errors.Is(err, mandate.ErrInvalidTransition),
		errors.Is(err, mandate.ErrBindingNotUsable):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, governance.ErrPolicyUnavailable):
		writeError(w, http.StatusServiceUnavailable, governance.ErrPolicyUnavailable.Error())
	default:
		writeError(w, http.StatusInternalServerError, "agent identity admin: internal error")
	}
}

// actor is the resolved auth_core user id the permission gate stamped.
func actor(r *http.Request) (string, bool) {
	user, ok := auth.UserFromContext(r.Context())
	if !ok {
		return "", false
	}
	if user.UserID != "" {
		return user.UserID, true
	}
	return user.ID, user.ID != ""
}

func decode(w http.ResponseWriter, r *http.Request, into any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil || decoder.More() {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return false
	}
	return true
}

type listWindow struct {
	tenantProjectID int64
	state           string
	limit, offset   int
}

func parseListWindow(r *http.Request) (listWindow, bool) {
	query := r.URL.Query()
	var out listWindow
	parseInt := func(name string, min int64) (int64, bool) {
		raw := query.Get(name)
		if raw == "" {
			return 0, true
		}
		value, err := strconv.ParseInt(raw, 10, 64)
		return value, err == nil && value >= min
	}
	var ok bool
	if out.tenantProjectID, ok = parseInt("tenant_project_id", 1); !ok {
		return listWindow{}, false
	}
	limit, ok := parseInt("limit", 1)
	if !ok || limit > 200 {
		return listWindow{}, false
	}
	offset, ok := parseInt("offset", 0)
	if !ok {
		return listWindow{}, false
	}
	out.limit, out.offset, out.state = int(limit), int(offset), query.Get("state")
	return out, true
}

// pathID reads {id} without its `:verb` suffix.
func pathID(r *http.Request, verb string) string {
	return strings.TrimSuffix(chi.URLParam(r, "id"), ":"+verb)
}

// ---- bindings ----

type bindingView struct {
	ID                      string  `json:"id"`
	TenantProjectID         int64   `json:"tenant_project_id"`
	ApplicationID           int64   `json:"application_id"`
	ApplicationVersionID    int64   `json:"application_version_id"`
	DefinitionDigest        string  `json:"definition_digest"`
	Mode                    string  `json:"mode"`
	ExternalAgentIdentityID *string `json:"external_agent_identity_id"`
	WorkloadIdentity        string  `json:"workload_identity"`
	State                   string  `json:"state"`
	RevocationEpoch         int64   `json:"revocation_epoch"`
	PolicyRevisionID        string  `json:"policy_revision_id"`
	ProposedBy              string  `json:"proposed_by"`
	ApprovedBy              *string `json:"approved_by"`
	CreatedAt               string  `json:"created_at"`
	UpdatedAt               string  `json:"updated_at"`
}

func optional(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func viewBinding(b agentidentity.AgentIdentityBinding) bindingView {
	return bindingView{
		ID: b.ID, TenantProjectID: b.TenantProjectID, ApplicationID: b.ApplicationID,
		ApplicationVersionID: b.ApplicationVersionID, DefinitionDigest: hex.EncodeToString(b.DefinitionDigest[:]),
		Mode: string(b.Mode), ExternalAgentIdentityID: optional(b.ExternalAgentIdentityID),
		WorkloadIdentity: b.WorkloadIdentity, State: string(b.State), RevocationEpoch: b.RevocationEpoch,
		PolicyRevisionID: b.PolicyRevisionID, ProposedBy: b.ProposedBy, ApprovedBy: optional(b.ApprovedBy),
		CreatedAt: b.CreatedAt.UTC().Format(time.RFC3339Nano), UpdatedAt: b.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
}

// ListBindings serves GET /admin/agent_identities.
func (h *Handler) ListBindings(w http.ResponseWriter, r *http.Request) {
	window, ok := parseListWindow(r)
	state := agentidentity.BindingState(window.state)
	if !ok || (state != "" && !state.Valid()) {
		writeError(w, http.StatusBadRequest, "invalid tenant_project_id, state, limit or offset")
		return
	}
	rows, err := h.service.ListBindings(r.Context(), agentidentity.ListFilter{
		TenantProjectID: window.tenantProjectID, State: state, Limit: window.limit, Offset: window.offset,
	})
	if err != nil {
		writeDomainError(w, err)
		return
	}
	views := make([]bindingView, 0, len(rows))
	for _, row := range rows {
		views = append(views, viewBinding(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{"rows": views})
}

type bindingCreate struct {
	TenantProjectID         int64  `json:"tenant_project_id"`
	ApplicationID           int64  `json:"application_id"`
	ApplicationVersionID    int64  `json:"application_version_id"`
	DefinitionDigest        string `json:"definition_digest"`
	Mode                    string `json:"mode"`
	ExternalAgentIdentityID string `json:"external_agent_identity_id"`
	WorkloadIdentity        string `json:"workload_identity"`
}

// CreateBinding serves POST /admin/agent_identities.
func (h *Handler) CreateBinding(w http.ResponseWriter, r *http.Request) {
	who, ok := actor(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var body bindingCreate
	if !decode(w, r, &body) {
		return
	}
	raw, err := hex.DecodeString(body.DefinitionDigest)
	if err != nil || len(raw) != 32 {
		writeError(w, http.StatusBadRequest, "definition_digest must be 64 hex characters")
		return
	}
	proposal := agentidentity.Proposal{
		TenantProjectID: body.TenantProjectID, ApplicationID: body.ApplicationID,
		ApplicationVersionID: body.ApplicationVersionID, Mode: agentidentity.IdentityMode(body.Mode),
		ExternalAgentIdentityID: body.ExternalAgentIdentityID, WorkloadIdentity: body.WorkloadIdentity,
	}
	copy(proposal.DefinitionDigest[:], raw)
	created, err := h.service.ProposeBinding(r.Context(), proposal, who)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, viewBinding(created))
}

func (h *Handler) transitionBinding(action agentidentity.Action) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		who, ok := actor(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		next, err := h.service.TransitionBinding(r.Context(), pathID(r, string(action)), action, who)
		if err != nil {
			writeDomainError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, viewBinding(next))
	}
}

// ApproveBinding serves POST /admin/agent_identities/{id}:approve.
func (h *Handler) ApproveBinding(w http.ResponseWriter, r *http.Request) {
	h.transitionBinding(agentidentity.ActionApprove)(w, r)
}

// SuspendBinding serves POST /admin/agent_identities/{id}:suspend.
func (h *Handler) SuspendBinding(w http.ResponseWriter, r *http.Request) {
	h.transitionBinding(agentidentity.ActionSuspend)(w, r)
}

// RevokeBinding serves POST /admin/agent_identities/{id}:revoke.
func (h *Handler) RevokeBinding(w http.ResponseWriter, r *http.Request) {
	h.transitionBinding(agentidentity.ActionRevoke)(w, r)
}

// ---- mandates ----

type mandateView struct {
	ID               string   `json:"id"`
	TenantProjectID  int64    `json:"tenant_project_id"`
	Kind             string   `json:"kind"`
	AgentBindingID   string   `json:"agent_binding_id"`
	SubjectUserID    *int64   `json:"subject_user_id"`
	DelegationID     *string  `json:"delegation_id"`
	OperationScopes  []string `json:"operation_scopes"`
	ResourceScopes   []string `json:"resource_scopes"`
	NotBefore        string   `json:"not_before"`
	ExpiresAt        string   `json:"expires_at"`
	State            string   `json:"state"`
	RevocationEpoch  int64    `json:"revocation_epoch"`
	PolicyRevisionID string   `json:"policy_revision_id"`
	IssuedBy         string   `json:"issued_by"`
	ApprovedBy       *string  `json:"approved_by"`
	CreatedAt        string   `json:"created_at"`
	UpdatedAt        string   `json:"updated_at"`
}

func (h *Handler) viewMandate(m mandate.Mandate) mandateView {
	resources := m.ResourceScopes
	if resources == nil {
		resources = []string{}
	}
	return mandateView{
		ID: m.ID, TenantProjectID: m.TenantProjectID, Kind: string(m.Kind), AgentBindingID: m.AgentBindingID,
		SubjectUserID: m.SubjectUserID, DelegationID: m.DelegationID, OperationScopes: m.OperationScopes,
		ResourceScopes: resources, NotBefore: m.NotBefore.UTC().Format(time.RFC3339Nano),
		ExpiresAt: m.ExpiresAt.UTC().Format(time.RFC3339Nano), State: string(m.EffectiveState(h.now())),
		RevocationEpoch: m.RevocationEpoch, PolicyRevisionID: m.PolicyRevisionID, IssuedBy: m.IssuedBy,
		ApprovedBy: optional(m.ApprovedBy), CreatedAt: m.CreatedAt.UTC().Format(time.RFC3339Nano),
		UpdatedAt: m.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
}

// ListMandates serves GET /admin/mandates.
func (h *Handler) ListMandates(w http.ResponseWriter, r *http.Request) {
	window, ok := parseListWindow(r)
	state := mandate.State(window.state)
	if !ok || (state != "" && !state.Valid()) {
		writeError(w, http.StatusBadRequest, "invalid tenant_project_id, state, limit or offset")
		return
	}
	rows, err := h.service.ListMandates(r.Context(), mandate.ListFilter{
		TenantProjectID: window.tenantProjectID, AgentBindingID: r.URL.Query().Get("agent_binding_id"),
		State: state, Limit: window.limit, Offset: window.offset,
	})
	if err != nil {
		writeDomainError(w, err)
		return
	}
	views := make([]mandateView, 0, len(rows))
	for _, row := range rows {
		views = append(views, h.viewMandate(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{"rows": views})
}

type mandateCreate struct {
	TenantProjectID int64      `json:"tenant_project_id"`
	Kind            string     `json:"kind"`
	AgentBindingID  string     `json:"agent_binding_id"`
	SubjectUserID   *int64     `json:"subject_user_id"`
	DelegationID    *string    `json:"delegation_id"`
	OperationScopes []string   `json:"operation_scopes"`
	ResourceScopes  []string   `json:"resource_scopes"`
	NotBefore       *time.Time `json:"not_before"`
	ExpiresAt       *time.Time `json:"expires_at"`
}

// CreateMandate serves POST /admin/mandates.
func (h *Handler) CreateMandate(w http.ResponseWriter, r *http.Request) {
	who, ok := actor(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var body mandateCreate
	if !decode(w, r, &body) {
		return
	}
	req := mandate.Request{
		TenantProjectID: body.TenantProjectID, Kind: mandate.Kind(body.Kind), AgentBindingID: body.AgentBindingID,
		SubjectUserID: body.SubjectUserID, DelegationID: body.DelegationID,
		OperationScopes: body.OperationScopes, ResourceScopes: body.ResourceScopes,
	}
	if body.NotBefore != nil {
		req.NotBefore = *body.NotBefore
	}
	if body.ExpiresAt != nil {
		req.ExpiresAt = *body.ExpiresAt
	}
	created, err := h.service.IssueMandate(r.Context(), req, who)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, h.viewMandate(created))
}

func (h *Handler) transitionMandate(action mandate.Action) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		who, ok := actor(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		next, err := h.service.TransitionMandate(r.Context(), pathID(r, string(action)), action, who)
		if err != nil {
			writeDomainError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, h.viewMandate(next))
	}
}

// ApproveMandate serves POST /admin/mandates/{id}:approve.
func (h *Handler) ApproveMandate(w http.ResponseWriter, r *http.Request) {
	h.transitionMandate(mandate.ActionApprove)(w, r)
}

// SuspendMandate serves POST /admin/mandates/{id}:suspend.
func (h *Handler) SuspendMandate(w http.ResponseWriter, r *http.Request) {
	h.transitionMandate(mandate.ActionSuspend)(w, r)
}

// RevokeMandate serves POST /admin/mandates/{id}:revoke.
func (h *Handler) RevokeMandate(w http.ResponseWriter, r *http.Request) {
	h.transitionMandate(mandate.ActionRevoke)(w, r)
}
