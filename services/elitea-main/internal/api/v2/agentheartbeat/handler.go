// Package agentheartbeat serves the heartbeat routes of a proactive agent
// (ADR-0050 decision 2, issue 1618): read and save one agent's heartbeat
// settings, read its heartbeat history, and the project switch.
//
// WHO MAY DO WHAT. The router admits the reads on
// models.applications.versions.get and the writes on
// models.applications.publish.post. Turning a heartbeat on also needs:
//
//   - the platform switch (section proactivity, key enabled) on;
//   - an agent of at least medium risk, or a personal project whose one
//     member self-approves (personal_self_approve, on by default);
//   - a published release whose proactivity block has mode heartbeat;
//   - when a mandate is attached, a mandate that passes the attach check
//     for the release's version and that acts for the caller.
//
// The person who turns the heartbeat on becomes its owner: the person the
// agent notifies and acts for.
package agentheartbeat

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/heartbeat"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/runmandate"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/mandate"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/proactivity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// The permissions of the routes.
const (
	ReadPermission  = "models.applications.versions.get"
	WritePermission = "models.applications.publish.post"
)

// The route patterns, relative to the /api/v2/elitea_core group.
const (
	SettingsPath = "/agent_heartbeat/prompt_lib/{projectID}/{applicationID}"
	HistoryPath  = "/agent_heartbeats/prompt_lib/{projectID}/{applicationID}"
	ProjectPath  = "/agent_heartbeat_project/prompt_lib/{projectID}"
	maxBody      = 4 << 10
)

// The refusal codes of a save.
const (
	CodePlatformOff     = "proactivity_platform_off"
	CodeTierTooLow      = "proactivity_tier_too_low"
	CodeNoRelease       = "proactivity_no_release"
	CodeBlockOff        = "proactivity_block_off"
	CodeMandateRefused  = "proactivity_mandate_refused"
	CodeMandateNotOwner = "proactivity_mandate_not_owner"
)

// Store is *repos.AgentHeartbeats.
type Store interface {
	GetHeartbeatSettings(ctx context.Context, projectID, applicationID int64) (repos.HeartbeatSettings, error)
	SaveHeartbeatSettings(ctx context.Context, projectID, applicationID int64, update repos.HeartbeatSettingsUpdate) (repos.HeartbeatSettings, error)
	ListHeartbeats(ctx context.Context, projectID, applicationID int64, limit, offset int) ([]repos.HeartbeatEntry, int, error)
	HeartbeatGovernanceOf(ctx context.Context, projectID, applicationID, userID int64) (repos.HeartbeatGovernance, error)
	CurrentRelease(ctx context.Context, agent heartbeat.Agent) (heartbeat.Release, bool, error)
	ProjectEnabled(ctx context.Context, projectID int64) (bool, error)
	SetProjectEnabled(ctx context.Context, projectID, actorID int64, enabled bool) error
	Settings(ctx context.Context) (platformconfig.ProactivitySettings, error)
}

var _ Store = (*repos.AgentHeartbeats)(nil)

// Mandates is the attach check of *runmandate.Service.
type Mandates interface {
	CheckAttach(ctx context.Context, mandateID string, target runmandate.Target) (mandate.Mandate, error)
}

// Handler serves the routes.
type Handler struct {
	store    Store
	mandates Mandates
}

// NewHandler builds the handler. A nil store answers 503. A nil mandate
// service refuses every mandate (credential governance is off).
func NewHandler(store Store, mandates *runmandate.Service) *Handler {
	h := &Handler{}
	if typed, ok := store.(*repos.AgentHeartbeats); !ok || typed != nil {
		h.store = store
	}
	if mandates != nil {
		h.mandates = mandates
	}
	return h
}

// Settings is the answer of the settings routes.
type Settings struct {
	repos.HeartbeatSettings
	// Configured is false when nobody turned the heartbeat on yet; the
	// other fields of the row are then zero.
	Configured bool         `json:"configured"`
	Governance Governance   `json:"governance"`
	Release    *ReleaseView `json:"release"`
}

// Governance is whether the caller may turn the heartbeat on, and why not.
type Governance struct {
	PlatformEnabled bool    `json:"platform_enabled"`
	ProjectEnabled  bool    `json:"project_enabled"`
	Tier            string  `json:"tier"`
	Personal        bool    `json:"personal"`
	CanEnable       bool    `json:"can_enable"`
	Reason          string  `json:"reason,omitempty"`
	ActsPerDay      int     `json:"acts_per_day"`
	WakesPerDay     int     `json:"wakes_per_day"`
	SpendPerDayUSD  float64 `json:"spend_per_day_usd"`
}

// ReleaseView is the published release's block, as the heartbeat reads it.
type ReleaseView struct {
	ReleaseID  int64  `json:"release_id"`
	VersionID  int64  `json:"version_id"`
	Mode       string `json:"mode"`
	Interval   string `json:"interval"`
	QuietHours string `json:"quiet_hours,omitempty"`
	ActTarget  string `json:"act_target"`
	GoalCount  int    `json:"goal_count"`
}

type request struct {
	projectID, applicationID, userID int64
}

func (h *Handler) request(w http.ResponseWriter, r *http.Request, withApplication bool) (request, bool) {
	if h == nil || h.store == nil {
		apierr.WriteStatus(w, http.StatusServiceUnavailable, "database unavailable")
		return request{}, false
	}
	var req request
	var ok bool
	if req.projectID, ok = pathID(w, r, "projectID"); !ok {
		return request{}, false
	}
	if withApplication {
		if req.applicationID, ok = pathID(w, r, "applicationID"); !ok {
			return request{}, false
		}
	}
	principal, _ := auth.UserFromContext(r.Context())
	if req.userID, ok = principal.OwningUserID(); !ok || req.userID <= 0 {
		apierr.WriteStatus(w, http.StatusUnauthorized, "this action needs a signed-in user")
		return request{}, false
	}
	return req, true
}

// Get answers one agent's heartbeat settings and whether the caller may
// turn it on.
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	req, ok := h.request(w, r, true)
	if !ok {
		return
	}
	answer, _, _, err := h.view(r.Context(), req)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, answer)
}

// view reads the row, the governance and the release.
func (h *Handler) view(ctx context.Context, req request) (Settings, heartbeat.Release, bool, error) {
	var answer Settings
	row, err := h.store.GetHeartbeatSettings(ctx, req.projectID, req.applicationID)
	switch {
	case errors.Is(err, repos.ErrHeartbeatNotFound):
		answer.HeartbeatSettings = repos.HeartbeatSettings{ApplicationID: req.applicationID}
	case err != nil:
		return Settings{}, heartbeat.Release{}, false, err
	default:
		answer.HeartbeatSettings, answer.Configured = row, true
	}
	platform, err := h.store.Settings(ctx)
	if err != nil {
		return Settings{}, heartbeat.Release{}, false, err
	}
	projectOn, err := h.store.ProjectEnabled(ctx, req.projectID)
	if err != nil {
		return Settings{}, heartbeat.Release{}, false, err
	}
	governance, err := h.store.HeartbeatGovernanceOf(ctx, req.projectID, req.applicationID, req.userID)
	if err != nil {
		return Settings{}, heartbeat.Release{}, false, err
	}
	release, found, err := h.store.CurrentRelease(ctx, heartbeat.Agent{
		ProjectID: req.projectID, ApplicationID: req.applicationID, OwnerUserID: req.userID,
	})
	if err != nil {
		return Settings{}, heartbeat.Release{}, false, err
	}
	answer.Governance = Governance{
		PlatformEnabled: platform.Enabled, ProjectEnabled: projectOn,
		Tier: string(governance.Tier), Personal: governance.Personal,
	}
	block := proactivity.Off
	if found {
		block = proactivity.FromMeta(release.Meta)
		view := &ReleaseView{
			ReleaseID: release.ReleaseID, VersionID: release.VersionID, Mode: block.Mode,
			Interval: block.Interval.String(), ActTarget: block.ActTarget, GoalCount: len(release.Goals),
		}
		if block.QuietHours != nil {
			view.QuietHours = block.QuietHours.String()
		}
		if !block.Enabled() {
			view.Mode = proactivity.ModeOff
		}
		answer.Release = view
	}
	limits := block.Effective(platform.Ceilings, governance.Personal)
	answer.Governance.WakesPerDay, answer.Governance.ActsPerDay, answer.Governance.SpendPerDayUSD =
		limits.WakesPerDay, limits.ActsPerDay, limits.SpendPerDayUSD
	answer.Governance.Reason = refusal(platform, governance, found, block)
	answer.Governance.CanEnable = answer.Governance.Reason == ""
	return answer, release, found, nil
}

// refusal is the code that stops a caller turning the heartbeat on.
func refusal(platform platformconfig.ProactivitySettings, governance repos.HeartbeatGovernance, found bool, block proactivity.Block) string {
	switch {
	case !platform.Enabled:
		return CodePlatformOff
	case !(governance.Personal && platform.PersonalSelfApprove) &&
		(!governance.Tier.Valid() || governance.Tier == agentlifecycle.TierLow):
		return CodeTierTooLow
	case !found:
		return CodeNoRelease
	case !block.Enabled():
		return CodeBlockOff
	}
	return ""
}

type saveBody struct {
	Enabled   *bool   `json:"enabled"`
	MandateID *string `json:"mandate_id"`
}

// Put saves one agent's heartbeat settings.
func (h *Handler) Put(w http.ResponseWriter, r *http.Request) {
	req, ok := h.request(w, r, true)
	if !ok {
		return
	}
	var body saveBody
	if !decode(w, r, &body) {
		return
	}
	if body.Enabled == nil {
		apierr.Write(w, apierr.BadRequest("enabled is required"))
		return
	}
	update := repos.HeartbeatSettingsUpdate{Enabled: *body.Enabled, OwnerUserID: req.userID, ActorID: req.userID}
	if body.MandateID != nil {
		trimmed := strings.TrimSpace(*body.MandateID)
		if trimmed == "" {
			update.ClearMandate = true
		} else if _, err := uuid.Parse(trimmed); err != nil {
			apierr.Write(w, apierr.BadRequest("mandate_id is not a UUID"))
			return
		} else {
			update.MandateID = &trimmed
		}
	}
	current, release, found, err := h.view(r.Context(), req)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if update.Enabled && !current.Governance.CanEnable {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": refusalSentence(current.Governance.Reason), "code": current.Governance.Reason,
		})
		return
	}
	// Turning a heartbeat off, or keeping a row's owner, never takes the
	// owner from somebody else: a save by another person takes over the
	// heartbeat only when it turns it on.
	if !update.Enabled && current.Configured {
		update.OwnerUserID = current.OwnerUserID
	}
	if update.MandateID != nil {
		if h.mandates == nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
				"error": "Credential governance is off on this platform, so an agent cannot have a mandate.",
				"code":  CodeMandateRefused,
			})
			return
		}
		target := runmandate.Target{
			Origin: heartbeat.OriginAgentHeartbeat, TenantProjectID: req.projectID,
			ResourceID: strconv.FormatInt(req.applicationID, 10),
		}
		if found {
			target.ApplicationVersionID = release.VersionID
		}
		attached, err := h.mandates.CheckAttach(r.Context(), *update.MandateID, target)
		if err != nil {
			code := runmandate.Code(err)
			if code == "" {
				writeError(w, r, err)
				return
			}
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
				"error": "The mandate cannot be attached (" + code + ").", "code": CodeMandateRefused,
			})
			return
		}
		if attached.SubjectUserID == nil || *attached.SubjectUserID != update.OwnerUserID {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
				"error": "The mandate must be a delegated mandate of the heartbeat's owner.",
				"code":  CodeMandateNotOwner,
			})
			return
		}
	}
	if _, err := h.store.SaveHeartbeatSettings(r.Context(), req.projectID, req.applicationID, update); err != nil {
		writeError(w, r, err)
		return
	}
	answer, _, _, err := h.view(r.Context(), req)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, answer)
}

func refusalSentence(code string) string {
	switch code {
	case CodePlatformOff:
		return "Proactive agents are turned off on this platform."
	case CodeTierTooLow:
		return "A proactive agent must be at least medium risk. Change its risk tier and publish a reviewed release."
	case CodeNoRelease:
		return "The agent has no published release. Publish a release with a proactivity block first."
	case CodeBlockOff:
		return "The published release has no proactivity block with mode heartbeat."
	}
	return "The heartbeat cannot be turned on."
}

// History answers the agent's heartbeat history, newest first.
func (h *Handler) History(w http.ResponseWriter, r *http.Request) {
	req, ok := h.request(w, r, true)
	if !ok {
		return
	}
	limit, offset := 50, 0
	var err error
	for name, target := range map[string]*int{"limit": &limit, "offset": &offset} {
		if raw := r.URL.Query().Get(name); raw != "" {
			if *target, err = strconv.Atoi(raw); err != nil || *target < 0 {
				apierr.Write(w, apierr.BadRequest(name+" is not valid"))
				return
			}
		}
	}
	if limit < 1 || limit > 200 {
		apierr.Write(w, apierr.BadRequest("limit must be from 1 to 200"))
		return
	}
	entries, total, err := h.store.ListHeartbeats(r.Context(), req.projectID, req.applicationID, limit, offset)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"total": total, "rows": entries})
}

// GetProject answers the project switch.
func (h *Handler) GetProject(w http.ResponseWriter, r *http.Request) {
	req, ok := h.request(w, r, false)
	if !ok {
		return
	}
	enabled, err := h.store.ProjectEnabled(r.Context(), req.projectID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"proactive_agents_enabled": enabled})
}

// PutProject saves the project switch. Off stops every heartbeat of the
// project at its next wake.
func (h *Handler) PutProject(w http.ResponseWriter, r *http.Request) {
	req, ok := h.request(w, r, false)
	if !ok {
		return
	}
	var body struct {
		Enabled *bool `json:"proactive_agents_enabled"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Enabled == nil {
		apierr.Write(w, apierr.BadRequest("proactive_agents_enabled is required"))
		return
	}
	if err := h.store.SetProjectEnabled(r.Context(), req.projectID, req.userID, *body.Enabled); err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"proactive_agents_enabled": *body.Enabled})
}

func writeError(w http.ResponseWriter, r *http.Request, err error) {
	slog.ErrorContext(r.Context(), "agent heartbeat", "error", err)
	apierr.WriteStatus(w, http.StatusInternalServerError, "could not read or save the agent's heartbeat")
}

func pathID(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	value, err := strconv.ParseInt(chi.URLParam(r, name), 10, 32)
	if err != nil || value <= 0 {
		apierr.Write(w, apierr.BadRequest("the path's "+name+" is not a valid id"))
		return 0, false
	}
	return value, true
}

func decode(w http.ResponseWriter, r *http.Request, target any) bool {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil || len(raw) > maxBody {
		apierr.Write(w, apierr.BadRequest("the request body is not readable or too large"))
		return false
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(target) != nil {
		apierr.Write(w, apierr.BadRequest("the request body is not valid JSON for this route"))
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
