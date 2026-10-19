// Package httpactions serves the project HTTP action rules the `type: http`
// pipeline node names (api/openapi/v2.yaml, "HTTP ACTION RULES"). Origin, path
// template, method and credential live only in a rule; pipeline YAML carries
// the rule name. Rules are disabled, never deleted, through this API.
package httpactions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/httpaction"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/audit"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/egress"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

// The two permissions of this surface — decision D4, pending user
// confirmation. shared/0170 grants manage to project admins and list to
// admin, editor and viewer. Manage does not imply list: the rule-names route
// asks for list alone.
const (
	// PermissionManage gates the admin view: list, read, create and update.
	PermissionManage = "models.http_action_rules.manage"
	// PermissionList gates the rule names the pipeline editor reads.
	PermissionList = "models.http_action_rules.list"
)

// MaxRequestBodyBytes bounds one create/update body. The largest valid rule
// is about 5 KiB of JSON.
const MaxRequestBodyBytes = 16 << 10

// AuditEntityType names a rule in centry.audit_events.
const AuditEntityType = "http_action_rule"

const originRefused = "HTTP action rule origin does not resolve to a permitted destination"

// Repository is the rule store (repos.HTTPActionRulesRepo).
type Repository interface {
	List(ctx context.Context, projectID int64) ([]repos.HTTPActionRuleRecord, error)
	Get(ctx context.Context, projectID, ruleID int64) (repos.HTTPActionRuleRecord, error)
	ListNames(ctx context.Context, projectID int64) ([]repos.HTTPActionRuleName, error)
	Create(ctx context.Context, projectID, actorID int64, rule httpaction.Rule) (repos.HTTPActionRuleRecord, error)
	Update(ctx context.Context, projectID, ruleID, actorID int64, rule httpaction.Rule, expectedRevision int64) (repos.HTTPActionRuleRecord, error)
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

// Routes is mounted at /httpactions/prompt_lib/{projectID}; the project comes
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
	rule, _, actorID, ok := h.admit(w, r)
	if !ok {
		return
	}
	created, err := h.repo.Create(r.Context(), projectID, actorID, rule)
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
	rule, revision, actorID, ok := h.admit(w, r)
	if !ok {
		return
	}
	if revision == nil || *revision <= 0 {
		apierr.Write(w, apierr.BadRequest("HTTP action rule revision is required: send the revision you read"))
		return
	}
	updated, err := h.repo.Update(r.Context(), projectID, ruleID, actorID, rule, *revision)
	if err != nil {
		h.fail(w, r, "update", err)
		return
	}
	annotate(r.Context(), updated)
	writeJSON(w, http.StatusOK, updated)
}

// writeRequest is HttpActionRuleWriteRequest. Pointers mark the required
// fields whose zero value is a meaningful choice, so an omitted one is refused
// rather than read as false or 0.
type writeRequest struct {
	Name            string   `json:"name"`
	Origin          string   `json:"origin"`
	PathTemplate    string   `json:"path_template"`
	Method          string   `json:"method"`
	QueryNames      []string `json:"query_names"`
	ConfigurationID *int32   `json:"configuration_id"`
	Toolkit         string   `json:"toolkit"`
	Tool            string   `json:"tool"`
	OutputBucket    string   `json:"output_bucket"`
	Replay          string   `json:"replay"`
	Enabled         *bool    `json:"enabled"`
	Revision        *int64   `json:"revision"`
}

// admit decodes, validates and egress-checks one write body, and names the
// acting user. It writes the refusal itself.
func (h *Handler) admit(w http.ResponseWriter, r *http.Request) (httpaction.Rule, *int64, int64, bool) {
	user, _ := auth.UserFromContext(r.Context())
	actorID, ok := user.OwningUserID()
	if !ok {
		apierr.Write(w, apierr.Forbidden("an HTTP action rule write needs a user principal"))
		return httpaction.Rule{}, nil, 0, false
	}
	if h.guard == nil {
		apierr.Write(w, apierr.BadRequest("this deployment cannot validate HTTP action rule origins; contact an administrator"))
		return httpaction.Rule{}, nil, 0, false
	}
	body, err := decode(w, r)
	if err != nil {
		apierr.Write(w, err)
		return httpaction.Rule{}, nil, 0, false
	}
	for _, required := range []struct {
		field   string
		missing bool
	}{
		{"query_names", body.QueryNames == nil}, {"configuration_id", body.ConfigurationID == nil}, {"enabled", body.Enabled == nil},
	} {
		if required.missing {
			apierr.Write(w, apierr.BadRequest("HTTP action rule "+required.field+" is required"))
			return httpaction.Rule{}, nil, 0, false
		}
	}
	rule := httpaction.Rule{
		Name: body.Name, Origin: body.Origin, PathTemplate: body.PathTemplate, Method: body.Method,
		QueryNames: body.QueryNames, ConfigurationID: *body.ConfigurationID, Toolkit: body.Toolkit, Tool: body.Tool,
		OutputBucket: body.OutputBucket, Replay: body.Replay, Enabled: *body.Enabled,
	}
	rule.Normalize()
	if err := rule.Validate(); err != nil {
		apierr.Write(w, ruleRefusal(err))
		return httpaction.Rule{}, nil, 0, false
	}
	if err := h.guard.Validate(r.Context(), rule.Origin); err != nil {
		// The guard's own message names the host; the answer never does.
		if errors.Is(err, egress.ErrDestinationRefused) {
			apierr.Write(w, apierr.BadRequest(originRefused))
		} else {
			h.fail(w, r, "check origin", err)
		}
		return httpaction.Rule{}, nil, 0, false
	}
	return rule, body.Revision, actorID, true
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
		// The decoder's message repeats the submitted key; this one does not.
		return writeRequest{}, apierr.BadRequest("request body has a field an HTTP action rule does not define")
	default:
		return writeRequest{}, apierr.BadRequest("request body is not a valid HTTP action rule")
	}
}

func ruleRefusal(err error) error {
	if ruleErr, ok := httpaction.IsRuleError(err); ok {
		return apierr.BadRequest(ruleErr.Error())
	}
	return apierr.BadRequest("HTTP action rule is invalid")
}

func (h *Handler) project(w http.ResponseWriter, r *http.Request) (int64, bool) {
	if h.repo == nil {
		apierr.WriteStatus(w, http.StatusServiceUnavailable, "HTTP action rules are not available on this deployment")
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
		apierr.Write(w, apierr.NotFound(repos.ErrHTTPActionRuleNotFound.Error()))
	}
	return ruleID, ok
}

func positiveID(value string) (int64, bool) {
	id, err := strconv.ParseInt(value, 10, 64)
	return id, err == nil && id > 0 && strconv.FormatInt(id, 10) == value
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, operation string, err error) {
	switch {
	case errors.Is(err, repos.ErrHTTPActionRuleNotFound):
		apierr.Write(w, apierr.NotFound(repos.ErrHTTPActionRuleNotFound.Error()))
	case errors.Is(err, repos.ErrHTTPActionRuleNameTaken):
		apierr.Write(w, apierr.Conflict(repos.ErrHTTPActionRuleNameTaken.Error()))
	case errors.Is(err, repos.ErrHTTPActionRuleStale):
		apierr.Write(w, apierr.Conflict(repos.ErrHTTPActionRuleStale.Error()))
	case errors.Is(err, repos.ErrHTTPActionRuleLimit):
		apierr.Write(w, apierr.Conflict(repos.ErrHTTPActionRuleLimit.Error()))
	default:
		if r.Context().Err() == nil {
			slog.ErrorContext(r.Context(), "http action rules: request failed", "operation", operation, "error", err)
		}
		apierr.Write(w, apierr.Internal("internal server error"))
	}
}

func annotate(ctx context.Context, record repos.HTTPActionRuleRecord) {
	id, projectID := record.ID, record.ProjectID
	audit.Annotate(ctx, audit.Annotation{EntityType: AuditEntityType, EntityID: &id, EntityName: record.Name, ProjectID: &projectID})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
