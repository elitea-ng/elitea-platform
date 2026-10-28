// Package mandateattachments serves the routes that attach a mandate to an
// unattended run target (ADR-0039, issue #1122):
//
//	PUT|DELETE /admin/mandate_attachments/pipeline_schedules/{projectID}/{versionID}
//	PUT|DELETE /admin/mandate_attachments/pipeline_triggers/{projectID}/{versionID}
//	PUT|DELETE /admin/mandate_attachments/index_schedules/{projectID}/{toolkitID}/{indexName}
//
// The router mounts them only with credential governance on and gates each
// on the central `models.agent_identity.mandates.manage` (shared 0181). An
// attach is refused unless runmandate.Service.CheckAttach accepts it: the
// mandate is in the target's tenant project, of an unattended kind the
// active policy revision allows, bound to the pipeline version the target
// starts, not revoked or expired, and its scopes cover the target. Every
// attach and detach appends a security audit event in its own transaction.
package mandateattachments

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/runmandate"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/mandate"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

const (
	maxBodyBytes     = 4 << 10
	maxIndexNameSize = 256
)

// Store is the attachment store (repos.UnattendedRunMandates).
type Store interface {
	Target(ctx context.Context, a repos.MandateAttachment) (runmandate.Target, error)
	Attach(ctx context.Context, a repos.MandateAttachment, target runmandate.Target) error
	Detach(ctx context.Context, a repos.MandateAttachment, target runmandate.Target) error
}

// Checker is the attach-time check (runmandate.Service).
type Checker interface {
	CheckAttach(ctx context.Context, mandateID string, target runmandate.Target) (mandate.Mandate, error)
}

var (
	_ Store   = (*repos.UnattendedRunMandates)(nil)
	_ Checker = (*runmandate.Service)(nil)
)

// Handler serves the attach routes.
type Handler struct {
	store   Store
	checker Checker
}

// New builds the handler.
func New(store Store, checker Checker) (*Handler, error) {
	if store == nil || checker == nil {
		return nil, errors.New("mandate attachments: a store and a checker are required")
	}
	return &Handler{store: store, checker: checker}, nil
}

// attachBody is the accepted write.
type attachBody struct {
	MandateID string `json:"mandate_id"`
}

// attachmentView is the answer to an attach or a detach.
type attachmentView struct {
	Origin     string `json:"origin"`
	ProjectID  int64  `json:"project_id"`
	ResourceID string `json:"resource_id"`
	// MandateID is absent after a detach.
	MandateID string `json:"mandate_id,omitempty"`
}

func (h *Handler) AttachPipelineSchedule(w http.ResponseWriter, r *http.Request) {
	h.attach(w, r, runmandate.OriginPipelineSchedule)
}

func (h *Handler) DetachPipelineSchedule(w http.ResponseWriter, r *http.Request) {
	h.detach(w, r, runmandate.OriginPipelineSchedule)
}

func (h *Handler) AttachPipelineTrigger(w http.ResponseWriter, r *http.Request) {
	h.attach(w, r, runmandate.OriginPipelineTrigger)
}

func (h *Handler) DetachPipelineTrigger(w http.ResponseWriter, r *http.Request) {
	h.detach(w, r, runmandate.OriginPipelineTrigger)
}

func (h *Handler) AttachIndexSchedule(w http.ResponseWriter, r *http.Request) {
	h.attach(w, r, runmandate.OriginIndexSchedule)
}

func (h *Handler) DetachIndexSchedule(w http.ResponseWriter, r *http.Request) {
	h.detach(w, r, runmandate.OriginIndexSchedule)
}

func (h *Handler) attach(w http.ResponseWriter, r *http.Request, origin runmandate.Origin) {
	attachment, ok := routeAttachment(w, r, origin)
	if !ok {
		return
	}
	var body attachBody
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil || json.Unmarshal(raw, &body) != nil || strings.TrimSpace(body.MandateID) == "" {
		writeError(w, http.StatusBadRequest, "a JSON body with `mandate_id` is required", "")
		return
	}
	attachment.MandateID = strings.TrimSpace(body.MandateID)
	target, ok := h.target(w, r, attachment)
	if !ok {
		return
	}
	if _, err := h.checker.CheckAttach(r.Context(), attachment.MandateID, target); err != nil {
		writeRefusal(w, err)
		return
	}
	if err := h.store.Attach(r.Context(), attachment, target); err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, attachmentView{
		Origin: string(origin), ProjectID: attachment.ProjectID,
		ResourceID: target.ResourceID, MandateID: attachment.MandateID,
	})
}

func (h *Handler) detach(w http.ResponseWriter, r *http.Request, origin runmandate.Origin) {
	attachment, ok := routeAttachment(w, r, origin)
	if !ok {
		return
	}
	target, ok := h.target(w, r, attachment)
	if !ok {
		return
	}
	if err := h.store.Detach(r.Context(), attachment, target); err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, attachmentView{
		Origin: string(origin), ProjectID: attachment.ProjectID, ResourceID: target.ResourceID,
	})
}

func (h *Handler) target(w http.ResponseWriter, r *http.Request, a repos.MandateAttachment) (runmandate.Target, bool) {
	target, err := h.store.Target(r.Context(), a)
	if err != nil {
		writeStoreError(w, err)
		return runmandate.Target{}, false
	}
	return target, true
}

// routeAttachment reads the target from the path and the actor from the
// authenticated context; nothing in the body selects either.
func routeAttachment(w http.ResponseWriter, r *http.Request, origin runmandate.Origin) (repos.MandateAttachment, bool) {
	a := repos.MandateAttachment{Origin: origin}
	var err error
	if a.ProjectID, err = strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64); err != nil || a.ProjectID <= 0 {
		writeError(w, http.StatusBadRequest, "invalid project id", "")
		return a, false
	}
	if origin == runmandate.OriginIndexSchedule {
		if a.ToolkitID, err = strconv.ParseInt(chi.URLParam(r, "toolkitID"), 10, 64); err != nil || a.ToolkitID <= 0 {
			writeError(w, http.StatusBadRequest, "invalid toolkit id", "")
			return a, false
		}
		a.IndexName = chi.URLParam(r, "indexName")
		if a.IndexName == "" || len(a.IndexName) > maxIndexNameSize || strings.ContainsAny(a.IndexName, "\x00\r\n") {
			writeError(w, http.StatusBadRequest, "invalid index name", "")
			return a, false
		}
	} else if a.VersionID, err = strconv.ParseInt(chi.URLParam(r, "versionID"), 10, 64); err != nil || a.VersionID <= 0 {
		writeError(w, http.StatusBadRequest, "invalid version id", "")
		return a, false
	}
	user, ok := auth.UserFromContext(r.Context())
	raw := user.UserID
	if raw == "" {
		raw = user.ID
	}
	if a.ActorID, err = strconv.ParseInt(raw, 10, 64); !ok || err != nil || a.ActorID <= 0 {
		writeError(w, http.StatusUnauthorized, "authentication is required", "")
		return a, false
	}
	return a, true
}

// writeRefusal answers an attach CheckAttach refused, with its result code.
func writeRefusal(w http.ResponseWriter, err error) {
	code := runmandate.Code(err)
	switch {
	case errors.Is(err, runmandate.ErrMandateNotFound):
		writeError(w, http.StatusNotFound, "no such mandate", code)
	case code != "":
		writeError(w, http.StatusConflict, strings.TrimPrefix(err.Error(), "runmandate: "), code)
	default:
		writeError(w, http.StatusInternalServerError, "the mandate could not be checked", "")
	}
}

func writeStoreError(w http.ResponseWriter, err error) {
	if errors.Is(err, repos.ErrAttachmentTargetNotFound) {
		writeError(w, http.StatusNotFound, "no such schedule, live trigger or index in this project", "")
		return
	}
	writeError(w, http.StatusInternalServerError, "the mandate attachment could not be written", "")
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, message, code string) {
	body := map[string]string{"error": message}
	if code != "" {
		body["code"] = code
	}
	writeJSON(w, status, body)
}
