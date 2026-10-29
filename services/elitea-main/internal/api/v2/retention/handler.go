// Package retention serves the admin Data retention and Legal holds pages
// (ADR-0042 decision 4, issue #1495):
//
//	GET    /api/v2/admin/retention/policies
//	PUT    /api/v2/admin/retention/policies/{dataClass}
//	PUT    /api/v2/admin/retention/policies/{dataClass}/projects/{projectID}
//	DELETE /api/v2/admin/retention/policies/{dataClass}/projects/{projectID}
//	GET    /api/v2/admin/legal_holds
//	POST   /api/v2/admin/legal_holds
//	POST   /api/v2/admin/legal_holds/{id}/release
//
// Policy routes need `retention.manage`, hold routes `legal_hold.manage`
// (administration mode, shared 0195). Every write is under /api/v2/admin/, so
// the audit middleware records it; the handler annotates the row with the
// domain action (retention.policy.update, retention.override.set,
// retention.override.delete, legal_hold.create, legal_hold.release).
package retention

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/audit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/retention"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// Permissions the routes need.
const (
	PermissionRetentionManage = "retention.manage"
	PermissionLegalHoldManage = "legal_hold.manage"
)

const maxBodyBytes = 8 << 10

// Store is the persistence the handler needs (repos.RetentionPolicyRepo).
type Store interface {
	ListPolicies(ctx context.Context) ([]retention.Policy, error)
	SetPlatformPolicy(ctx context.Context, p retention.Policy, actor int64) (retention.Policy, error)
	SetProjectOverride(ctx context.Context, p retention.Policy, actor int64) (retention.Policy, error)
	DeleteProjectOverride(ctx context.Context, class retention.DataClass, projectID int64) error
	ListHolds(ctx context.Context, activeOnly bool) ([]retention.Hold, error)
	CreateHold(ctx context.Context, h retention.Hold) (retention.Hold, error)
	ReleaseHold(ctx context.Context, id, actor int64, reason string) (retention.Hold, error)
}

// Handler serves the routes. A zero Handler answers 503.
type Handler struct {
	store Store
}

// NewHandler builds the handler.
func NewHandler(store Store) *Handler { return &Handler{store: store} }

// enforcedBy says, per class, what applies a maximum, so the page can show it.
var enforcedBy = map[retention.DataClass]string{
	retention.ClassConversation:      "elitea-main conversation retention routine (main.conversation_retention)",
	retention.ClassAttachment:        "deleted with their conversation; no separate maximum yet",
	retention.ClassGatewayRequestLog: "LLM gateway hourly prune",
	retention.ClassAdminAudit:        "elitea-scheduler audit retention sweeper (after an administrator saves this row; until then AUDIT_RETENTION_DAYS)",
	retention.ClassSecurityAudit:     "elitea-scheduler security audit outbox sweeper (after an administrator saves this row; until then SECURITY_AUDIT_OUTBOX_RETENTION_DAYS)",
}

type policyView struct {
	DataClass            string `json:"data_class"`
	ProjectID            *int64 `json:"project_id"`
	MinAgeDays           int    `json:"min_age_days"`
	MaxAgeDays           *int   `json:"max_age_days"`
	AllowProjectOverride bool   `json:"allow_project_override"`
	OverridesSupported   bool   `json:"overrides_supported"`
	FloorDays            int    `json:"floor_days"`
	EnforcedBy           string `json:"enforced_by"`
	UpdatedBy            *int64 `json:"updated_by"`
	UpdatedAt            string `json:"updated_at"`
}

func viewPolicy(p retention.Policy) policyView {
	return policyView{
		DataClass: string(p.Class), ProjectID: p.ProjectID, MinAgeDays: p.MinAgeDays, MaxAgeDays: p.MaxAgeDays,
		AllowProjectOverride: p.AllowProjectOverride, OverridesSupported: retention.Overridable(p.Class),
		FloorDays: retention.FloorDays(p.Class), EnforcedBy: enforcedBy[p.Class],
		UpdatedBy: p.UpdatedBy, UpdatedAt: p.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

type holdView struct {
	ID            int64   `json:"id"`
	Scope         string  `json:"scope"`
	ProjectID     *int64  `json:"project_id"`
	AgentID       *int64  `json:"agent_id"`
	UserID        *int64  `json:"user_id"`
	Reason        string  `json:"reason"`
	Active        bool    `json:"active"`
	CreatedBy     int64   `json:"created_by"`
	CreatedAt     string  `json:"created_at"`
	ReleasedBy    *int64  `json:"released_by"`
	ReleasedAt    *string `json:"released_at"`
	ReleaseReason *string `json:"release_reason"`
}

func viewHold(h retention.Hold) holdView {
	v := holdView{
		ID: h.ID, Scope: string(h.Scope), ProjectID: h.ProjectID, AgentID: h.AgentID, UserID: h.UserID,
		Reason: h.Reason, Active: h.Active(), CreatedBy: h.CreatedBy, CreatedAt: h.CreatedAt.UTC().Format(time.RFC3339),
		ReleasedBy: h.ReleasedBy, ReleaseReason: h.ReleaseReason,
	}
	if h.ReleasedAt != nil {
		at := h.ReleasedAt.UTC().Format(time.RFC3339)
		v.ReleasedAt = &at
	}
	return v
}

func (h *Handler) ready(w http.ResponseWriter) bool {
	if h == nil || h.store == nil {
		writeJSON(w, http.StatusServiceUnavailable, apierr.Response{Error: "retention policy store is not configured"})
		return false
	}
	return true
}

// ListPolicies answers every platform row and project override.
func (h *Handler) ListPolicies(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	policies, err := h.store.ListPolicies(r.Context())
	if err != nil {
		apierr.Write(w, err)
		return
	}
	out := make([]policyView, 0, len(policies))
	for _, p := range policies {
		out = append(out, viewPolicy(p))
	}
	writeJSON(w, http.StatusOK, map[string]any{"policies": out})
}

// required is a body field that must be present; null is a value (for
// max_age_days, keep forever), absence is not.
type required[T any] struct {
	set   bool
	value *T
}

func (r *required[T]) UnmarshalJSON(data []byte) error {
	r.set = true
	if string(data) == "null" {
		r.value = nil
		return nil
	}
	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	r.value = &v
	return nil
}

type platformUpdate struct {
	MinAgeDays           required[int]  `json:"min_age_days"`
	MaxAgeDays           required[int]  `json:"max_age_days"`
	AllowProjectOverride required[bool] `json:"allow_project_override"`
}

// SetPlatformPolicy replaces the platform row of a data class. The PUT is a
// replace, so every field is required: an omitted field used to reset the
// row's minimum to 0 and its override switch to off without saying so.
// max_age_days null means keep forever.
func (h *Handler) SetPlatformPolicy(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	userID, ok := actor(w, r)
	if !ok {
		return
	}
	class := retention.DataClass(chi.URLParam(r, "dataClass"))
	var body platformUpdate
	if !decode(w, r, &body) {
		return
	}
	if !body.MinAgeDays.set || body.MinAgeDays.value == nil || !body.MaxAgeDays.set ||
		!body.AllowProjectOverride.set || body.AllowProjectOverride.value == nil {
		apierr.Write(w, apierr.BadRequest(
			"min_age_days, max_age_days (null keeps forever) and allow_project_override are all required"))
		return
	}
	audit.Annotate(r.Context(), audit.Annotation{
		EventType: "retention", Action: "retention.policy.update", EntityType: "retention_policy",
		EntityName: string(class) + " " + describeMax(body.MaxAgeDays.value),
	})
	stored, err := h.store.SetPlatformPolicy(r.Context(), retention.Policy{
		Class: class, MinAgeDays: *body.MinAgeDays.value, MaxAgeDays: body.MaxAgeDays.value,
		AllowProjectOverride: *body.AllowProjectOverride.value,
	}, userID)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, viewPolicy(stored))
}

type overrideUpdate struct {
	MaxAgeDays *int `json:"max_age_days"`
}

// SetProjectOverride stores a project's own maximum, inside the platform
// bounds.
func (h *Handler) SetProjectOverride(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	userID, ok := actor(w, r)
	if !ok {
		return
	}
	class := retention.DataClass(chi.URLParam(r, "dataClass"))
	projectID, ok := positiveParam(w, r, "projectID")
	if !ok {
		return
	}
	var body overrideUpdate
	if !decode(w, r, &body) {
		return
	}
	audit.Annotate(r.Context(), audit.Annotation{
		EventType: "retention", Action: "retention.override.set", EntityType: "retention_policy",
		EntityName: string(class) + " " + describeMax(body.MaxAgeDays), ProjectID: &projectID,
	})
	stored, err := h.store.SetProjectOverride(r.Context(), retention.Policy{
		Class: class, ProjectID: &projectID, MaxAgeDays: body.MaxAgeDays,
	}, userID)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, viewPolicy(stored))
}

// DeleteProjectOverride removes a project's override.
func (h *Handler) DeleteProjectOverride(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	if _, ok := actor(w, r); !ok {
		return
	}
	class := retention.DataClass(chi.URLParam(r, "dataClass"))
	projectID, ok := positiveParam(w, r, "projectID")
	if !ok {
		return
	}
	audit.Annotate(r.Context(), audit.Annotation{
		EventType: "retention", Action: "retention.override.delete", EntityType: "retention_policy",
		EntityName: string(class), ProjectID: &projectID,
	})
	if err := h.store.DeleteProjectOverride(r.Context(), class, projectID); err != nil {
		apierr.Write(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ListHolds answers holds, newest first. ?active=true drops released ones.
func (h *Handler) ListHolds(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	activeOnly := false
	if raw := r.URL.Query().Get("active"); raw != "" {
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			apierr.Write(w, apierr.BadRequest("active must be true or false"))
			return
		}
		activeOnly = parsed
	}
	holds, err := h.store.ListHolds(r.Context(), activeOnly)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	out := make([]holdView, 0, len(holds))
	for _, hold := range holds {
		out = append(out, viewHold(hold))
	}
	writeJSON(w, http.StatusOK, map[string]any{"holds": out})
}

type holdCreate struct {
	Scope     string `json:"scope"`
	ProjectID *int64 `json:"project_id"`
	AgentID   *int64 `json:"agent_id"`
	UserID    *int64 `json:"user_id"`
	Reason    string `json:"reason"`
}

// CreateHold places a legal hold.
func (h *Handler) CreateHold(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	userID, ok := actor(w, r)
	if !ok {
		return
	}
	var body holdCreate
	if !decode(w, r, &body) {
		return
	}
	hold := retention.Hold{
		Scope: retention.HoldScope(body.Scope), ProjectID: body.ProjectID, AgentID: body.AgentID, UserID: body.UserID,
		Reason: strings.TrimSpace(body.Reason), CreatedBy: userID,
	}
	audit.Annotate(r.Context(), audit.Annotation{
		EventType: "legal_hold", Action: "legal_hold.create", EntityType: "legal_hold",
		EntityName: holdSubject(hold), ProjectID: hold.ProjectID,
	})
	stored, err := h.store.CreateHold(r.Context(), hold)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	audit.Annotate(r.Context(), audit.Annotation{
		EventType: "legal_hold", Action: "legal_hold.create", EntityType: "legal_hold",
		EntityID: &stored.ID, EntityName: holdSubject(stored), ProjectID: stored.ProjectID,
	})
	writeJSON(w, http.StatusCreated, viewHold(stored))
}

type holdRelease struct {
	Reason string `json:"reason"`
}

// ReleaseHold releases an active hold.
func (h *Handler) ReleaseHold(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	userID, ok := actor(w, r)
	if !ok {
		return
	}
	id, ok := positiveParam(w, r, "id")
	if !ok {
		return
	}
	var body holdRelease
	if !decode(w, r, &body) {
		return
	}
	audit.Annotate(r.Context(), audit.Annotation{
		EventType: "legal_hold", Action: "legal_hold.release", EntityType: "legal_hold", EntityID: &id,
	})
	stored, err := h.store.ReleaseHold(r.Context(), id, userID, strings.TrimSpace(body.Reason))
	if err != nil {
		apierr.Write(w, err)
		return
	}
	audit.Annotate(r.Context(), audit.Annotation{
		EventType: "legal_hold", Action: "legal_hold.release", EntityType: "legal_hold", EntityID: &id,
		EntityName: holdSubject(stored), ProjectID: stored.ProjectID,
	})
	writeJSON(w, http.StatusOK, viewHold(stored))
}

func holdSubject(h retention.Hold) string {
	switch h.Scope {
	case retention.HoldProject:
		if h.ProjectID != nil {
			return "project " + strconv.FormatInt(*h.ProjectID, 10)
		}
	case retention.HoldAgent:
		if h.ProjectID != nil && h.AgentID != nil {
			return "agent " + strconv.FormatInt(*h.AgentID, 10) + " in project " + strconv.FormatInt(*h.ProjectID, 10)
		}
	case retention.HoldUser:
		if h.UserID != nil {
			return "user " + strconv.FormatInt(*h.UserID, 10)
		}
	}
	return string(h.Scope)
}

func describeMax(maxDays *int) string {
	if maxDays == nil {
		return "keep forever"
	}
	return "max " + strconv.Itoa(*maxDays) + " days"
}

// actor is the caller's auth_core user id.
func actor(w http.ResponseWriter, r *http.Request) (int64, bool) {
	user, ok := auth.UserFromContext(r.Context())
	if !ok {
		apierr.Write(w, apierr.Unauthorized("authentication required"))
		return 0, false
	}
	id, err := strconv.ParseInt(strings.TrimSpace(user.UserID), 10, 64)
	if err != nil || id <= 0 {
		apierr.Write(w, apierr.Forbidden("the caller has no resolved user id"))
		return 0, false
	}
	return id, true
}

func positiveParam(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	value, err := strconv.ParseInt(chi.URLParam(r, name), 10, 64)
	if err != nil || value <= 0 {
		apierr.Write(w, apierr.BadRequest(name+" must be a positive integer"))
		return 0, false
	}
	return value, true
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
		apierr.Write(w, apierr.BadRequest("invalid request body"))
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
