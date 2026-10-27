package agentidentity

// The external agent identity admin API (ADR-0039 decision 11, issue #1151):
//
//	GET  /api/v2/admin/external_agent_identities
//	POST /api/v2/admin/external_agent_identities
//	POST /api/v2/admin/external_agent_identities/{id}:provision|:activate|:suspend|:resume|:retire
//	GET  /api/v2/admin/external_agent_identities/drift   (the reconcile loop's open findings, #1154)
//
// router.go mounts these only when RouterConfig.ExternalAgentIdentityAdmin is
// set, which the composition root does only with the governance flag on.

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/agentidentity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/agentidentity/reconcile"
	domain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentidentity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentidentity/governance"
)

// PermissionExternalManage gates every external identity route (shared
// 0186). Activation also needs PermissionBindingsApprove.
const PermissionExternalManage = "models.agent_identity.external.manage"

// ExternalService is the decision surface the handler calls.
// *agentidentity.ExternalIdentityService implements it.
type ExternalService interface {
	Create(ctx context.Context, req domain.ExternalIdentityRequest, actor string) (domain.ExternalAgentIdentity, error)
	List(ctx context.Context, filter domain.ExternalListFilter) ([]domain.ExternalAgentIdentity, error)
	Provision(ctx context.Context, id, actor string) (domain.ExternalAgentIdentity, error)
	Activate(ctx context.Context, id, actor string) (domain.ExternalAgentIdentity, error)
	Suspend(ctx context.Context, id, actor string) (domain.ExternalAgentIdentity, error)
	Resume(ctx context.Context, id, actor string) (domain.ExternalAgentIdentity, error)
	Retire(ctx context.Context, id, actor string) (domain.ExternalAgentIdentity, error)
}

var _ ExternalService = (*agentidentity.ExternalIdentityService)(nil)

// DriftReader is what the drift route reads. *repos.AgentIdentityReconcile
// implements it.
type DriftReader interface {
	OpenFindings(ctx context.Context, provider string, limit, offset int) ([]reconcile.Finding, error)
	Health(ctx context.Context) ([]reconcile.ProviderHealth, error)
}

// ExternalHandler serves the external identity routes.
type ExternalHandler struct {
	service ExternalService
	drift   DriftReader
}

// NewExternal builds the handler. A nil service is refused. drift is
// optional: without it the drift route answers no findings, which is true
// of a deployment where no reconcile store exists.
func NewExternal(service ExternalService, drift ...DriftReader) (*ExternalHandler, error) {
	if service == nil {
		return nil, errors.New("external agent identity admin: a service is required")
	}
	h := &ExternalHandler{service: service}
	if len(drift) > 1 {
		return nil, errors.New("external agent identity admin: at most one drift reader")
	}
	if len(drift) == 1 {
		h.drift = drift[0]
	}
	return h, nil
}

type externalView struct {
	ID                string  `json:"id"`
	TenantProjectID   int64   `json:"tenant_project_id"`
	ApplicationID     int64   `json:"application_id"`
	Mode              string  `json:"mode"`
	Provider          string  `json:"provider"`
	ProviderTenant    string  `json:"provider_tenant"`
	Kind              *string `json:"kind"`
	ExternalID        *string `json:"external_id"`
	Generation        int64   `json:"generation"`
	ProviderConfigKey string  `json:"provider_config_key"`
	AgentUserRef      *string `json:"agent_user_ref"`
	OwnerRef          string  `json:"owner_ref"`
	State             string  `json:"state"`
	RevocationEpoch   int64   `json:"revocation_epoch"`
	CreatedBy         string  `json:"created_by"`
	CreatedAt         string  `json:"created_at"`
	UpdatedAt         string  `json:"updated_at"`
}

func viewExternal(e domain.ExternalAgentIdentity) externalView {
	return externalView{
		ID: e.ID, TenantProjectID: e.TenantProjectID, ApplicationID: e.ApplicationID, Mode: string(e.Mode),
		Provider: e.Ref.Provider, ProviderTenant: e.Ref.ProviderTenant, Kind: optional(e.Ref.Kind),
		ExternalID: optional(e.Ref.ID), Generation: e.Ref.Generation, ProviderConfigKey: e.ProviderConfigKey,
		AgentUserRef: optional(e.AgentUserRef), OwnerRef: e.OwnerRef, State: string(e.State),
		RevocationEpoch: e.RevocationEpoch, CreatedBy: e.CreatedBy,
		CreatedAt: e.CreatedAt.UTC().Format(time.RFC3339Nano), UpdatedAt: e.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
}

// writeExternalError maps a typed error to a status. A provider failure
// after a stop (suspend, retire) is reported with the state it left.
func writeExternalError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrInvalidExternalIdentity):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, domain.ErrIdentityModeNotAllowed):
		writeError(w, http.StatusForbidden, err.Error())
	case errors.Is(err, domain.ErrExternalIdentityNotFound):
		writeError(w, http.StatusNotFound, domain.ErrExternalIdentityNotFound.Error())
	case errors.Is(err, domain.ErrExternalTransition), errors.Is(err, domain.ErrDuplicateExternalIdentity),
		errors.Is(err, domain.ErrNoApprovedBinding):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, agentidentity.ErrProviderNotConfigured):
		writeError(w, http.StatusServiceUnavailable, agentidentity.ErrProviderNotConfigured.Error())
	case errors.Is(err, agentidentity.ErrProviderCall):
		writeError(w, http.StatusBadGateway, err.Error())
	case errors.Is(err, governance.ErrPolicyUnavailable):
		writeError(w, http.StatusServiceUnavailable, governance.ErrPolicyUnavailable.Error())
	default:
		writeError(w, http.StatusInternalServerError, "external agent identity admin: internal error")
	}
}

// ListExternalIdentities serves GET /admin/external_agent_identities.
func (h *ExternalHandler) ListExternalIdentities(w http.ResponseWriter, r *http.Request) {
	window, ok := parseListWindow(r)
	state := domain.ExternalIdentityState(window.state)
	var application int64
	if raw := r.URL.Query().Get("application_id"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		ok = ok && err == nil && parsed > 0
		application = parsed
	}
	if !ok || (state != "" && !state.Valid()) {
		writeError(w, http.StatusBadRequest, "invalid tenant_project_id, application_id, state, limit or offset")
		return
	}
	rows, err := h.service.List(r.Context(), domain.ExternalListFilter{
		TenantProjectID: window.tenantProjectID, ApplicationID: application, State: state,
		Limit: window.limit, Offset: window.offset,
	})
	if err != nil {
		writeExternalError(w, err)
		return
	}
	views := make([]externalView, 0, len(rows))
	for _, row := range rows {
		views = append(views, viewExternal(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{"rows": views})
}

type externalCreate struct {
	TenantProjectID   int64  `json:"tenant_project_id"`
	ApplicationID     int64  `json:"application_id"`
	Mode              string `json:"mode"`
	Provider          string `json:"provider"`
	ProviderTenant    string `json:"provider_tenant"`
	ProviderConfigKey string `json:"provider_config_key"`
	OwnerRef          string `json:"owner_ref"`
}

// CreateExternalIdentity serves POST /admin/external_agent_identities.
func (h *ExternalHandler) CreateExternalIdentity(w http.ResponseWriter, r *http.Request) {
	who, ok := actor(w, r)
	if !ok {
		return
	}
	var body externalCreate
	if !decode(w, r, &body) {
		return
	}
	created, err := h.service.Create(r.Context(), domain.ExternalIdentityRequest{
		TenantProjectID: body.TenantProjectID, ApplicationID: body.ApplicationID, Mode: domain.IdentityMode(body.Mode),
		Provider: body.Provider, ProviderTenant: body.ProviderTenant, ProviderConfigKey: body.ProviderConfigKey,
		OwnerRef: body.OwnerRef,
	}, who)
	if err != nil {
		writeExternalError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, viewExternal(created))
}

func (h *ExternalHandler) transition(verb string, run func(context.Context, string, string) (domain.ExternalAgentIdentity, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		who, ok := actor(w, r)
		if !ok {
			return
		}
		next, err := run(r.Context(), pathID(r, verb), who)
		if err != nil {
			writeExternalError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, viewExternal(next))
	}
}

// ProvisionExternalIdentity serves POST /admin/external_agent_identities/{id}:provision.
func (h *ExternalHandler) ProvisionExternalIdentity(w http.ResponseWriter, r *http.Request) {
	h.transition("provision", h.service.Provision)(w, r)
}

// ActivateExternalIdentity serves POST /admin/external_agent_identities/{id}:activate.
func (h *ExternalHandler) ActivateExternalIdentity(w http.ResponseWriter, r *http.Request) {
	h.transition("activate", h.service.Activate)(w, r)
}

// SuspendExternalIdentity serves POST /admin/external_agent_identities/{id}:suspend.
func (h *ExternalHandler) SuspendExternalIdentity(w http.ResponseWriter, r *http.Request) {
	h.transition("suspend", h.service.Suspend)(w, r)
}

// ResumeExternalIdentity serves POST /admin/external_agent_identities/{id}:resume.
func (h *ExternalHandler) ResumeExternalIdentity(w http.ResponseWriter, r *http.Request) {
	h.transition("resume", h.service.Resume)(w, r)
}

// RetireExternalIdentity serves POST /admin/external_agent_identities/{id}:retire.
func (h *ExternalHandler) RetireExternalIdentity(w http.ResponseWriter, r *http.Request) {
	h.transition("retire", h.service.Retire)(w, r)
}

type driftFindingView struct {
	ID                      string  `json:"id"`
	Kind                    string  `json:"kind"`
	Provider                string  `json:"provider"`
	ProviderTenant          string  `json:"provider_tenant"`
	ExternalID              string  `json:"external_id"`
	ExternalAgentIdentityID *string `json:"external_agent_identity_id"`
	TenantProjectID         *int64  `json:"tenant_project_id"`
	ApplicationID           *int64  `json:"application_id"`
	RevocationEpoch         int64   `json:"revocation_epoch"`
	DetectedAt              string  `json:"detected_at"`
	LastSeenAt              string  `json:"last_seen_at"`
}

type providerHealthView struct {
	Provider            string  `json:"provider"`
	ConsecutiveFailures int     `json:"consecutive_failures"`
	AdmissionDenied     bool    `json:"admission_denied"`
	LastPassAt          *string `json:"last_pass_at"`
	LastSuccessAt       *string `json:"last_success_at"`
}

func optionalTime(t time.Time) *string {
	if t.IsZero() {
		return nil
	}
	formatted := t.UTC().Format(time.RFC3339Nano)
	return &formatted
}

func optionalID(v int64) *int64 {
	if v == 0 {
		return nil
	}
	return &v
}

// ListDriftFindings serves GET /admin/external_agent_identities/drift: the
// open drift findings, newest first, and every reconciled provider's
// health (an admission_denied provider opens no new agent session).
func (h *ExternalHandler) ListDriftFindings(w http.ResponseWriter, r *http.Request) {
	window, ok := parseListWindow(r)
	provider := r.URL.Query().Get("provider")
	if !ok || window.tenantProjectID != 0 || window.state != "" || (provider != "" && !domain.ValidProviderName(provider)) {
		writeError(w, http.StatusBadRequest, "invalid provider, limit or offset")
		return
	}
	findings := []driftFindingView{}
	providers := []providerHealthView{}
	if h.drift != nil {
		rows, err := h.drift.OpenFindings(r.Context(), provider, window.limit, window.offset)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "external agent identity admin: internal error")
			return
		}
		for _, f := range rows {
			findings = append(findings, driftFindingView{
				ID: f.ID, Kind: string(f.Kind), Provider: f.Provider, ProviderTenant: f.ProviderTenant,
				ExternalID: f.ExternalID, ExternalAgentIdentityID: optional(f.ExternalAgentIdentityID),
				TenantProjectID: optionalID(f.TenantProjectID), ApplicationID: optionalID(f.ApplicationID),
				RevocationEpoch: f.RevocationEpoch,
				DetectedAt:      f.DetectedAt.UTC().Format(time.RFC3339Nano), LastSeenAt: f.LastSeenAt.UTC().Format(time.RFC3339Nano),
			})
		}
		health, err := h.drift.Health(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "external agent identity admin: internal error")
			return
		}
		for _, p := range health {
			if provider != "" && p.Provider != provider {
				continue
			}
			providers = append(providers, providerHealthView{Provider: p.Provider, ConsecutiveFailures: p.ConsecutiveFailures,
				AdmissionDenied: p.AdmissionDenied, LastPassAt: optionalTime(p.LastPassAt), LastSuccessAt: optionalTime(p.LastSuccessAt)})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"rows": findings, "providers": providers})
}
