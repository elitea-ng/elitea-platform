// Package credentialreferences serves the credential reference admin API
// and the agent toolkit bindings (ADR-0039, issue #1124):
//
//	GET    /api/v2/admin/credential_references?project_id=N
//	POST   /api/v2/admin/credential_references                 {"project_id", "provider_type", "target_identity", "class", "backend", "locator", "generation"}
//	GET    /api/v2/admin/credential_references/{id}
//	POST   /api/v2/admin/credential_references/{id}:activate   {}
//	POST   /api/v2/admin/credential_references/{id}:retire     {}
//	POST   /api/v2/admin/credential_references/{id}:attach     {"project_id", "toolkit_id"}
//	POST   /api/v2/admin/credential_references/{id}:detach     {"project_id", "toolkit_id"}
//	PUT    /api/v2/admin/agent_identities/{id}/toolkits/{toolkit_id}  {"project_id"}
//	DELETE /api/v2/admin/agent_identities/{id}/toolkits/{toolkit_id}  {"project_id"}
//
// router.go mounts these only when RouterConfig.CredentialReferencesAdmin is
// set, which the composition root does only with credential governance on.
// The permission gates live in router.go; this package assumes they passed.
// The actor is always the authenticated caller, never a body field.
package credentialreferences

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

const maxBodyBytes = 4 << 10

// Store is the credential reference store (*repos.CredentialReferences).
type Store interface {
	Get(ctx context.Context, id string) (repos.CredentialReferenceRecord, error)
	List(ctx context.Context, filter repos.CredentialReferenceFilter) ([]repos.CredentialReferenceRecord, error)
	Transition(ctx context.Context, id string, action repos.CredentialReferenceAction, actor string) (repos.CredentialReferenceRecord, error)
	Attach(ctx context.Context, referenceID string, target repos.ToolkitAttachment, actor string) (repos.ToolkitAttachmentResult, error)
	Detach(ctx context.Context, referenceID string, target repos.ToolkitAttachment, actor string) (repos.ToolkitAttachmentResult, error)
	PinToolkit(ctx context.Context, agentBindingID string, target repos.ToolkitAttachment, actor string) (repos.AgentToolkitPin, error)
	UnpinToolkit(ctx context.Context, agentBindingID string, target repos.ToolkitAttachment, actor string) error
	StageReference(ctx context.Context, staged repos.StagedReference, newID func() string) (repos.StageResult, error)
}

var _ Store = (*repos.CredentialReferences)(nil)

// Handler serves the routes.
type Handler struct {
	store Store
	// locators and providers serve Create (WithCreate); ids is a test seam
	// for new ids.
	locators  Locators
	providers Providers
	ids       func() string
}

// New builds the handler. A nil store is refused.
func New(store Store) (*Handler, error) {
	if store == nil {
		return nil, errors.New("credential references admin: a store is required")
	}
	return &Handler{store: store}, nil
}

type referenceView struct {
	ID               string  `json:"id"`
	TenantProjectID  int64   `json:"tenant_project_id"`
	Class            string  `json:"class"`
	Backend          string  `json:"backend"`
	Locator          string  `json:"locator"`
	ProviderType     string  `json:"provider_type"`
	TargetBindingID  string  `json:"target_binding_id"`
	TargetIdentity   string  `json:"target_identity"`
	Generation       int64   `json:"generation"`
	State            string  `json:"state"`
	StagedBy         *string `json:"staged_by"`
	ActivatedBy      *string `json:"activated_by"`
	LastTransitionBy *string `json:"last_transition_by"`
	MigratedFrom     *string `json:"migrated_from"`
	CreatedAt        string  `json:"created_at"`
	UpdatedAt        string  `json:"updated_at"`
}

func optional(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func view(r repos.CredentialReferenceRecord) referenceView {
	return referenceView{
		ID: r.ID, TenantProjectID: r.TenantProjectID, Class: string(r.Class), Backend: r.Backend,
		Locator: r.Locator, ProviderType: r.ProviderType, TargetBindingID: r.TargetBindingID,
		TargetIdentity: r.TargetIdentity, Generation: r.Generation, State: string(r.State),
		StagedBy: optional(r.StagedBy), ActivatedBy: optional(r.ActivatedBy),
		LastTransitionBy: optional(r.LastTransitionBy), MigratedFrom: optional(r.MigratedFrom),
		CreatedAt: r.CreatedAt.UTC().Format(time.RFC3339Nano), UpdatedAt: r.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
}

// List serves GET /admin/credential_references.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	projectID, err := strconv.ParseInt(query.Get("project_id"), 10, 64)
	if err != nil || projectID <= 0 {
		writeError(w, http.StatusBadRequest, "project_id is required")
		return
	}
	filter := repos.CredentialReferenceFilter{TenantProjectID: projectID, State: credentialgrant.ReferenceState(query.Get("state"))}
	switch filter.State {
	case "", credentialgrant.ReferenceStaged, credentialgrant.ReferenceActive, credentialgrant.ReferenceRetiring, credentialgrant.ReferenceRetired:
	default:
		writeError(w, http.StatusBadRequest, "invalid state")
		return
	}
	for name, into := range map[string]*int{"limit": &filter.Limit, "offset": &filter.Offset} {
		raw := query.Get(name)
		if raw == "" {
			continue
		}
		value, err := strconv.Atoi(raw)
		if err != nil || value < 0 || (name == "limit" && (value < 1 || value > 200)) {
			writeError(w, http.StatusBadRequest, "invalid limit or offset")
			return
		}
		*into = value
	}
	rows, err := h.store.List(r.Context(), filter)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	views := make([]referenceView, 0, len(rows))
	for _, row := range rows {
		views = append(views, view(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{"rows": views})
}

// Get serves GET /admin/credential_references/{id}.
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	rec, err := h.store.Get(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view(rec))
}

func (h *Handler) transition(action repos.CredentialReferenceAction) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		who, ok := actor(w, r)
		if !ok {
			return
		}
		var body struct{}
		if !decode(w, r, &body) {
			return
		}
		rec, err := h.store.Transition(r.Context(), pathID(r, string(action)), action, who)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, view(rec))
	}
}

// Activate serves POST …/{id}:activate. The caller must not be the stager.
func (h *Handler) Activate(w http.ResponseWriter, r *http.Request) {
	h.transition(repos.CredentialReferenceActivate)(w, r)
}

// Retire serves POST …/{id}:retire.
func (h *Handler) Retire(w http.ResponseWriter, r *http.Request) {
	h.transition(repos.CredentialReferenceRetire)(w, r)
}

type toolkitBody struct {
	ProjectID int64 `json:"project_id"`
	ToolkitID int64 `json:"toolkit_id"`
}

type attachmentView struct {
	ProjectID             int64   `json:"project_id"`
	ToolkitID             int64   `json:"toolkit_id"`
	Field                 string  `json:"field"`
	CredentialReferenceID *string `json:"credential_reference_id"`
	ToolkitDigest         string  `json:"toolkit_digest"`
}

func (h *Handler) attachment(verb string, write func(context.Context, string, repos.ToolkitAttachment, string) (repos.ToolkitAttachmentResult, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		who, ok := actor(w, r)
		if !ok {
			return
		}
		var body toolkitBody
		if !decode(w, r, &body) {
			return
		}
		if body.ProjectID <= 0 || body.ToolkitID <= 0 {
			writeError(w, http.StatusBadRequest, "project_id and toolkit_id are required")
			return
		}
		result, err := write(r.Context(), pathID(r, verb), repos.ToolkitAttachment{ProjectID: body.ProjectID, ToolkitID: body.ToolkitID}, who)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, attachmentView{
			ProjectID: result.ProjectID, ToolkitID: result.ToolkitID, Field: result.Field,
			CredentialReferenceID: optional(result.CredentialReferenceID),
			ToolkitDigest:         hex.EncodeToString(result.ToolkitDigest[:]),
		})
	}
}

// Attach serves POST …/{id}:attach. It edits the toolkit, so the toolkit's
// digest changes and an agent toolkit binding must be pinned again.
func (h *Handler) Attach(w http.ResponseWriter, r *http.Request) {
	h.attachment("attach", h.store.Attach)(w, r)
}

// Detach serves POST …/{id}:detach.
func (h *Handler) Detach(w http.ResponseWriter, r *http.Request) {
	h.attachment("detach", h.store.Detach)(w, r)
}

type pinBody struct {
	ProjectID int64 `json:"project_id"`
}

type pinView struct {
	AgentBindingID  string `json:"agent_binding_id"`
	TenantProjectID int64  `json:"tenant_project_id"`
	ToolkitID       int64  `json:"toolkit_id"`
	ToolkitDigest   string `json:"toolkit_digest"`
}

func pinTarget(w http.ResponseWriter, r *http.Request) (repos.ToolkitAttachment, bool) {
	toolkitID, err := strconv.ParseInt(chi.URLParam(r, "toolkitID"), 10, 64)
	if err != nil || toolkitID <= 0 {
		writeError(w, http.StatusBadRequest, "invalid toolkit id")
		return repos.ToolkitAttachment{}, false
	}
	var body pinBody
	if !decode(w, r, &body) {
		return repos.ToolkitAttachment{}, false
	}
	// A DELETE may name the project in the query instead of a body.
	if raw := r.URL.Query().Get("project_id"); raw != "" && body.ProjectID == 0 {
		if body.ProjectID, err = strconv.ParseInt(raw, 10, 64); err != nil {
			body.ProjectID = 0
		}
	}
	if body.ProjectID <= 0 {
		writeError(w, http.StatusBadRequest, "project_id is required")
		return repos.ToolkitAttachment{}, false
	}
	return repos.ToolkitAttachment{ProjectID: body.ProjectID, ToolkitID: toolkitID}, true
}

// PinToolkit serves PUT /admin/agent_identities/{id}/toolkits/{toolkitID}:
// it binds the toolkit to the agent binding at its current stored digest.
func (h *Handler) PinToolkit(w http.ResponseWriter, r *http.Request) {
	who, ok := actor(w, r)
	if !ok {
		return
	}
	target, ok := pinTarget(w, r)
	if !ok {
		return
	}
	pin, err := h.store.PinToolkit(r.Context(), chi.URLParam(r, "id"), target, who)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, pinView{
		AgentBindingID: pin.AgentBindingID, TenantProjectID: pin.TenantProjectID, ToolkitID: pin.ToolkitID,
		ToolkitDigest: hex.EncodeToString(pin.ToolkitDigest[:]),
	})
}

// UnpinToolkit serves DELETE /admin/agent_identities/{id}/toolkits/{toolkitID}.
func (h *Handler) UnpinToolkit(w http.ResponseWriter, r *http.Request) {
	who, ok := actor(w, r)
	if !ok {
		return
	}
	target, ok := pinTarget(w, r)
	if !ok {
		return
	}
	if err := h.store.UnpinToolkit(r.Context(), chi.URLParam(r, "id"), target, who); err != nil {
		writeStoreError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

// pathID reads {id} without its `:verb` suffix.
func pathID(r *http.Request, verb string) string {
	return strings.TrimSuffix(chi.URLParam(r, "id"), ":"+verb)
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

// decode reads a JSON object with no unknown field. An empty body is `{}`.
func decode(w http.ResponseWriter, r *http.Request, into any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	decoder.DisallowUnknownFields()
	err := decoder.Decode(into)
	if errors.Is(err, io.EOF) {
		return true
	}
	if err != nil || decoder.More() {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return false
	}
	return true
}

func writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, credentialgrant.ErrReferenceNotFound), errors.Is(err, repos.ErrToolkitNotFound),
		errors.Is(err, repos.ErrAgentBindingNotUsable):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, repos.ErrCredentialReferenceSelfActive), errors.Is(err, repos.ErrCredentialReferenceActor):
		writeError(w, http.StatusForbidden, err.Error())
	case errors.Is(err, repos.ErrCredentialReferenceTransition), errors.Is(err, repos.ErrCredentialReferenceNoStager),
		errors.Is(err, repos.ErrCredentialReferenceRetired), errors.Is(err, repos.ErrCredentialReferenceTenant),
		errors.Is(err, repos.ErrCredentialReferenceIncomplete),
		errors.Is(err, repos.ErrCredentialReferenceProvider), errors.Is(err, repos.ErrToolkitNoConfigurationRef),
		errors.Is(err, repos.ErrToolkitConfigurationAmbiguous), errors.Is(err, repos.ErrToolkitPrivateConfiguration),
		errors.Is(err, repos.ErrToolkitAttachedElsewhere):
		writeError(w, http.StatusConflict, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "credential references admin: internal error")
	}
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
