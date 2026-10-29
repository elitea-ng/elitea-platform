// Package agentreleases serves the agent releases and draft revisions of
// ADR-0040 decision 2 under /api/v2/elitea_core:
//
//	GET  /application_releases/prompt_lib/{projectID}/{applicationID}
//	     the releases of an agent, newest first (models.applications.versions.get)
//	GET  /application_releases/prompt_lib/{projectID}/{applicationID}/{releaseID}
//	     one release: definition, toolkit pins, diff against the release
//	     before it (models.applications.version.details)
//	POST /application_releases/prompt_lib/{projectID}/{applicationID}/{releaseID}/rollback
//	     points the agent's published release at an earlier release
//	     (models.applications.publish.post; for an agent whose tier needs
//	     approval, also models.applications.governance); audited as
//	     application_release.rolled_back
//	GET  /version_revisions/prompt_lib/{projectID}/{applicationID}/{versionID}
//	     the draft revisions of a version, newest first
//	     (models.applications.versions.get)
//
// The router applies the gates. A handler with no store answers 503.
package agentreleases

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/audit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentrelease"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// The route patterns, relative to the /api/v2/elitea_core group.
const (
	ReleasesPath  = "/application_releases/prompt_lib/{projectID}/{applicationID}"
	ReleasePath   = "/application_releases/prompt_lib/{projectID}/{applicationID}/{releaseID}"
	RollbackPath  = "/application_releases/prompt_lib/{projectID}/{applicationID}/{releaseID}/rollback"
	RevisionsPath = "/version_revisions/prompt_lib/{projectID}/{applicationID}/{versionID}"
)

// AuditActionRolledBack is the audit action of a rollback.
const AuditActionRolledBack = "application_release.rolled_back"

// maxReasonLength bounds the rollback reason in characters (the column
// CHECK). It keeps the reason whole in the audit event's entity name,
// which the audit store clamps at 256 bytes (rollbackEntityName).
const maxReasonLength = 200

// maxRollbackBody bounds the rollback request body.
const maxRollbackBody = 8 << 10

// Store is *repos.AgentReleases.
type Store interface {
	ListReleases(ctx context.Context, projectID, applicationID int64) ([]agentrelease.Release, error)
	GetRelease(ctx context.Context, projectID, applicationID, releaseID int64) (repos.ReleaseDetail, error)
	Rollback(ctx context.Context, projectID, applicationID, releaseID, actorID int64, reason string) (repos.RollbackOutcome, error)
	ListRevisions(ctx context.Context, projectID, applicationID, versionID int64) ([]agentrelease.Revision, error)
}

var _ Store = (*repos.AgentReleases)(nil)

// Governor is the agent lifecycle governance a rollback answers to
// (*repos.AgentLifecycle): a rollback moves the published release of an
// agent with no change request, so an agent whose tier needs approval is
// rolled back by a governance role holder only (ADR-0040 decision 3).
type Governor interface {
	// CheckDirectPublish refuses (change_request_required) when the
	// agent's tier needs an approved change request.
	CheckDirectPublish(ctx context.Context, projectID, applicationID int64) error
}

// Handler serves the routes.
type Handler struct {
	store    Store
	governor Governor
	// holdsGovernance resolves the caller's governance role in the path's
	// project.
	holdsGovernance func(*http.Request) (bool, error)
}

// Option configures the handler.
type Option func(*Handler)

// WithGovernance makes a rollback of an agent whose tier needs approval
// require the governance role. A nil governor (or a typed nil) is absent.
func WithGovernance(governor Governor, holdsGovernance func(*http.Request) (bool, error)) Option {
	return func(h *Handler) {
		if governor == nil || holdsGovernance == nil {
			return
		}
		if lifecycle, ok := governor.(*repos.AgentLifecycle); ok && lifecycle == nil {
			return
		}
		h.governor, h.holdsGovernance = governor, holdsGovernance
	}
}

// NewHandler builds the handler. A nil store (or a typed nil) answers 503.
func NewHandler(store Store, options ...Option) *Handler {
	if releases, ok := store.(*repos.AgentReleases); ok && releases == nil {
		store = nil
	}
	h := &Handler{store: store}
	for _, option := range options {
		option(h)
	}
	return h
}

// ReleaseSummary is one release in a response.
type ReleaseSummary struct {
	ID                   int64      `json:"id"`
	ApplicationID        int64      `json:"application_id"`
	ApplicationVersionID int64      `json:"application_version_id"`
	SourceVersionID      *int64     `json:"source_version_id"`
	Name                 string     `json:"name"`
	DefinitionDigest     string     `json:"definition_digest"`
	State                string     `json:"state"`
	IsPublishedRelease   bool       `json:"is_published_release"`
	PublishedBy          *int64     `json:"published_by"`
	CreatedAt            time.Time  `json:"created_at"`
	WithdrawnAt          *time.Time `json:"withdrawn_at"`
	WithdrawnBy          *int64     `json:"withdrawn_by"`
}

// ReleaseDetail is one release with its definition, pins and diff.
type ReleaseDetail struct {
	ReleaseSummary
	Definition        json.RawMessage           `json:"definition"`
	ToolkitPins       []agentrelease.ToolkitPin `json:"toolkit_pins"`
	PreviousReleaseID *int64                    `json:"previous_release_id"`
	Changes           []agentrelease.Change     `json:"changes"`
}

// Revision is one draft revision in a response.
type Revision struct {
	ID        int64           `json:"id"`
	Revision  int             `json:"revision"`
	Kind      string          `json:"kind"`
	ActorID   *int64          `json:"actor_id"`
	CreatedAt time.Time       `json:"created_at"`
	Snapshot  json.RawMessage `json:"snapshot"`
	Diff      json.RawMessage `json:"diff"`
}

func positive(value int64) *int64 {
	if value <= 0 {
		return nil
	}
	return &value
}

func summary(release agentrelease.Release) ReleaseSummary {
	return ReleaseSummary{
		ID: release.ID, ApplicationID: release.ApplicationID, ApplicationVersionID: release.ApplicationVersionID,
		SourceVersionID: positive(release.SourceVersionID), Name: release.Name,
		DefinitionDigest: release.DigestHex(), State: release.State, IsPublishedRelease: release.IsHead,
		PublishedBy: positive(release.PublishedBy), CreatedAt: release.CreatedAt,
		WithdrawnAt: release.WithdrawnAt, WithdrawnBy: positive(release.WithdrawnBy),
	}
}

// List answers the releases of an application.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	projectID, applicationID, ok := h.application(w, r)
	if !ok {
		return
	}
	releases, err := h.store.ListReleases(r.Context(), projectID, applicationID)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	rows := make([]ReleaseSummary, 0, len(releases))
	for _, release := range releases {
		rows = append(rows, summary(release))
	}
	writeJSON(w, http.StatusOK, map[string]any{"total": len(rows), "rows": rows})
}

// Get answers one release.
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	projectID, applicationID, ok := h.application(w, r)
	if !ok {
		return
	}
	releaseID, ok := pathID(w, r, "releaseID", "invalid release id")
	if !ok {
		return
	}
	detail, err := h.store.GetRelease(r.Context(), projectID, applicationID, releaseID)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	pins := detail.Release.ToolkitPins
	if pins == nil {
		pins = []agentrelease.ToolkitPin{}
	}
	changes := detail.Changes
	if changes == nil {
		changes = []agentrelease.Change{}
	}
	writeJSON(w, http.StatusOK, ReleaseDetail{
		ReleaseSummary: summary(detail.Release), Definition: detail.Release.Definition,
		ToolkitPins: pins, PreviousReleaseID: positive(detail.PreviousID), Changes: changes,
	})
}

// Rollback points the application's published release at an earlier one.
func (h *Handler) Rollback(w http.ResponseWriter, r *http.Request) {
	projectID, applicationID, ok := h.application(w, r)
	if !ok {
		return
	}
	releaseID, ok := pathID(w, r, "releaseID", "invalid release id")
	if !ok {
		return
	}
	principal, _ := auth.UserFromContext(r.Context())
	actorID, signedIn := principal.OwningUserID()
	if !signedIn || actorID <= 0 {
		apierr.WriteStatus(w, http.StatusUnauthorized, "the rollback needs a signed-in user")
		return
	}
	var body struct {
		Reason string `json:"reason"`
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxRollbackBody+1))
	if err != nil || len(raw) > maxRollbackBody {
		apierr.WriteStatus(w, http.StatusBadRequest, "invalid rollback request")
		return
	}
	if len(strings.TrimSpace(string(raw))) > 0 {
		decoder := json.NewDecoder(strings.NewReader(string(raw)))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&body) != nil {
			apierr.WriteStatus(w, http.StatusBadRequest, "invalid rollback request")
			return
		}
	}
	body.Reason = strings.TrimSpace(body.Reason)
	if utf8.RuneCountInString(body.Reason) > maxReasonLength {
		apierr.WriteStatus(w, http.StatusBadRequest, "the reason is longer than 200 characters")
		return
	}
	if !h.rollbackAllowed(w, r, projectID, applicationID) {
		return
	}
	outcome, err := h.store.Rollback(r.Context(), projectID, applicationID, releaseID, actorID, body.Reason)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	id := outcome.To.ID
	project := projectID
	audit.Annotate(r.Context(), audit.Annotation{
		Action:     AuditActionRolledBack,
		EntityType: "application_release",
		EntityID:   &id,
		EntityName: rollbackEntityName(applicationID, outcome.From.ID, body.Reason),
		ProjectID:  &project,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"from":   summary(outcome.From),
		"to":     summary(outcome.To),
		"reason": body.Reason,
	})
}

// rollbackAllowed refuses a rollback of an agent whose tier needs an
// approved change request unless the caller holds the governance role: 409
// change_request_required, as a direct publish of that agent answers.
func (h *Handler) rollbackAllowed(w http.ResponseWriter, r *http.Request, projectID, applicationID int64) bool {
	if h.governor == nil {
		return true
	}
	err := h.governor.CheckDirectPublish(r.Context(), projectID, applicationID)
	if err == nil {
		return true
	}
	refusal, ok := agentlifecycle.AsRefusal(err)
	if !ok {
		slog.ErrorContext(r.Context(), "application releases: read the agent's tier", "error", err)
		apierr.WriteStatus(w, http.StatusInternalServerError, "could not read the agent's governance")
		return false
	}
	if refusal.Code == agentlifecycle.ChangeRequiredCode {
		held, err := h.holdsGovernance(r)
		if err != nil {
			slog.ErrorContext(r.Context(), "application releases: resolve the governance role", "error", err)
			apierr.WriteStatus(w, http.StatusInternalServerError, "could not resolve the governance role")
			return false
		}
		if held {
			return true
		}
		refusal = agentlifecycle.RefuseWith(agentlifecycle.ChangeRequiredCode,
			"This agent's tier needs an approved change request to change its published release. "+
				"Only a holder of the governance role ("+agentlifecycle.GovernancePermission+") can roll it back directly.",
			refusal.Detail)
	}
	writeJSON(w, refusal.Status(), refusal.Body())
	return false
}

// Revisions answers the draft revisions of one version.
func (h *Handler) Revisions(w http.ResponseWriter, r *http.Request) {
	projectID, applicationID, ok := h.application(w, r)
	if !ok {
		return
	}
	versionID, ok := pathID(w, r, "versionID", "invalid version id")
	if !ok {
		return
	}
	revisions, err := h.store.ListRevisions(r.Context(), projectID, applicationID, versionID)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	rows := make([]Revision, 0, len(revisions))
	for _, revision := range revisions {
		rows = append(rows, Revision{
			ID: revision.ID, Revision: revision.Revision, Kind: revision.Kind,
			ActorID: positive(revision.ActorID), CreatedAt: revision.CreatedAt,
			Snapshot: revision.Snapshot, Diff: revision.Diff,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"total": len(rows), "rows": rows})
}

// application reads the project and application of the path.
func (h *Handler) application(w http.ResponseWriter, r *http.Request) (int64, int64, bool) {
	if h == nil || h.store == nil {
		apierr.WriteStatus(w, http.StatusServiceUnavailable, "database unavailable")
		return 0, 0, false
	}
	projectID, ok := pathID(w, r, "projectID", "invalid project id")
	if !ok {
		return 0, 0, false
	}
	applicationID, ok := pathID(w, r, "applicationID", "invalid application id")
	if !ok {
		return 0, 0, false
	}
	return projectID, applicationID, true
}

func pathID(w http.ResponseWriter, r *http.Request, name, message string) (int64, bool) {
	value, err := strconv.ParseInt(chi.URLParam(r, name), 10, 32)
	if err != nil || value <= 0 {
		apierr.WriteStatus(w, http.StatusBadRequest, message)
		return 0, false
	}
	return value, true
}

func writeError(ctx context.Context, w http.ResponseWriter, err error) {
	if status, body, refused := agentrelease.HTTPRefusal(err); refused {
		writeJSON(w, status, body)
		return
	}
	switch {
	case errors.Is(err, agentrelease.ErrNotPublished):
		writeCode(w, http.StatusConflict, "application_not_published", "The agent has no published release to roll back.")
	case errors.Is(err, agentrelease.ErrAlreadyHead):
		writeCode(w, http.StatusConflict, "release_already_published", "The release is already the published release.")
	case errors.Is(err, agentrelease.ErrWithdrawn):
		writeCode(w, http.StatusConflict, "release_withdrawn",
			"The release was withdrawn when its version was unpublished. It cannot be the published release.")
	case errors.Is(err, agentrelease.ErrDefinitionUnreadable):
		slog.ErrorContext(ctx, "agent releases: unreadable release definition", "error", err)
		apierr.WriteStatus(w, http.StatusInternalServerError, "could not read the release")
	default:
		slog.ErrorContext(ctx, "agent releases", "error", err)
		apierr.WriteStatus(w, http.StatusInternalServerError, "could not read or save the release")
	}
}

func writeCode(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{"error": code, "code": code, "message": message})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// rollbackEntityName names the rollback in the audit event: the agent, the
// release it moved away from and the operator's reason. The release pointer
// keeps only its latest move, so the event is where a rollback's reason
// survives; the audit store clamps the name to 256 characters.
func rollbackEntityName(applicationID, fromReleaseID int64, reason string) string {
	name := "application:" + strconv.FormatInt(applicationID, 10) + " from release:" + strconv.FormatInt(fromReleaseID, 10)
	if reason != "" {
		name += " reason: " + reason
	}
	return name
}
