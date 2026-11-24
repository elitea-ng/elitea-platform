// Package dbactions serves the project DB action rules the `type: db`
// pipeline node names (api/openapi/v2.yaml, "DB ACTION RULES"). SQL
// configuration reference, dialect, database name, mode, and write marker
// table live only in a rule; host/port/username/password live only in the
// referenced `sql` configuration, never in pipeline YAML or rule rows. Rules
// are disabled, never deleted, through this API.
package dbactions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	apimw "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/dbaction"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/audit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/egress"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// The two permissions of this surface. shared/0271 grants manage to project
// admins and list to admin, editor and viewer. Manage does not imply list:
// the rule-names route asks for list alone.
const (
	// PermissionManage gates the admin view: list, read, create and update.
	PermissionManage = "models.db_action_rules.manage"
	// PermissionList gates the redacted rule names the pipeline editor reads.
	PermissionList = "models.db_action_rules.list"
	// permissionConfigurationDetails is required in addition to PermissionManage
	// when a rule write binds a non-zero configuration_id (migration 0072).
	permissionConfigurationDetails = "configurations.configuration.details"
)

// MaxRequestBodyBytes bounds one create/update body.
const MaxRequestBodyBytes = 16 << 10

// AuditEntityType names a DB action rule in centry.audit_events.
const AuditEntityType = "db_action_rule"

const destinationRefused = "DB action rule destination does not resolve to a permitted destination"

// Repository is the rule store (repos.DBActionRulesRepo).
type Repository interface {
	List(ctx context.Context, projectID int64) ([]repos.DBActionRuleRecord, error)
	Get(ctx context.Context, projectID, ruleID int64) (repos.DBActionRuleRecord, error)
	ListNames(ctx context.Context, projectID int64) ([]repos.DBActionRuleName, error)
	LookupConfigurationTarget(ctx context.Context, projectID, actorID int64, configurationID int32, dialect string) (repos.DBActionConfigurationTarget, error)
	Create(ctx context.Context, projectID, actorID int64, rule dbaction.Rule, expectedTarget repos.DBActionConfigurationTarget) (repos.DBActionRuleRecord, error)
	Update(ctx context.Context, projectID, ruleID, actorID int64, rule dbaction.Rule, expectedRevision int64, expectedTarget repos.DBActionConfigurationTarget) (repos.DBActionRuleRecord, error)
}

// Handler serves the five routes. A nil resolver refuses every route (403), a
// nil guard refuses every write (400), a nil repository answers 503.
type Handler struct {
	repo     Repository
	guard    *egress.Guard
	resolver auth.PermissionResolver
}

func NewHandler(repo Repository, guard *egress.Guard, resolver auth.PermissionResolver) *Handler {
	return &Handler{repo: repo, guard: guard, resolver: resolver}
}

// Routes is mounted at /dbactions/prompt_lib/{projectID}; the project comes
// only from that segment and every route is gated on it.
func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	r.With(h.require(PermissionManage)).Get("/rules", h.list)
	r.With(h.require(PermissionManage)).Post("/rules", h.create)
	r.With(h.require(PermissionManage)).Get("/rules/{ruleID}", h.get)
	r.With(h.require(PermissionManage)).Put("/rules/{ruleID}", h.update)
	r.With(h.require(PermissionList)).Get("/rule-names", h.names)
	return r
}

func (h *Handler) require(permission string) func(http.Handler) http.Handler {
	return apimw.RequireResolvedPermissions(h.resolver, auth.PermissionModeDefault, permission)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	projectID, ok := h.project(w, r)
	if !ok {
		return
	}
	items, err := h.repo.List(r.Context(), projectID)
	if err != nil {
		h.fail(w, r, "list", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *Handler) names(w http.ResponseWriter, r *http.Request) {
	projectID, ok := h.project(w, r)
	if !ok {
		return
	}
	items, err := h.repo.ListNames(r.Context(), projectID)
	if err != nil {
		h.fail(w, r, "list names", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	projectID, ok := h.project(w, r)
	if !ok {
		return
	}
	ruleID, ok := ruleIDFrom(w, r)
	if !ok {
		return
	}
	record, err := h.repo.Get(r.Context(), projectID, ruleID)
	if err != nil {
		h.fail(w, r, "get", err)
		return
	}
	writeJSON(w, http.StatusOK, record)
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	projectID, ok := h.project(w, r)
	if !ok {
		return
	}
	rule, _, actorID, target, ok := h.admit(w, r, projectID, 0)
	if !ok {
		return
	}
	created, err := h.repo.Create(r.Context(), projectID, actorID, rule, target)
	if err != nil {
		h.fail(w, r, "create", err)
		return
	}
	annotate(r.Context(), created)
	writeJSON(w, http.StatusCreated, created)
}

func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	projectID, ok := h.project(w, r)
	if !ok {
		return
	}
	ruleID, ok := ruleIDFrom(w, r)
	if !ok {
		return
	}
	rule, revision, actorID, target, ok := h.admit(w, r, projectID, ruleID)
	if !ok {
		return
	}
	updated, err := h.repo.Update(r.Context(), projectID, ruleID, actorID, rule, *revision, target)
	if err != nil {
		h.fail(w, r, "update", err)
		return
	}
	annotate(r.Context(), updated)
	writeJSON(w, http.StatusOK, updated)
}

// writeRequest is DbActionRuleWriteRequest. Pointers mark required fields whose
// zero value is meaningful (and marker_table, where omitted defaults on write
// while explicit empty string is refused).
type writeRequest struct {
	Name            string  `json:"name"`
	ConfigurationID *int32  `json:"configuration_id"`
	Dialect         string  `json:"dialect"`
	DatabaseName    string  `json:"database_name"`
	Mode            string  `json:"mode"`
	MarkerTable     *string `json:"marker_table"`
	Enabled         *bool   `json:"enabled"`
	Revision        *int64  `json:"revision"`
}

// admit decodes, validates, checks configuration access, verifies project-scoped
// rule existence for updates, and egress-checks the referenced SQL configuration's
// host/port. Returns the validated target so Create/Update can verify inside the
// FOR SHARE lock that no drift occurred.
func (h *Handler) admit(w http.ResponseWriter, r *http.Request, projectID, updateRuleID int64) (dbaction.Rule, *int64, int64, repos.DBActionConfigurationTarget, bool) {
	var zeroTarget repos.DBActionConfigurationTarget
	user, _ := auth.UserFromContext(r.Context())
	actorID, ok := user.OwningUserID()
	if !ok {
		apierr.Write(w, apierr.Forbidden("a DB action rule write needs a user principal"))
		return dbaction.Rule{}, nil, 0, zeroTarget, false
	}
	if h.guard == nil {
		apierr.Write(w, apierr.BadRequest("this deployment cannot validate DB action rule destinations; contact an administrator"))
		return dbaction.Rule{}, nil, 0, zeroTarget, false
	}
	body, err := decode(w, r)
	if err != nil {
		apierr.Write(w, err)
		return dbaction.Rule{}, nil, 0, zeroTarget, false
	}
	for _, required := range []struct {
		field   string
		missing bool
	}{
		{"configuration_id", body.ConfigurationID == nil},
		{"enabled", body.Enabled == nil},
	} {
		if required.missing {
			apierr.Write(w, apierr.BadRequest("DB action rule "+required.field+" is required"))
			return dbaction.Rule{}, nil, 0, zeroTarget, false
		}
	}
	markerTable := ""
	explicitEmptyMarker := false
	if body.MarkerTable != nil {
		markerTable = *body.MarkerTable
		explicitEmptyMarker = markerTable == ""
	}
	rule := dbaction.Rule{
		Name:            body.Name,
		ConfigurationID: *body.ConfigurationID,
		Dialect:         body.Dialect,
		DatabaseName:    body.DatabaseName,
		Mode:            body.Mode,
		MarkerTable:     markerTable,
		Enabled:         *body.Enabled,
	}
	if rule.Mode == dbaction.ModeWrite && explicitEmptyMarker {
		apierr.Write(w, apierr.BadRequest("DB action rule marker_table is required when mode is write"))
		return dbaction.Rule{}, nil, 0, zeroTarget, false
	}
	rule.Normalize()
	if err := rule.Validate(); err != nil {
		apierr.Write(w, ruleRefusal(err))
		return dbaction.Rule{}, nil, 0, zeroTarget, false
	}
	if updateRuleID > 0 && (body.Revision == nil || *body.Revision <= 0) {
		apierr.Write(w, apierr.BadRequest("DB action rule revision is required: send the revision you read"))
		return dbaction.Rule{}, nil, 0, zeroTarget, false
	}
	if rule.ConfigurationID > 0 {
		if h.resolver == nil {
			apierr.Write(w, apierr.Forbidden("insufficient permissions"))
			return dbaction.Rule{}, nil, 0, zeroTarget, false
		}
		resolution, err := h.resolver.ResolvePermissions(r.Context(), user, auth.PermissionModeDefault, strconv.FormatInt(projectID, 10))
		if err != nil || !slices.Contains(resolution.Permissions, permissionConfigurationDetails) {
			apierr.Write(w, apierr.Forbidden("a DB action rule referencing a configuration requires configuration access"))
			return dbaction.Rule{}, nil, 0, zeroTarget, false
		}
	}
	if updateRuleID > 0 {
		// Verify rule existence in this project before touching configuration
		// lookup or egress DNS so cross-project and nonexistent rule updates
		// answer the same 404 without probing external destinations.
		if _, err := h.repo.Get(r.Context(), projectID, updateRuleID); err != nil {
			h.fail(w, r, "update", err)
			return dbaction.Rule{}, nil, 0, zeroTarget, false
		}
	}
	target, err := h.repo.LookupConfigurationTarget(r.Context(), projectID, actorID, rule.ConfigurationID, rule.Dialect)
	if err != nil {
		h.fail(w, r, "lookup configuration", err)
		return dbaction.Rule{}, nil, 0, zeroTarget, false
	}
	if err := h.guard.ValidateHostPort(r.Context(), target.Host, target.Port); err != nil {
		// Never echo the resolved host or IP: prevents turning rule writes into
		// a DNS/host oracle for private networks.
		if errors.Is(err, egress.ErrDestinationRefused) {
			apierr.Write(w, apierr.BadRequest(destinationRefused))
		} else {
			h.fail(w, r, "check destination", err)
		}
		return dbaction.Rule{}, nil, 0, zeroTarget, false
	}
	return rule, body.Revision, actorID, target, true
}

func decode(w http.ResponseWriter, r *http.Request) (writeRequest, error) {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, MaxRequestBodyBytes))
	decoder.DisallowUnknownFields()
	var body writeRequest
	err := decoder.Decode(&body)
	if err == nil && decoder.Decode(&struct{}{}) != io.EOF {
		return writeRequest{}, apierr.BadRequest("request body must hold one JSON object")
	}
	var tooLarge *http.MaxBytesError
	switch {
	case err == nil:
		return body, nil
	case errors.As(err, &tooLarge):
		return writeRequest{}, apierr.BadRequest(fmt.Sprintf("request body exceeds %d bytes", MaxRequestBodyBytes))
	case strings.HasPrefix(err.Error(), "json: unknown field "):
		return writeRequest{}, apierr.BadRequest("request body has a field a DB action rule does not define")
	default:
		return writeRequest{}, apierr.BadRequest("request body is not a valid DB action rule")
	}
}

func ruleRefusal(err error) error {
	if ruleErr, ok := dbaction.IsRuleError(err); ok {
		return apierr.BadRequest(ruleErr.Error())
	}
	return apierr.BadRequest("DB action rule is invalid")
}

func (h *Handler) project(w http.ResponseWriter, r *http.Request) (int64, bool) {
	if h.repo == nil {
		apierr.WriteStatus(w, http.StatusServiceUnavailable, "DB action rules are not available on this deployment")
		return 0, false
	}
	projectID, ok := positiveID(chi.URLParam(r, "projectID"))
	if !ok {
		apierr.Write(w, apierr.Forbidden("insufficient permissions"))
	}
	return projectID, ok
}

// ruleIDFrom answers a malformed id exactly as a missing rule.
func ruleIDFrom(w http.ResponseWriter, r *http.Request) (int64, bool) {
	ruleID, ok := positiveID(chi.URLParam(r, "ruleID"))
	if !ok {
		apierr.Write(w, apierr.NotFound(repos.ErrDBActionRuleNotFound.Error()))
	}
	return ruleID, ok
}

func positiveID(value string) (int64, bool) {
	id, err := strconv.ParseInt(value, 10, 64)
	return id, err == nil && id > 0 && strconv.FormatInt(id, 10) == value
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, operation string, err error) {
	switch {
	case errors.Is(err, repos.ErrDBActionRuleNotFound):
		apierr.Write(w, apierr.NotFound(repos.ErrDBActionRuleNotFound.Error()))
	case errors.Is(err, repos.ErrDBActionRuleNameTaken):
		apierr.Write(w, apierr.Conflict(repos.ErrDBActionRuleNameTaken.Error()))
	case errors.Is(err, repos.ErrDBActionRuleStale):
		apierr.Write(w, apierr.Conflict(repos.ErrDBActionRuleStale.Error()))
	case errors.Is(err, repos.ErrDBActionRuleLimit):
		apierr.Write(w, apierr.Conflict(repos.ErrDBActionRuleLimit.Error()))
	case errors.Is(err, repos.ErrDBActionRuleConfigurationInvalid):
		apierr.Write(w, apierr.BadRequest(repos.ErrDBActionRuleConfigurationInvalid.Error()))
	default:
		if r.Context().Err() == nil {
			slog.ErrorContext(r.Context(), "db action rules: request failed", "operation", operation, "error", err)
		}
		apierr.Write(w, apierr.Internal("internal server error"))
	}
}

func annotate(ctx context.Context, record repos.DBActionRuleRecord) {
	id, projectID := record.ID, record.ProjectID
	audit.Annotate(ctx, audit.Annotation{EntityType: AuditEntityType, EntityID: &id, EntityName: record.Name, ProjectID: &projectID})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
