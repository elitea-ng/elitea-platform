// Package runcaps serves the run cap override (ADR-0041 decision 3, #1498):
//
//	POST /api/v2/elitea_core/run_cap_overrides/prompt_lib/{projectID}
//	     {"message_id": "...", "conversation_uuid": "..." (optional)}
//
// A holder of safety.override_cap (resolved in the project; the router
// applies the gate) lets the conversation of a response that ended with
// CAP_REACHED continue past that cap once: the conversation's next agent run
// gets that one cap doubled. The grant is a security audit event, and the
// request's audit record names it (safety.run_cap_override.granted).
package runcaps

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

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workerruncaps"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/audit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// ProjectPath is the route pattern, relative to /api/v2/elitea_core.
const ProjectPath = "/run_cap_overrides/prompt_lib/{projectID}"

// AuditAction names the grant in the request's audit record.
const AuditAction = "safety.run_cap_override.granted"

// Granter is *workerruncaps.OverrideService.
type Granter interface {
	Grant(ctx context.Context, request workerruncaps.OverrideRequest) (workerruncaps.OverrideGrant, error)
}

var _ Granter = (*workerruncaps.OverrideService)(nil)

// Handler serves the route. A nil Handler or service answers 503.
type Handler struct {
	service Granter
}

// NewHandler builds the handler. A nil service answers 503.
func NewHandler(service Granter) *Handler {
	return &Handler{service: service}
}

type grantBody struct {
	ConversationUUID string `json:"conversation_uuid"`
	MessageID        string `json:"message_id"`
}

type grantResponse struct {
	ID                int64     `json:"id"`
	Cap               string    `json:"cap"`
	SourceExecutionID string    `json:"source_execution_id,omitempty"`
	GrantedAt         time.Time `json:"granted_at"`
	ExpiresAt         time.Time `json:"expires_at"`
}

// Grant records an override.
func (h *Handler) Grant(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.service == nil {
		apierr.WriteStatus(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	projectID, err := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 32)
	if err != nil || projectID <= 0 {
		apierr.WriteStatus(w, http.StatusBadRequest, "invalid project id")
		return
	}
	principal, _ := auth.UserFromContext(r.Context())
	userID, ok := principal.OwningUserID()
	if !ok || userID > math.MaxInt32 {
		apierr.WriteStatus(w, http.StatusUnauthorized, "the override needs a signed-in user")
		return
	}
	var body grantBody
	decoder := json.NewDecoder(io.LimitReader(r.Body, 4<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		apierr.WriteStatus(w, http.StatusBadRequest, "invalid request body")
		return
	}
	grant, err := h.service.Grant(r.Context(), workerruncaps.OverrideRequest{
		ProjectID: projectID, ActorUserID: userID,
		ConversationUUID: body.ConversationUUID, MessageID: body.MessageID,
	})
	if err != nil {
		writeError(r, w, err)
		return
	}
	id := grant.ID
	audit.Annotate(r.Context(), audit.Annotation{
		Action:     AuditAction,
		EntityType: "run_cap_override",
		EntityID:   &id,
		EntityName: grant.Cap,
		ProjectID:  &projectID,
	})
	writeJSON(w, http.StatusCreated, grantResponse{
		ID: grant.ID, Cap: grant.Cap, SourceExecutionID: grant.SourceExecutionID,
		GrantedAt: grant.GrantedAt, ExpiresAt: grant.ExpiresAt,
	})
}

func writeError(r *http.Request, w http.ResponseWriter, err error) {
	status, code := http.StatusInternalServerError, ""
	switch {
	case errors.Is(err, workerruncaps.ErrOverrideInvalid):
		status, code = http.StatusBadRequest, "invalid_request"
	case errors.Is(err, workerruncaps.ErrOverrideNotCapped):
		status, code = http.StatusNotFound, "not_capped"
	case errors.Is(err, workerruncaps.ErrOverrideAlreadyGranted):
		status, code = http.StatusConflict, "already_granted"
	case errors.Is(err, workerruncaps.ErrOverrideOpenGrant):
		status, code = http.StatusConflict, "open_grant"
	case errors.Is(err, workerruncaps.ErrOverrideNeedsTierCaps):
		status, code = http.StatusUnprocessableEntity, "tier_caps_not_saved"
	}
	if status == http.StatusInternalServerError {
		slog.ErrorContext(r.Context(), "grant a run cap override", "err", err)
		apierr.WriteStatus(w, status, "the override could not be recorded")
		return
	}
	writeJSON(w, status, map[string]string{"error": err.Error(), "code": code})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
