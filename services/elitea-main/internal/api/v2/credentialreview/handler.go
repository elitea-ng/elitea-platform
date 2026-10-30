// Package credentialreview serves the evidence for credential and access
// review (ADR-0044 decision 5, #1501):
//
//	GET  /admin/credential_inventory            the inventory (credentials.inventory.read)
//	POST /admin/credential_inventory/revoke     revoke a PAT or use grant (credentials.inventory.manage)
//	GET  /admin/permission_trimming             the report across projects (credentials.inventory.read)
//	POST /admin/permission_trimming/remove      remove one item (credentials.inventory.manage)
//	GET  /elitea_core/permission_trimming/prompt_lib/{projectID}         the project's report (agents.trimming.manage)
//	POST /elitea_core/permission_trimming/prompt_lib/{projectID}/remove  remove one item (agents.trimming.manage;
//	     a project egress entry also needs models.project_settings.edit, the configuration route's gate)
//
// The router applies the route permissions; the admin routes are under the
// audited /api/v2/admin/ prefix and the project routes under the audited
// /api/v2/elitea_core/permission_trimming/ prefix, and every revoke and
// removal also appends a security audit event in its own transaction. A
// handler with no store answers 503.
package credentialreview

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/audit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialuse"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

// The route patterns. The admin ones are relative to /api/v2/admin, the
// project ones to /api/v2/elitea_core.
const (
	InventoryPath          = "/credential_inventory"
	InventoryRevokePath    = "/credential_inventory/revoke"
	AdminTrimmingPath      = "/permission_trimming"
	AdminTrimmingRemove    = "/permission_trimming/remove"
	ProjectTrimmingPath    = "/permission_trimming/prompt_lib/{projectID}"
	ProjectTrimmingRemove  = "/permission_trimming/prompt_lib/{projectID}/remove"
	ReadPermission         = "credentials.inventory.read"
	ManagePermission       = "credentials.inventory.manage"
	TrimmingPermission     = "agents.trimming.manage"
	ProjectEgressPermision = "models.project_settings.edit"
	maxBody                = 8 << 10
	defaultLimit           = 100
	maxLimit               = 500
)

// PermissionCheck reports whether the caller holds a permission in the
// path's project.
type PermissionCheck func(*http.Request) (bool, error)

// Handler serves the routes.
type Handler struct {
	review       *repos.CredentialReview
	trimming     *repos.PermissionTrimming
	projectAdmin PermissionCheck
}

// NewHandler builds the handler. A nil store answers 503; a nil
// projectEgress check refuses project egress removals.
func NewHandler(review *repos.CredentialReview, trimming *repos.PermissionTrimming, projectEgress PermissionCheck) *Handler {
	return &Handler{review: review, trimming: trimming, projectAdmin: projectEgress}
}

// Refusal is the error body.
type Refusal struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func refuse(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, Refusal{Code: code, Message: message})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func (h *Handler) unavailable(w http.ResponseWriter) bool {
	if h == nil || h.review == nil || h.trimming == nil {
		refuse(w, http.StatusServiceUnavailable, "unavailable", "Credential review needs the database.")
		return true
	}
	return false
}

func writeStoreError(w http.ResponseWriter, r *http.Request, err error) {
	var refusal *repos.TrimRefusal
	if errors.As(err, &refusal) {
		// Not a removable candidate now: the code says why (unknown, too
		// new, used in the window, not removable).
		refuse(w, http.StatusConflict, refusal.Code, strings.TrimPrefix(refusal.Error(), "permission trimming: "))
		return
	}
	switch {
	case errors.Is(err, repos.ErrCredentialReviewInvalid):
		refuse(w, http.StatusBadRequest, "invalid_request", "The request is not valid.")
	case errors.Is(err, repos.ErrCredentialNotFound), errors.Is(err, repos.ErrTrimItemNotFound),
		errors.Is(err, credentialgrant.ErrGrantNotFound):
		refuse(w, http.StatusNotFound, "not_found", "The credential or permission does not exist.")
	case errors.Is(err, repos.ErrCredentialNotRevocable):
		refuse(w, http.StatusConflict, "credential_not_revocable",
			"This credential is issued outside the platform and is flagged only; revoke it where it was issued.")
	case errors.Is(err, repos.ErrTrimItemNotRemovable), errors.Is(err, credentialgrant.ErrInvalidTransition):
		refuse(w, http.StatusConflict, "not_removable", strings.TrimPrefix(err.Error(), "permission trimming: "))
	default:
		slog.ErrorContext(r.Context(), "credential review: request failed", "path", r.URL.Path, "err", err)
		refuse(w, http.StatusInternalServerError, "internal_error", "The request failed.")
	}
}

func actorID(r *http.Request) int64 {
	user, ok := auth.UserFromContext(r.Context())
	if !ok {
		return 0
	}
	id, _ := strconv.ParseInt(user.UserID, 10, 64)
	return id
}

func queryInt(r *http.Request, name string, minimum, maximum int64) (int64, bool) {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return 0, true
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < minimum || value > maximum {
		return 0, false
	}
	return value, true
}

func queryBool(r *http.Request, name string) (*bool, bool) {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	switch raw {
	case "":
		return nil, true
	case "true":
		value := true
		return &value, true
	case "false":
		value := false
		return &value, true
	}
	return nil, false
}

func decode(w http.ResponseWriter, r *http.Request, into any) bool {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil || len(body) > maxBody {
		refuse(w, http.StatusBadRequest, "invalid_request", "The body is too large or unreadable.")
		return false
	}
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		refuse(w, http.StatusBadRequest, "invalid_request", "The body is not valid JSON for this route.")
		return false
	}
	return true
}

// InventoryList is the inventory response.
type InventoryList struct {
	Total int                         `json:"total"`
	Rows  []repos.InventoryCredential `json:"rows"`
	// StaleAfterDays and StaleAction are the policy the stale flag used.
	StaleAfterDays int    `json:"stale_after_days"`
	StaleAction    string `json:"stale_action"`
}

// Inventory answers GET /admin/credential_inventory.
func (h *Handler) Inventory(w http.ResponseWriter, r *http.Request) {
	if h.unavailable(w) {
		return
	}
	filter := repos.InventoryFilter{Limit: defaultLimit}
	if kind := strings.TrimSpace(r.URL.Query().Get("kind")); kind != "" {
		filter.Kind = credentialuse.Kind(kind)
		if !filter.Kind.Valid() {
			refuse(w, http.StatusBadRequest, "invalid_request", "kind is not a credential kind.")
			return
		}
	}
	project, ok1 := queryInt(r, "project_id", 1, 1<<31-1)
	limit, ok2 := queryInt(r, "limit", 0, maxLimit)
	offset, ok3 := queryInt(r, "offset", 0, 1<<31-1)
	stale, ok4 := queryBool(r, "stale")
	if !ok1 || !ok2 || !ok3 || !ok4 {
		refuse(w, http.StatusBadRequest, "invalid_request", "A filter is not valid.")
		return
	}
	filter.ProjectID, filter.Offset, filter.Stale = project, int(offset), stale
	if r.URL.Query().Has("limit") {
		filter.Limit = int(limit)
	}
	policy, err := h.review.Policy(r.Context())
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	rows, total, err := h.review.Inventory(r.Context(), filter)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	if rows == nil {
		rows = []repos.InventoryCredential{}
	}
	for i := range rows {
		if rows[i].Agents == nil {
			rows[i].Agents = []repos.InventoryAgent{}
		}
	}
	writeJSON(w, http.StatusOK, InventoryList{Total: total, Rows: rows,
		StaleAfterDays: policy.StaleAfterDays, StaleAction: string(policy.StaleAction)})
}

// RevokeBody is the revoke request.
type RevokeBody struct {
	Kind         string `json:"kind"`
	CredentialID string `json:"credential_id"`
	Reason       string `json:"reason"`
}

// Revoke answers POST /admin/credential_inventory/revoke.
func (h *Handler) Revoke(w http.ResponseWriter, r *http.Request) {
	if h.unavailable(w) {
		return
	}
	var body RevokeBody
	if !decode(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.CredentialID) == "" || len(body.Reason) > 500 || !credentialuse.Kind(body.Kind).Valid() {
		refuse(w, http.StatusBadRequest, "invalid_request", "kind, credential_id and a reason of at most 500 characters are required.")
		return
	}
	audit.Annotate(r.Context(), audit.Annotation{
		Action: "credential.revoke", EntityType: "credential." + body.Kind, EntityName: body.CredentialID,
	})
	result, err := h.review.Revoke(r.Context(), repos.RevokeRequest{
		Kind: credentialuse.Kind(body.Kind), CredentialID: body.CredentialID, ActorID: actorID(r), Reason: body.Reason,
	})
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func trimmingFilter(w http.ResponseWriter, r *http.Request) (repos.TrimmingFilter, bool) {
	app, ok1 := queryInt(r, "application_id", 1, 1<<62)
	window, ok2 := queryInt(r, "window_days", 1, 365)
	candidates, ok3 := queryBool(r, "candidates_only")
	if !ok1 || !ok2 || !ok3 {
		refuse(w, http.StatusBadRequest, "invalid_request", "A filter is not valid.")
		return repos.TrimmingFilter{}, false
	}
	filter := repos.TrimmingFilter{ApplicationID: app, WindowDays: int(window)}
	if candidates != nil {
		filter.CandidatesOnly = *candidates
	}
	return filter, true
}

// AdminTrimming answers GET /admin/permission_trimming.
func (h *Handler) AdminTrimming(w http.ResponseWriter, r *http.Request) {
	if h.unavailable(w) {
		return
	}
	filter, ok := trimmingFilter(w, r)
	if !ok {
		return
	}
	project, ok := queryInt(r, "project_id", 1, 1<<31-1)
	if !ok || (filter.ApplicationID > 0 && project == 0) {
		refuse(w, http.StatusBadRequest, "invalid_request", "project_id is not valid (application_id needs it).")
		return
	}
	filter.ProjectID = project
	filter.IncludePlatform = true
	report, err := h.trimming.Report(r.Context(), filter)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, normalizeReport(report))
}

// ProjectTrimming answers GET /elitea_core/permission_trimming/prompt_lib/{projectID}.
func (h *Handler) ProjectTrimming(w http.ResponseWriter, r *http.Request) {
	if h.unavailable(w) {
		return
	}
	project, err := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	if err != nil || project <= 0 {
		refuse(w, http.StatusBadRequest, "invalid_request", "The project id is not valid.")
		return
	}
	filter, ok := trimmingFilter(w, r)
	if !ok {
		return
	}
	filter.ProjectID = project
	report, err := h.trimming.Report(r.Context(), filter)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, normalizeReport(report))
}

// normalizeReport answers empty arrays, never null, as the contract says.
func normalizeReport(report repos.TrimmingReport) repos.TrimmingReport {
	if report.Items == nil {
		report.Items = []repos.TrimmingItem{}
	}
	for i := range report.Items {
		if report.Items[i].Scopes == nil {
			report.Items[i].Scopes = []string{}
		}
		if report.Items[i].UnusedScopes == nil {
			report.Items[i].UnusedScopes = []string{}
		}
	}
	return report
}

// RemoveBody is a removal request.
type RemoveBody struct {
	// ProjectID is required on the admin route (0 with a platform egress
	// entry) and ignored on the project route (the path names it).
	ProjectID     int64  `json:"project_id"`
	ApplicationID int64  `json:"application_id"`
	Kind          string `json:"kind"`
	Key           string `json:"key"`
}

func validRemove(body RemoveBody) bool {
	switch body.Kind {
	case repos.TrimTool, repos.TrimToolkit, repos.TrimCredentialGrant, repos.TrimEgressHost:
	default:
		return false
	}
	return strings.TrimSpace(body.Key) != "" && len(body.Key) <= 1024 && body.ProjectID >= 0 && body.ApplicationID >= 0
}

func (h *Handler) remove(w http.ResponseWriter, r *http.Request, request repos.RemoveRequest) {
	annotation := audit.Annotation{Action: "permission.trim", EntityType: "agent." + request.Kind, EntityName: request.Key}
	if request.ApplicationID > 0 {
		annotation.EntityID = &request.ApplicationID
	}
	if request.ProjectID > 0 {
		annotation.ProjectID = &request.ProjectID
	}
	audit.Annotate(r.Context(), annotation)
	result, err := h.trimming.Remove(r.Context(), request)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// AdminRemove answers POST /admin/permission_trimming/remove.
func (h *Handler) AdminRemove(w http.ResponseWriter, r *http.Request) {
	if h.unavailable(w) {
		return
	}
	var body RemoveBody
	if !decode(w, r, &body) {
		return
	}
	platform := body.Kind == repos.TrimEgressHost && strings.HasPrefix(body.Key, "platform:")
	if !validRemove(body) || (body.ProjectID == 0 && !platform) {
		refuse(w, http.StatusBadRequest, "invalid_request", "project_id, kind and key are required.")
		return
	}
	h.remove(w, r, repos.RemoveRequest{ProjectID: body.ProjectID, ApplicationID: body.ApplicationID,
		Kind: body.Kind, Key: body.Key, ActorID: actorID(r), Platform: platform})
}

// ProjectRemove answers POST /elitea_core/permission_trimming/prompt_lib/{projectID}/remove.
func (h *Handler) ProjectRemove(w http.ResponseWriter, r *http.Request) {
	if h.unavailable(w) {
		return
	}
	project, err := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	if err != nil || project <= 0 {
		refuse(w, http.StatusBadRequest, "invalid_request", "The project id is not valid.")
		return
	}
	var body RemoveBody
	if !decode(w, r, &body) {
		return
	}
	body.ProjectID = project
	if !validRemove(body) || strings.HasPrefix(body.Key, "platform:") {
		refuse(w, http.StatusBadRequest, "invalid_request", "kind and key are required; a platform entry is removed by an administrator.")
		return
	}
	if body.Kind == repos.TrimEgressHost {
		// The project's egress allowlist is a project setting: the
		// configuration route's own gate applies.
		allowed := false
		if h.projectAdmin != nil {
			allowed, err = h.projectAdmin(r)
			if err != nil {
				writeStoreError(w, r, err)
				return
			}
		}
		if !allowed {
			refuse(w, http.StatusForbidden, "forbidden", "Removing a project egress entry needs "+ProjectEgressPermision+".")
			return
		}
	}
	h.remove(w, r, repos.RemoveRequest{ProjectID: project, ApplicationID: body.ApplicationID,
		Kind: body.Kind, Key: body.Key, ActorID: actorID(r)})
}
