package configurations

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/jackc/pgx/v5"
)

const tracingConfigurationCategory = "tracing"

// tracingConfigurationsForbiddenMessage is the 403 of a tracing write the
// caller does not manage, on the compatibility and the reviewed routes.
const tracingConfigurationsForbiddenMessage = "tracing configurations are managed by project admins"

var tracingConfigurationTypeFallback = map[string]struct{}{
	"langfuse": {},
}

// TracingAccess answers whether a user manages tracing in a project: a
// project administrator, or the owner inside their own personal project.
type TracingAccess interface {
	CanManageTracing(context.Context, int64, int64) (bool, error)
}

type tracingAccessChecker = TracingAccess

type tracingAccessQueries interface {
	CanManageCurrentTracingConfiguration(
		context.Context,
		sqlcgen.CanManageCurrentTracingConfigurationParams,
	) (bool, error)
}

type postgresTracingAccessChecker struct {
	queries tracingAccessQueries
}

// NewPostgresTracingAccess is the TracingAccess the compatibility handler
// derives from its pool. cmd/elitea-main hands the same one to the reviewed
// routes, so both answer one question one way (#3032).
func NewPostgresTracingAccess(db sqlcgen.DBTX) TracingAccess {
	return postgresTracingAccessChecker{queries: sqlcgen.New(db)}
}

func (checker postgresTracingAccessChecker) CanManageTracing(
	ctx context.Context,
	projectID int64,
	userID int64,
) (bool, error) {
	if checker.queries == nil || projectID <= 0 || userID <= 0 {
		return false, errors.New("tracing access context is unavailable")
	}
	return checker.queries.CanManageCurrentTracingConfiguration(
		ctx,
		sqlcgen.CanManageCurrentTracingConfigurationParams{
			ProjectID: projectID,
			UserID:    userID,
		},
	)
}

// TracingRule is the tracing rule of the configuration row routes: a
// configuration of a tracing type (one whose registry schema names the
// tracing category, or a fallback type) is created, read by id, changed or
// deleted only by a caller who manages tracing in the project. The
// compatibility handler and the reviewed routes that shadow it decide through
// this one value, so they cannot disagree on a type or a caller (#3032).
type TracingRule struct {
	catalog *configurationapp.CurrentAvailableCatalog
	access  TracingAccess
}

var ErrInvalidTracingRule = errors.New("the configuration tracing rule needs the catalogue and the tracing access check")

// NewTracingRule is the rule over catalog's categories and access. Both are
// required: a reviewed route composed without them would admit the tracing
// writes the compatibility route refuses.
func NewTracingRule(catalog *configurationapp.CurrentAvailableCatalog, access TracingAccess) (TracingRule, error) {
	if catalog == nil || access == nil {
		return TracingRule{}, ErrInvalidTracingRule
	}
	return TracingRule{catalog: catalog, access: access}, nil
}

func (rule TracingRule) composed() bool { return rule.catalog != nil && rule.access != nil }

func (rule TracingRule) isTracingType(typeName string) bool {
	typeName = strings.TrimSpace(typeName)
	if _, fallback := tracingConfigurationTypeFallback[typeName]; fallback {
		return true
	}
	if rule.catalog == nil {
		return false
	}
	entry, ok := rule.catalog.EntryByType(typeName)
	return ok && currentConfigurationEntryHasCategory(entry, tracingConfigurationCategory)
}

// actorCanManage fails closed: no principal, no owning user, no access check
// or a failed one all answer false.
func (rule TracingRule) actorCanManage(ctx context.Context, projectID int64) bool {
	user, ok := auth.UserFromContext(ctx)
	if !ok {
		return false
	}
	userID, ok := user.OwningUserID()
	if !ok || rule.access == nil {
		return false
	}
	allowed, err := rule.access.CanManageTracing(ctx, projectID, userID)
	if err != nil {
		slog.WarnContext(ctx, "configuration tracing access check failed",
			"project_id", projectID, "user_id", userID, "err", err)
		return false
	}
	return allowed
}

// forbidden reports whether one of typeNames is a tracing type and the
// caller does not manage tracing in projectID. The access check runs only
// for a tracing type.
func (rule TracingRule) forbidden(ctx context.Context, projectID int64, typeNames ...string) bool {
	for _, typeName := range typeNames {
		if rule.isTracingType(typeName) {
			return !rule.actorCanManage(ctx, projectID)
		}
	}
	return false
}

func (h *Handler) tracingRule() TracingRule {
	return TracingRule{catalog: h.catalog, access: h.tracingAccess}
}

func (h *Handler) isTracingConfigurationType(typeName string) bool {
	return h.tracingRule().isTracingType(typeName)
}

func currentConfigurationEntryHasCategory(
	entry configurationapp.CurrentAvailableConfigurationType,
	category string,
) bool {
	var schema map[string]any
	if json.Unmarshal(entry.ConfigSchema, &schema) != nil {
		return false
	}
	properties, _ := schema["properties"].(map[string]any)
	data, _ := properties["data"].(map[string]any)
	metadata, _ := data["metadata"].(map[string]any)
	categories, _ := metadata["categories"].([]any)
	for _, raw := range categories {
		value, ok := raw.(string)
		if ok && strings.EqualFold(value, category) {
			return true
		}
	}
	return false
}

func (h *Handler) actorCanManageTracing(ctx context.Context, projectID int64) bool {
	return h.tracingRule().actorCanManage(ctx, projectID)
}

func (h *Handler) tracingWriteForbidden(ctx context.Context, projectID int64, typeNames ...string) bool {
	return h.tracingRule().forbidden(ctx, projectID, typeNames...)
}

// MarkTracingRule marks a configuration route whose handler applies the
// TracingRule, so that a walk of the router's middlewares tells it from one
// that does not: internal/api's reviewed-route shadow test fails when a
// reviewed route hides a marked compatibility route and carries no mark. It
// decides nothing; put it only on a route whose handler calls the rule.
func MarkTracingRule(next http.Handler) http.Handler { return tracingRuleMark{next} }

type tracingRuleMark struct{ http.Handler }

// IsTracingRuleMark reports whether mw is MarkTracingRule. It wraps a handler
// that serves nothing to read the mark, so call it on middleware, never per
// request.
func IsTracingRuleMark(mw func(http.Handler) http.Handler) bool {
	if mw == nil {
		return false
	}
	_, ok := mw(http.NotFoundHandler()).(tracingRuleMark)
	return ok
}

func (h *Handler) filterAvailableTracingTypes(
	ctx context.Context,
	projectID int64,
	entries []configurationapp.CurrentAvailableConfigurationType,
) []configurationapp.CurrentAvailableConfigurationType {
	if projectID <= 0 || h.actorCanManageTracing(ctx, projectID) {
		return entries
	}
	filtered := make([]configurationapp.CurrentAvailableConfigurationType, 0, len(entries))
	for _, entry := range entries {
		if !h.isTracingConfigurationType(entry.Type) {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}

func availableProjectID(raw string) int64 {
	projectID, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || projectID <= 0 {
		return 0
	}
	return projectID
}

func (h *Handler) storedConfigurationType(
	ctx context.Context,
	schema string,
	configID string,
) (string, error) {
	var typeName string
	err := h.pool.QueryRow(ctx,
		"SELECT type FROM "+schema+".configuration WHERE "+configurationIDColumn(configID)+" = $1",
		configID,
	).Scan(&typeName)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	return typeName, nil
}
